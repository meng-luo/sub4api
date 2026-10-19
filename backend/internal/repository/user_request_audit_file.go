package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/google/uuid"
	"github.com/shirou/gopsutil/v4/disk"
)

const userRequestAuditMinFreeBytes int64 = 2 * 1024 * 1024 * 1024
const auditColdPayloadLimit = 16 * 1024 * 1024

// SQL owns identity, membership, metadata and the complete set of payload paths.
// Immutable JSON files are cold evidence, decoded only by explicit open/export.
// No constructor, capture, cleanup or query discovers records from the filesystem.
type FileUserRequestAuditRepository struct {
	root        string
	db          *sql.DB
	mu          sync.Mutex
	lastCleanup time.Time
	// Open/export pins cold files against deletion; captures never take this lock.
	payloadMu sync.RWMutex
	diskUsage func(context.Context, string) (int64, int64, error)
}

type auditColdPayload struct {
	ChatML string `json:"chatml"`
}

type auditQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func NewFileUserRequestAuditRepository(db *sql.DB) *FileUserRequestAuditRepository {
	root := strings.TrimSpace(os.Getenv("DATA_DIR"))
	if root == "" {
		root = "./data"
	}
	return &FileUserRequestAuditRepository{root: filepath.Join(root, "user-request-audit"), db: db}
}

func (r *FileUserRequestAuditRepository) requireDB() error {
	if r == nil || r.db == nil {
		return errors.New("audit metadata database is required")
	}
	return nil
}

// The singleton database row serializes membership edits across repository
// instances, not merely goroutines. Reads remain ordinary MVCC queries.
func (r *FileUserRequestAuditRepository) beginWrite(ctx context.Context) (*sql.Tx, error) {
	if err := r.requireDB(); err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, "UPDATE user_request_audit_writer_lock SET version=1-version WHERE id=1")
	if err == nil {
		var count int64
		count, err = result.RowsAffected()
		if err == nil && count != 1 {
			err = errors.New("audit writer lock is unavailable")
		}
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func auditAtomicJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".audit-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if err = f.Chmod(0o640); err == nil {
		err = json.NewEncoder(f).Encode(value)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return auditSyncDirectory(filepath.Dir(path))
}

func auditSyncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func auditMetadata(row *service.UserRequestAudit) service.UserRequestAudit {
	out := *row
	out.RequestChatML, out.ResponseChatML, out.ChatML = "", "", ""
	out.RequestCount, out.UserLabel = 0, ""
	return out
}

func (r *FileUserRequestAuditRepository) payloadPath(relative string) (string, error) {
	// Paths come from SQL, but constrain them before either reading or unlinking.
	if relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, "\\") || strings.Contains(relative, ":") {
		return "", errors.New("invalid audit payload path")
	}
	clean := filepath.ToSlash(filepath.Clean(relative))
	if clean != relative || !strings.HasPrefix(clean, "payloads/") || strings.Contains(strings.TrimPrefix(clean, "payloads/"), "/") {
		return "", errors.New("invalid audit payload path")
	}
	return filepath.Join(r.root, filepath.FromSlash(clean)), nil
}

