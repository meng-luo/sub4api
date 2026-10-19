package repository

import (
	"context"
	"database/sql"

	"github.com/MACOS-DO/sub4api/internal/service"
)

// GetByID serves the single-request detail view. Opening a detail must not
// rebuild every other request in the conversation: only the requested row's own
// bytes (plus its delta ancestry) and its response chunks are read. The readable
// conversation transcript lives on the paged reader endpoint, so a detail lookup
// stays bounded regardless of how long the conversation has grown.
func (r *FileUserRequestAuditRepository) GetByID(ctx context.Context, id int64) (*service.UserRequestAudit, error) {
	if err := r.requireDB(); err != nil {
		return nil, err
	}
	r.payloadMu.RLock()
	defer r.payloadMu.RUnlock()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	ids, err := auditResolveConversationIDs(ctx, tx, []int64{id})
	if err != nil {
		return nil, err
	}
	conversationID := ids[0]
	row, err := r.loadSingleRequest(ctx, tx, conversationID, id)
	if err != nil {
		return nil, err
	}
	if err = auditLabelUsers(ctx, tx, []*service.UserRequestAudit{row}); err != nil {
		return nil, err
	}
	normalizeConversationKey(row)
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM user_request_audit_request_index WHERE conversation_id=$1", conversationID).Scan(&row.RequestCount); err != nil {
		return nil, err
	}
	return row, tx.Commit()
}
