package service

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
	"github.com/google/uuid"
)

const (
	userRequestAuditExportQueueSize = 8
	userRequestAuditExportTTL       = 24 * time.Hour
	userRequestAuditExportTimeout   = 30 * time.Minute
)

var (
	ErrUserRequestAuditExportQueueFull   = infraerrors.New(http.StatusTooManyRequests, "USER_REQUEST_AUDIT_EXPORT_QUEUE_FULL", "export queue is full")
	ErrUserRequestAuditExportNotFound    = infraerrors.New(http.StatusNotFound, "USER_REQUEST_AUDIT_EXPORT_NOT_FOUND", "export not found")
	ErrUserRequestAuditExportNotReady    = infraerrors.New(http.StatusConflict, "USER_REQUEST_AUDIT_EXPORT_NOT_READY", "export is not ready")
	ErrUserRequestAuditExportUnavailable = infraerrors.New(http.StatusServiceUnavailable, "USER_REQUEST_AUDIT_EXPORT_UNAVAILABLE", "export storage unavailable")
)

type UserRequestAuditExportJob struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	Format      string    `json:"format"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	Rows        int       `json:"rows"`
	SizeBytes   int64     `json:"size_bytes"`
	Error       string    `json:"error,omitempty"`
	Filename    string    `json:"filename,omitempty"`
}

type auditExportTask struct {
	jobID           string
	generation      uint64
	filter          UserRequestAuditFilter
	format          string
	conversationIDs []int64
}

type userRequestAuditConversationLister interface {
	ListConversations(context.Context, UserRequestAuditFilter) ([]*UserRequestAudit, int64, error)
}
type userRequestAuditConversationExporter interface {
	ExportConversations(context.Context, UserRequestAuditFilter, []int64, io.Writer, string) (int, error)
}
type userRequestAuditClearer interface {
	Clear(context.Context) (int64, error)
}
type userRequestAuditRootProvider interface {
	UserRequestAuditRoot() string
}

func (s *UserRequestAuditService) currentGeneration() uint64 {
	if s == nil {
		return 0
	}
	s.generationMu.RLock()
	defer s.generationMu.RUnlock()
	return s.generation
}

func (s *UserRequestAuditService) runAuditWrite(ctx context.Context, generation uint64, fn func(context.Context) error) error {
	if s == nil || fn == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.currentGeneration() != generation {
		return nil
	}
	return fn(ctx)
}

func (s *UserRequestAuditService) exportRoot() (string, error) {
	if s == nil || s.repo == nil {
		return "", ErrUserRequestAuditExportUnavailable
	}
	r, ok := s.repo.(userRequestAuditRootProvider)
	if !ok {
		return "", ErrUserRequestAuditExportUnavailable
	}
	root := filepath.Clean(strings.TrimSpace(r.UserRequestAuditRoot()))
	if root == "" || root == "." {
		return "", ErrUserRequestAuditExportUnavailable
	}
	exports := filepath.Join(root, "exports")
	if rel, err := filepath.Rel(root, exports); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrUserRequestAuditExportUnavailable
	}
	if err := os.MkdirAll(exports, 0o750); err != nil {
		return "", ErrUserRequestAuditExportUnavailable
	}
	return exports, nil
}

const userRequestAuditMinimumFreeBytes int64 = 2 * 1024 * 1024 * 1024

func (s *UserRequestAuditService) exportDiskAllowed() bool {
	status := s.StorageStatus()
	return status.DiskTotalBytes > 0 && status.DiskFreeBytes >= userRequestAuditMinimumFreeBytes
}

// guardedAuditExportWriter stops a long export when the disk fills up. The
// check is time-boxed: StorageStatus globs and stats the whole archive, so
// testing it on every 32 KiB write would dominate export cost.
type guardedAuditExportWriter struct {
	service  *UserRequestAuditService
	writer   io.Writer
	lastGate time.Time
}

func (w *guardedAuditExportWriter) Write(p []byte) (int, error) {
	if w.service == nil {
		return 0, ErrUserRequestAuditExportUnavailable
	}
	if time.Since(w.lastGate) >= time.Minute {
		w.lastGate = time.Now()
		if !w.service.exportDiskAllowed() {
			return 0, ErrUserRequestAuditExportUnavailable
		}
	}
	return w.writer.Write(p)
}

func validExportID(id string) bool {
	if len(id) < 1 || len(id) > 64 || filepath.Base(id) != id {
		return false
	}
	for _, r := range id {
		if r != '-' && r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func cloneExportJob(job *UserRequestAuditExportJob) UserRequestAuditExportJob {
	if job == nil {
		return UserRequestAuditExportJob{}
	}
	return *job
}

func (s *UserRequestAuditService) CreateExport(ctx context.Context, filter UserRequestAuditFilter, format string, conversationIDs []int64) (UserRequestAuditExportJob, error) {
	if s == nil || s.repo == nil {
		return UserRequestAuditExportJob{}, ErrUserRequestAuditExportUnavailable
	}
	if format != "json" && format != "jsonl" {
		return UserRequestAuditExportJob{}, infraerrors.New(http.StatusBadRequest, "INVALID_EXPORT_FORMAT", "format must be json or jsonl")
	}
	if !s.exportDiskAllowed() {
		return UserRequestAuditExportJob{}, ErrUserRequestAuditExportUnavailable
	}
	if len(conversationIDs) > 10000 {
		return UserRequestAuditExportJob{}, infraerrors.New(http.StatusBadRequest, "INVALID_EXPORT_SELECTION", "too many conversations selected")
	}
	if len(conversationIDs) > 0 {
		ids := make([]int64, 0, len(conversationIDs))
		seen := make(map[int64]struct{}, len(conversationIDs))
		for _, id := range conversationIDs {
			if id <= 0 {
				return UserRequestAuditExportJob{}, infraerrors.New(http.StatusBadRequest, "INVALID_EXPORT_SELECTION", "conversation IDs must be positive")
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		conversationIDs = ids
	}
	id := uuid.NewString()
	filter.Page, filter.PageSize = 0, 0
	now := time.Now().UTC()
	job := &UserRequestAuditExportJob{ID: id, Status: "queued", Format: format, CreatedAt: now, ExpiresAt: now.Add(userRequestAuditExportTTL)}
	task := auditExportTask{jobID: id, generation: s.currentGeneration(), filter: filter, format: format, conversationIDs: conversationIDs}
	s.exportJobsMu.Lock()
	s.exportJobs[id] = job
	var accepted UserRequestAuditExportJob
	select {
	case s.exportQueue <- task:
		accepted = cloneExportJob(job)
		s.exportJobsMu.Unlock()
	case <-ctx.Done():
		delete(s.exportJobs, id)
		s.exportJobsMu.Unlock()
		return UserRequestAuditExportJob{}, ctx.Err()
	default:
		delete(s.exportJobs, id)
		s.exportJobsMu.Unlock()
		return UserRequestAuditExportJob{}, ErrUserRequestAuditExportQueueFull
	}
	return accepted, nil
}

func (s *UserRequestAuditService) ExportJobs() []UserRequestAuditExportJob {
	if s == nil {
		return []UserRequestAuditExportJob{}
	}
	s.cleanupExportArtifacts()
	s.exportJobsMu.RLock()
	out := make([]UserRequestAuditExportJob, 0, len(s.exportJobs))
	for _, job := range s.exportJobs {
		out = append(out, cloneExportJob(job))
	}
	s.exportJobsMu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *UserRequestAuditService) setExportJob(id string, fn func(*UserRequestAuditExportJob)) bool {
	s.exportJobsMu.Lock()
	defer s.exportJobsMu.Unlock()
	job := s.exportJobs[id]
	if job == nil {
		return false
	}
	fn(job)
	return true
}

func (s *UserRequestAuditService) runExportWorker() {
	defer close(s.exportDone)
	ctx, cancel := context.WithCancel(context.Background())
	s.exportCancelMu.Lock()
	s.exportCancel = cancel
	s.exportCancelMu.Unlock()
	defer func() { s.exportCancelMu.Lock(); s.exportCancel = nil; s.exportCancelMu.Unlock(); cancel() }()
	go func() { <-s.exportStop; cancel() }()
	for {
		select {
		case task := <-s.exportQueue:
			s.runOneExport(ctx, task)
		case <-s.exportStop:
			return
		}
	}
}

func (s *UserRequestAuditService) runOneExport(workerCtx context.Context, task auditExportTask) {
	if s.currentGeneration() != task.generation {
		return
	}
	if !s.setExportJob(task.jobID, func(job *UserRequestAuditExportJob) { job.Status = "running" }) {
		return
	}
	s.exportMu.Lock()
	defer s.exportMu.Unlock()
	if s.currentGeneration() != task.generation {
		return
	}
	root, err := s.exportRoot()
	if err != nil || !s.exportDiskAllowed() {
		s.failExport(task.jobID, ErrUserRequestAuditExportUnavailable)
		return
	}
	filename := "user-request-audit-" + task.jobID + "." + task.format + ".gz"
	tmp := filepath.Join(root, ".tmp-"+task.jobID)
	final := filepath.Join(root, filename)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		s.failExport(task.jobID, err)
		return
	}
	exporter, ok := s.repo.(userRequestAuditConversationExporter)
	if !ok {
		s.failExport(task.jobID, ErrUserRequestAuditExportUnavailable)
		return
	}
	gz := gzip.NewWriter(f)
	guarded := &guardedAuditExportWriter{service: s, writer: gz, lastGate: time.Now()}
	ctx, cancel := context.WithTimeout(workerCtx, userRequestAuditExportTimeout)
	defer cancel()
	s.setExportJob(task.jobID, func(job *UserRequestAuditExportJob) { job.Filename = filename })
	rows, err := exporter.ExportConversations(ctx, task.filter, task.conversationIDs, guarded, task.format)
	closeErr := gz.Close()
	fileErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = fileErr
	}
	if err == nil && !s.exportDiskAllowed() {
		err = ErrUserRequestAuditExportUnavailable
	}
	if err != nil {
		_ = os.Remove(tmp)
		s.failExport(task.jobID, err)
		return
	}
	if s.currentGeneration() != task.generation {
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		s.failExport(task.jobID, err)
		return
	}
	info, err := os.Stat(final)
	if err != nil {
		s.failExport(task.jobID, err)
		return
	}
	s.setExportJob(task.jobID, func(job *UserRequestAuditExportJob) {
		job.Status = "completed"
		job.CompletedAt = time.Now().UTC()
		job.Rows = rows
		job.SizeBytes = info.Size()
		job.Filename = filename
	})
}

func (s *UserRequestAuditService) failExport(id string, err error) {
	message := "export failed"
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		message = "export canceled"
	}
	s.setExportJob(id, func(job *UserRequestAuditExportJob) {
		job.Status = "failed"
		job.Error = message
		job.CompletedAt = time.Now().UTC()
	})
	slog.Warn("user_request_audit.export_failed", "error", message)
}

func (s *UserRequestAuditService) OpenExport(jobID string) (*os.File, string, error) {
	if !validExportID(jobID) {
		return nil, "", ErrUserRequestAuditExportNotFound
	}
	s.exportJobsMu.RLock()
	job := s.exportJobs[jobID]
	snapshot := cloneExportJob(job)
	s.exportJobsMu.RUnlock()
	if job == nil {
		return nil, "", ErrUserRequestAuditExportNotFound
	}
	if snapshot.Status != "completed" {
		return nil, "", ErrUserRequestAuditExportNotReady
	}
	if !snapshot.ExpiresAt.IsZero() && time.Now().UTC().After(snapshot.ExpiresAt) {
		s.removeExportJob(jobID)
		return nil, "", ErrUserRequestAuditExportNotFound
	}
	root, err := s.exportRoot()
	if err != nil || snapshot.Filename == "" || filepath.Base(snapshot.Filename) != snapshot.Filename {
		return nil, "", ErrUserRequestAuditExportNotFound
	}
	path := filepath.Join(root, snapshot.Filename)
	if rel, err := filepath.Rel(root, path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", ErrUserRequestAuditExportNotFound
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, "", ErrUserRequestAuditExportNotFound
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", ErrUserRequestAuditExportNotFound
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, "", ErrUserRequestAuditExportNotFound
	}
	return f, snapshot.Filename, nil
}

func (s *UserRequestAuditService) removeExportJob(id string) {
	s.exportJobsMu.Lock()
	job := s.exportJobs[id]
	var filename string
	if job != nil {
		filename = job.Filename
	}
	delete(s.exportJobs, id)
	s.exportJobsMu.Unlock()
	if filename != "" {
		if root, err := s.exportRoot(); err == nil && filepath.Base(filename) == filename {
			_ = os.Remove(filepath.Join(root, filename))
		}
	}
}

func (s *UserRequestAuditService) cleanupOrphanExports() {
	root, err := s.exportRoot()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") || strings.HasSuffix(entry.Name(), ".gz") {
			_ = os.Remove(filepath.Join(root, entry.Name()))
		}
	}
}

func (s *UserRequestAuditService) cleanupExportArtifacts() {
	now := time.Now().UTC()
	s.exportJobsMu.RLock()
	var expired []string
	for id, job := range s.exportJobs {
		if !job.ExpiresAt.IsZero() && now.After(job.ExpiresAt) {
			expired = append(expired, id)
		}
	}
	s.exportJobsMu.RUnlock()
	for _, id := range expired {
		s.removeExportJob(id)
	}
}

func (s *UserRequestAuditService) ClearAudits(ctx context.Context) (int64, error) {
	if s == nil || s.repo == nil {
		return 0, nil
	}
	clearer, ok := s.repo.(userRequestAuditClearer)
	if !ok {
		return 0, ErrUserRequestAuditExportUnavailable
	}
	s.generationMu.Lock()
	s.generation++
	s.generationMu.Unlock()
	s.exportCancelMu.Lock()
	cancel := s.exportCancel
	s.exportCancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.exportMu.Lock()
	defer s.exportMu.Unlock()
	s.writeMu.Lock()
	deleted, err := clearer.Clear(ctx)
	s.writeMu.Unlock()
	if err != nil {
		return deleted, err
	}
	s.exportJobsMu.Lock()
	s.exportJobs = make(map[string]*UserRequestAuditExportJob)
	s.exportJobsMu.Unlock()
	root, rootErr := s.exportRoot()
	if rootErr != nil {
		return deleted, ErrUserRequestAuditExportUnavailable
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		return deleted, ErrUserRequestAuditExportUnavailable
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".gz") || strings.HasPrefix(entry.Name(), ".tmp-") {
			if removeErr := os.Remove(filepath.Join(root, entry.Name())); removeErr != nil && !os.IsNotExist(removeErr) {
				return deleted, ErrUserRequestAuditExportUnavailable
			}
		}
	}
	return deleted, nil
}