func (r *FileUserRequestAuditRepository) writePayload(ctx context.Context, tx *sql.Tx, id int64, kind, text string, ordinal int) (string, int64, error) {
	if len(text) > auditColdPayloadLimit {
		return "", 0, errors.New("audit payload exceeds capture limit")
	}
	relative := "payloads/" + strconv.FormatInt(id, 10) + "-" + kind + "-" + uuid.NewString() + ".json"
	path, err := r.payloadPath(relative)
	if err != nil {
		return "", 0, err
	}
	if err = ctx.Err(); err != nil {
		return "", 0, err
	}
	if err = auditAtomicJSON(path, auditColdPayload{ChatML: text}); err != nil {
		return "", 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		_ = os.Remove(path)
		return "", 0, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO user_request_audit_payloads(request_id,relative_path,kind,ordinal,byte_size) VALUES($1,$2,$3,$4,$5)", id, relative, kind, ordinal, info.Size())
	if err != nil {
		_ = os.Remove(path)
		return "", 0, err
	}
	return relative, info.Size(), nil
}

func (r *FileUserRequestAuditRepository) discardPayloads(paths []string) {
	for _, relative := range paths {
		if path, err := r.payloadPath(relative); err == nil {
			_ = os.Remove(path)
		}
	}
}

func (r *FileUserRequestAuditRepository) Create(ctx context.Context, row *service.UserRequestAudit) error {
	if row == nil {
		return nil
	}
	if err := r.requireDB(); err != nil {
		return err
	}
	if err := r.checkDisk(ctx); err != nil {
		return err
	}
	tx, err := r.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	paths := make([]string, 0, 2)
	committed := false
	defer func() {
		if !committed {
			r.discardPayloads(paths)
		}
	}()
	now := time.Now().UTC()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = row.CreatedAt
	}
	if row.ExpiresAt.IsZero() {
		row.ExpiresAt = row.CreatedAt.AddDate(0, 0, service.UserRequestAuditDefaultRetentionDays)
	}
	// Caller IDs are never accepted. The database sequence survives deletes/clear.
	err = tx.QueryRowContext(ctx, "INSERT INTO user_request_audit_request_index(logical_key,created_at,metadata) VALUES($1,$2,$3) RETURNING id", row.LogicalKey, row.CreatedAt, "{}").Scan(&row.ID)
	if err != nil {
		return err
	}
	if row.SessionID != "" {
		row.ConversationKey = "session:" + row.SessionID
	} else if row.ResponseID != "" {
		row.ConversationKey = "response:" + row.ResponseID
	} else {
		row.ConversationKey = "request:" + strconv.FormatInt(row.ID, 10)
	}
	if len(row.RequestChatML) > auditColdPayloadLimit {
		return errors.New("audit payload exceeds capture limit")
	}
	meta, err := json.Marshal(auditMetadata(row))
	if err != nil {
		return err
	}
	requestProof := service.UserRequestAuditProof(row)
	proof, err := json.Marshal(requestProof)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE user_request_audit_request_index SET metadata=$2,proof=$3 WHERE id=$1", row.ID, string(meta), string(proof)); err != nil {
		return err
	}
	if err = r.regroupScope(ctx, tx, row.UserID, row.APIKeyID, row.ID); err != nil {
		return err
	}
	parent, prefix, err := auditRequestDelta(ctx, tx, row, requestProof)
	if err != nil {
		return err
	}
	requestPath, size, err := r.writePayload(ctx, tx, row.ID, "request", row.RequestChatML[prefix:], 0)
	if err != nil {
		return err
	}
	paths = append(paths, requestPath)
	responsePath := ""
	if row.ResponseChatML != "" {
		var responseSize int64
		responsePath, responseSize, err = r.writePayload(ctx, tx, row.ID, "response", row.ResponseChatML, 1)
		if err != nil {
			return err
		}
		paths = append(paths, responsePath)
		size += responseSize
	}
	_, err = tx.ExecContext(ctx, "UPDATE user_request_audit_request_index SET request_path=$2,response_path=$3,payload_bytes=$4,request_parent_id=$5,request_prefix_bytes=$6,request_bytes=$7,request_sha256=$8 WHERE id=$1", row.ID, requestPath, responsePath, size, parent, prefix, len(row.RequestChatML), auditRequestSHA(row.RequestChatML))
	if err != nil {
		return err
	}
	// Commit errors may be ambiguous. Never delete a potentially committed file.
	committed = true
	return tx.Commit()
}

func (r *FileUserRequestAuditRepository) Complete(ctx context.Context, c *service.UserRequestAuditCompletion) error {
	if c == nil {
		return nil
	}
	if err := r.requireDB(); err != nil {
		return err
	}
	if err := r.checkDisk(ctx); err != nil {
		return err
	}
	tx, err := r.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var raw, proofRaw []byte
	var row service.UserRequestAudit
	var proof service.UserRequestAuditContinuityProof
	err = tx.QueryRowContext(ctx, "SELECT metadata,proof FROM user_request_audit_request_index WHERE logical_key=$1", c.LogicalKey).Scan(&raw, &proofRaw)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &row); err != nil {
		return err
	}
	if err = json.Unmarshal(proofRaw, &proof); err != nil {
		return err
	}
	row.UpdatedAt = time.Now().UTC()
	if c.ResponseID != "" {
		row.ResponseID = c.ResponseID
		normalizeConversationKey(&row)
	}
	if c.UpstreamModel != "" {
		row.UpstreamModel = c.UpstreamModel
	}
	if c.Status != "" {
		row.Status = c.Status
	}
	if c.InputUsage != nil {
		row.InputUsage = c.InputUsage
	}
	if c.OutputUsage != nil {
		row.OutputUsage = c.OutputUsage
	}
	if c.CacheUsage != nil {
		row.CacheUsage = c.CacheUsage
	}
	if c.LastError != "" {
		row.LastError = c.LastError
	}
	if len(c.Metadata) > 0 {
		if row.Metadata == nil {
			row.Metadata = make(map[string]any)
		}
		for key, value := range c.Metadata {
			row.Metadata[key] = value
		}
	}
	paths := make([]string, 0, 1)
	committed := false
	defer func() {
		if !committed {
			r.discardPayloads(paths)
		}
	}()
	if c.ResponseChatML != "" {
		var ordinal int
		if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(ordinal),0)+1 FROM user_request_audit_payloads WHERE request_id=$1", row.ID).Scan(&ordinal); err != nil {
			return err
		}
		path, size, e := r.writePayload(ctx, tx, row.ID, "response", c.ResponseChatML, ordinal)
		if e != nil {
			return e
		}
		paths = append(paths, path)
		if _, err = tx.ExecContext(ctx, "UPDATE user_request_audit_request_index SET response_path=$2,payload_bytes=payload_bytes+$3 WHERE id=$1", row.ID, path, size); err != nil {
			return err
		}
	}
	proof = service.CompleteUserRequestAuditProof(proof, c.ResponseChatML, row.Status, row.Metadata)
	raw, err = json.Marshal(auditMetadata(&row))
	if err != nil {
		return err
	}
	proofRaw, err = json.Marshal(proof)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE user_request_audit_request_index SET metadata=$2,proof=$3 WHERE id=$1", row.ID, string(raw), string(proofRaw)); err != nil {
		return err
	}
	if err = r.regroupScope(ctx, tx, row.UserID, row.APIKeyID, row.ID); err != nil {
		return err
	}
	committed = true
	return tx.Commit()
}

