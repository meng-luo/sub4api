package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"
)

// Deletion never discovers paths from disk. The transaction queues every known
// immutable payload before deleting membership; the SQL queue survives crashes.
func (r *FileUserRequestAuditRepository) deleteConversations(ctx context.Context, tx *sql.Tx, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders, args := auditIDArguments(ids)
	var count int64
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM user_request_audit_request_index WHERE conversation_id IN ("+placeholders+")", args...).Scan(&count); err != nil {
		return 0, err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO user_request_audit_payload_gc(relative_path) SELECT p.relative_path FROM user_request_audit_payloads p JOIN user_request_audit_request_index q ON q.id=p.request_id WHERE q.conversation_id IN (`+placeholders+`) ON CONFLICT(relative_path) DO NOTHING`, args...)
	if err != nil {
		return 0, err
	}
	// Explicit child deletes also make the contract independent of SQLite FK pragmas.
	if _, err = tx.ExecContext(ctx, "DELETE FROM user_request_audit_payloads WHERE request_id IN (SELECT id FROM user_request_audit_request_index WHERE conversation_id IN ("+placeholders+"))", args...); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM user_request_audit_request_index WHERE conversation_id IN ("+placeholders+")", args...); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM user_request_audit_conversations WHERE id IN ("+placeholders+")", args...); err != nil {
		return 0, err
	}
	return count, nil
}

func (r *FileUserRequestAuditRepository) drainPayloadGC(ctx context.Context) error {
	for {
		result, err := r.db.QueryContext(ctx, "SELECT relative_path FROM user_request_audit_payload_gc ORDER BY relative_path LIMIT 500")
		if err != nil {
			return err
		}
		paths := make([]string, 0, 500)
		for result.Next() {
			var path string
			if err = result.Scan(&path); err != nil {
				_ = result.Close()
				return err
			}
			paths = append(paths, path)
		}
		err = result.Err()
		_ = result.Close()
		if err != nil {
			return err
		}
		if len(paths) == 0 {
			return nil
		}
		for _, relative := range paths {
			if err = ctx.Err(); err != nil {
				return err
			}
			path, e := r.payloadPath(relative)
			if e != nil {
				return e
			}
			if e = os.Remove(path); e != nil && !os.IsNotExist(e) {
				return fmt.Errorf("remove audit payload: %w", e)
			}
			if _, e = r.db.ExecContext(ctx, "DELETE FROM user_request_audit_payload_gc WHERE relative_path=$1", relative); e != nil {
				return e
			}
		}
	}
}

func (r *FileUserRequestAuditRepository) Clear(ctx context.Context) (int64, error) {
	if err := r.requireDB(); err != nil {
		return 0, err
	}
	r.payloadMu.Lock()
	defer r.payloadMu.Unlock()
	tx, err := r.beginWrite(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var count int64
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM user_request_audit_request_index").Scan(&count); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO user_request_audit_payload_gc(relative_path) SELECT relative_path FROM user_request_audit_payloads WHERE TRUE ON CONFLICT(relative_path) DO NOTHING"); err != nil {
		return 0, err
	}
	for _, table := range []string{"user_request_audit_payloads", "user_request_audit_request_index", "user_request_audit_conversations"} {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return count, r.drainPayloadGC(ctx)
}

func (r *FileUserRequestAuditRepository) DeleteExpired(ctx context.Context, before time.Time, batchSize int) (int64, error) {
	if err := r.requireDB(); err != nil {
		return 0, err
	}
	if batchSize <= 0 {
		batchSize = 500
	}
	if batchSize > 500 {
		batchSize = 500
	}
	r.payloadMu.Lock()
	defer r.payloadMu.Unlock()
	tx, err := r.beginWrite(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.QueryContext(ctx, "SELECT id FROM user_request_audit_conversations WHERE expires_at<$1 ORDER BY expires_at,id LIMIT $2", before, batchSize)
	if err != nil {
		return 0, err
	}
	ids := make([]int64, 0, batchSize)
	for result.Next() {
		var id int64
		if err = result.Scan(&id); err != nil {
			_ = result.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = result.Err()
	_ = result.Close()
	if err != nil {
		return 0, err
	}
	count, err := r.deleteConversations(ctx, tx, ids)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	err = r.drainPayloadGC(ctx)
	if err == nil {
		r.mu.Lock()
		r.lastCleanup = time.Now().UTC()
		r.mu.Unlock()
	}
	return count, err
}
