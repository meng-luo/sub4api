package service

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type exportTestRepo struct {
	root    string
	mu      sync.Mutex
	created int
	cleared bool
}

func (r *exportTestRepo) Create(context.Context, *UserRequestAudit) error {
	r.mu.Lock()
	r.created++
	r.mu.Unlock()
	return nil
}
func (r *exportTestRepo) Complete(context.Context, *UserRequestAuditCompletion) error { return nil }
func (r *exportTestRepo) List(context.Context, UserRequestAuditFilter) ([]*UserRequestAudit, int64, error) {
	return nil, 0, nil
}
func (r *exportTestRepo) GetByID(context.Context, int64) (*UserRequestAudit, error) {
	return nil, os.ErrNotExist
}
func (r *exportTestRepo) DeleteExpired(context.Context, time.Time, int) (int64, error) { return 0, nil }
func (r *exportTestRepo) UserRequestAuditRoot() string                                 { return r.root }

func (r *exportTestRepo) StorageStatus() UserRequestAuditStorageStatus {
	return UserRequestAuditStorageStatus{DiskTotalBytes: 10 * userRequestAuditMinimumFreeBytes, DiskFreeBytes: 10 * userRequestAuditMinimumFreeBytes}
}
func (r *exportTestRepo) ExportConversations(_ context.Context, _ UserRequestAuditFilter, _ []int64, w io.Writer, format string) (int, error) {
	row := &UserRequestAudit{ID: 1, Protocol: "openai_responses", RequestChatML: "request", ResponseChatML: "response"}
	if format == "json" {
		b, _ := json.Marshal([]*UserRequestAudit{row})
		_, _ = w.Write(b)
	} else {
		b, _ := json.Marshal(row)
		_, _ = w.Write(append(b, '\n'))
	}
	return 1, nil
}
func (r *exportTestRepo) Clear(context.Context) (int64, error) {
	r.mu.Lock()
	r.cleared = true
	r.mu.Unlock()
	return int64(r.created), nil
}

func waitExportTestJob(t *testing.T, s *UserRequestAuditService, id string) UserRequestAuditExportJob {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, job := range s.ExportJobs() {
			if job.ID == id && (job.Status == "completed" || job.Status == "failed") {
				return job
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("export job %s did not finish", id)
	return UserRequestAuditExportJob{}
}

func TestUserRequestAuditExportGzipArtifact(t *testing.T) {
	repo := &exportTestRepo{root: t.TempDir()}
	s := NewUserRequestAuditService(repo, nil)
	s.Start()
	job, err := s.CreateExport(context.Background(), UserRequestAuditFilter{}, "jsonl", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	got := waitExportTestJob(t, s, job.ID)
	if got.Status != "completed" || got.Rows != 1 || got.SizeBytes <= 0 || got.Filename == "" {
		t.Fatalf("unexpected job: %+v", got)
	}
	f, _, err := s.OpenExport(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	_ = gz.Close()
	if len(body) == 0 || body[0] != '{' {
		t.Fatalf("unexpected gzip body: %q", body)
	}
}

func TestUserRequestAuditClearInvalidatesQueuedWrites(t *testing.T) {
	repo := &exportTestRepo{root: t.TempDir()}
	s := NewUserRequestAuditService(repo, nil)
	generation := s.currentGeneration()
	s.queue <- func(ctx context.Context) {
		_ = s.runAuditWrite(ctx, generation, func(context.Context) error { return repo.Create(context.Background(), &UserRequestAudit{}) })
	}
	if _, err := s.ClearAudits(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Start()
	s.Stop()
	repo.mu.Lock()
	created, cleared := repo.created, repo.cleared
	repo.mu.Unlock()
	if !cleared || created != 0 {
		t.Fatalf("queued write resurrected after clear: created=%d cleared=%v", created, cleared)
	}
}

func TestUserRequestAuditExpiredExportRemoved(t *testing.T) {
	repo := &exportTestRepo{root: t.TempDir()}
	s := NewUserRequestAuditService(repo, nil)
	s.Start()
	job, err := s.CreateExport(context.Background(), UserRequestAuditFilter{}, "jsonl", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	got := waitExportTestJob(t, s, job.ID)
	if got.Status != "completed" {
		t.Fatalf("unexpected job: %+v", got)
	}
	s.exportJobsMu.Lock()
	s.exportJobs[job.ID].ExpiresAt = time.Now().UTC().Add(-time.Second)
	s.exportJobsMu.Unlock()
	_ = s.ExportJobs()
	if _, _, err := s.OpenExport(job.ID); err == nil {
		t.Fatal("expired export remained downloadable")
	}
	if _, err := os.Stat(filepath.Join(repo.root, "exports", got.Filename)); !os.IsNotExist(err) {
		t.Fatalf("expired artifact remains: %v", err)
	}
}