// Association uses SQL metadata and bounded hash proofs only. No historical
// transcript is decoded or rewritten. Only changed memberships are updated.
func (r *FileUserRequestAuditRepository) regroupScope(ctx context.Context, tx *sql.Tx, userID, apiKeyID, changedID int64) error {
	result, err := tx.QueryContext(ctx, "SELECT id,conversation_id,metadata,proof,request_parent_id FROM user_request_audit_request_index WHERE CAST(metadata->>'user_id' AS BIGINT)=$1 AND CAST(metadata->>'api_key_id' AS BIGINT)=$2", userID, apiKeyID)
	if err != nil {
		return err
	}
	rows := make([]*service.UserRequestAudit, 0)
	proofs := make(map[int64]service.UserRequestAuditContinuityProof)
	old := make(map[int64]int64)
	oldConversations := make(map[int64]bool)
	oldCounts := make(map[int64]int)
	dependencies := make(map[int64]int64)
	for result.Next() {
		var id int64
		var cid, parent sql.NullInt64
		var raw, pr []byte
		if err = result.Scan(&id, &cid, &raw, &pr, &parent); err != nil {
			_ = result.Close()
			return err
		}
		row := new(service.UserRequestAudit)
		proof := service.UserRequestAuditContinuityProof{}
		if err = json.Unmarshal(raw, row); err != nil {
			_ = result.Close()
			return err
		}
		if err = json.Unmarshal(pr, &proof); err != nil {
			_ = result.Close()
			return err
		}
		row.ID = id
		rows = append(rows, row)
		proofs[id] = proof
		old[id] = cid.Int64
		if parent.Valid {
			dependencies[id] = parent.Int64
		}
		if cid.Valid {
			oldConversations[cid.Int64] = true
			oldCounts[cid.Int64]++
		}
	}
	err = result.Err()
	_ = result.Close()
	if err != nil {
		return err
	}
	for _, group := range groupAuditConversationsWithDependencies(rows, proofs, dependencies) {
		summary := auditConversationSummary(group)
		expires := group.rows[0].ExpiresAt
		updated := group.rows[0].UpdatedAt
		changed := false
		touched := false
		for _, row := range group.rows {
			if row.ExpiresAt.After(expires) {
				expires = row.ExpiresAt
			}
			if row.UpdatedAt.After(updated) {
				updated = row.UpdatedAt
			}
			if old[row.ID] != summary.ID {
				changed = true
			}
			if row.ID == changedID {
				touched = true
			}
		}
		delete(oldConversations, summary.ID)
		if !changed && !touched && oldCounts[summary.ID] == len(group.rows) {
			continue
		}
		summary.ExpiresAt, summary.UpdatedAt = expires, updated
		raw, e := json.Marshal(summary)
		if e != nil {
			return e
		}
		// Updating summary metadata is cheap and transactionally consistent with membership.
		_, err = tx.ExecContext(ctx, `INSERT INTO user_request_audit_conversations(id,user_id,api_key_id,created_at,updated_at,expires_at,request_count,summary) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(id) DO UPDATE SET updated_at=excluded.updated_at,expires_at=excluded.expires_at,request_count=excluded.request_count,summary=excluded.summary`, summary.ID, userID, apiKeyID, summary.CreatedAt, updated, expires, len(group.rows), string(raw))
		if err != nil {
			return err
		}
		if changed {
			for _, row := range group.rows {
				if old[row.ID] == summary.ID {
					continue
				}
				if _, err = tx.ExecContext(ctx, "UPDATE user_request_audit_request_index SET conversation_id=$2 WHERE id=$1", row.ID, summary.ID); err != nil {
					return err
				}
			}
		}
	}
	for id := range oldConversations {
		if _, err = tx.ExecContext(ctx, "DELETE FROM user_request_audit_conversations WHERE id=$1", id); err != nil {
			return err
		}
	}
	return nil
}

func normalizeConversationKey(row *service.UserRequestAudit) {
	if row != nil && row.ResponseID != "" && (strings.HasPrefix(row.ConversationKey, "fallback:") || strings.HasPrefix(row.ConversationKey, "request:")) {
		row.ConversationKey = "response:" + row.ResponseID
	}
}

func (r *FileUserRequestAuditRepository) UserRequestAuditRoot() string {
	if r == nil {
		return ""
	}
	return r.root
}

type userRequestAuditFileConfig struct {
	RetentionDays        int `json:"retention_days"`
	CleanupIntervalHours int `json:"cleanup_interval_hours"`
}

func (r *FileUserRequestAuditRepository) configPath() string {
	return filepath.Join(r.root, "config.json")
}
func (r *FileUserRequestAuditRepository) GetAuditConfig() service.UserRequestAuditConfig {
	body, err := os.ReadFile(r.configPath())
	if err != nil {
		return service.UserRequestAuditConfig{}
	}
	var stored userRequestAuditFileConfig
	if json.Unmarshal(body, &stored) != nil {
		return service.UserRequestAuditConfig{}
	}
	return service.UserRequestAuditConfig{RetentionDays: stored.RetentionDays, CleanupIntervalHours: stored.CleanupIntervalHours}
}
func (r *FileUserRequestAuditRepository) SetAuditConfig(cfg service.UserRequestAuditConfig) error {
	return auditAtomicJSON(r.configPath(), userRequestAuditFileConfig{RetentionDays: cfg.RetentionDays, CleanupIntervalHours: cfg.CleanupIntervalHours})
}

func (r *FileUserRequestAuditRepository) StorageStatus() service.UserRequestAuditStorageStatus {
	out := service.UserRequestAuditStorageStatus{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out.DiskTotalBytes, out.DiskFreeBytes, out.PauseReason = r.diskStatus(ctx)
	out.RecordingPaused = out.PauseReason != ""
	err := r.requireDB()
	if err == nil {
		err = r.db.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(byte_size),0) FROM user_request_audit_payloads").Scan(&out.FileCount, &out.TotalBytes)
	}
	if err == nil {
		// JSON timestamp strings work identically across PostgreSQL and SQLite.
		var oldest, latest sql.NullString
		err = r.db.QueryRowContext(ctx, "SELECT MIN(summary->>'created_at'),MAX(summary->>'updated_at') FROM user_request_audit_conversations").Scan(&oldest, &latest)
		if oldest.Valid {
			out.Oldest, _ = time.Parse(time.RFC3339Nano, oldest.String)
		}
		if latest.Valid {
			out.Latest, _ = time.Parse(time.RFC3339Nano, latest.String)
		}
	}
	if err != nil {
		out.RecordingPaused = true
		out.PauseReason = "audit metadata database unavailable"
	}
	r.mu.Lock()
	out.LastCleanup = r.lastCleanup
	r.mu.Unlock()
	return out
}

func (r *FileUserRequestAuditRepository) diskStatus(ctx context.Context) (int64, int64, string) {
	path, err := filepath.Abs(r.root)
	if err != nil {
		return 0, 0, "disk space unavailable"
	}
	for {
		_, err = os.Stat(path)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) || filepath.Dir(path) == path {
			return 0, 0, "disk space unavailable"
		}
		path = filepath.Dir(path)
	}
	var total, free int64
	if r.diskUsage != nil {
		total, free, err = r.diskUsage(ctx, path)
	} else {
		var usage *disk.UsageStat
		usage, err = disk.UsageWithContext(ctx, path)
		if err == nil && usage != nil {
			total, free = int64(usage.Total), int64(usage.Free)
		}
	}
	if err != nil || total <= 0 || free < 0 {
		return 0, 0, "disk space unavailable"
	}
	if free < userRequestAuditMinFreeBytes {
		return total, free, "less than 2 GiB free disk space"
	}
	return total, free, ""
}
func (r *FileUserRequestAuditRepository) checkDisk(ctx context.Context) error {
	_, _, reason := r.diskStatus(ctx)
	if reason != "" {
		return fmt.Errorf("audit recording paused: %s", reason)
	}
	return nil
}

var _ service.UserRequestAuditRepository = (*FileUserRequestAuditRepository)(nil)
