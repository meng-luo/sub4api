package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
)

func auditSQLConditions(f service.UserRequestAuditFilter, requestOnly bool) (string, []any) {
	args := make([]any, 0)
	where := make([]string, 0)
	add := func(expr string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(expr, len(args)))
	}
	if f.UserID != nil {
		add("c.user_id=$%d", *f.UserID)
	}
	if f.APIKeyID != nil {
		add("c.api_key_id=$%d", *f.APIKeyID)
	}
	request := make([]string, 0)
	addRequest := func(expr string, value any) {
		args = append(args, value)
		request = append(request, fmt.Sprintf(expr, len(args)))
	}
	if f.GroupID != nil {
		addRequest("CAST(q.metadata->>'group_id' AS BIGINT)=$%d", *f.GroupID)
	}
	for _, pair := range [][2]string{{"group_name", f.GroupName}, {"protocol", f.Protocol}, {"requested_model", f.RequestedModel}, {"response_id", f.ResponseID}, {"client_request_id", f.ClientRequestID}, {"status", f.Status}} {
		if pair[1] != "" {
			addRequest("lower(q.metadata->>'"+pair[0]+"')=lower($%d)", pair[1])
		}
	}
	if f.StartTime != nil {
		addRequest("q.created_at >= $%d", *f.StartTime)
	}
	if f.EndTime != nil {
		addRequest("q.created_at < $%d", *f.EndTime)
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		// Literal, case-insensitive metadata search. Payloads are never consulted.
		escaped := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(strings.ToLower(q))
		addRequest("lower(CAST(q.metadata AS TEXT)) LIKE $%d ESCAPE '\\'", "%"+escaped+"%")
	}
	if requestOnly {
		where = append(where, request...)
	} else if len(request) > 0 {
		where = append(where, "EXISTS (SELECT 1 FROM user_request_audit_request_index q WHERE q.conversation_id=c.id AND "+strings.Join(request, " AND ")+")")
	}
	if len(where) == 0 {
		return "TRUE", args
	}
	return strings.Join(where, " AND "), args
}

func auditPage(f service.UserRequestAuditFilter) (int, int) {
	page, size := f.Page, f.PageSize
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > 200 {
		size = 200
	}
	return page, size
}

func readAuditMetadataRows(result *sql.Rows) ([]*service.UserRequestAudit, error) {
	defer func() { _ = result.Close() }()
	out := make([]*service.UserRequestAudit, 0)
	for result.Next() {
		var raw []byte
		if err := result.Scan(&raw); err != nil {
			return nil, err
		}
		row := new(service.UserRequestAudit)
		if err := json.Unmarshal(raw, row); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, result.Err()
}

func (r *FileUserRequestAuditRepository) ListConversations(ctx context.Context, f service.UserRequestAuditFilter) ([]*service.UserRequestAudit, int64, error) {
	if err := r.requireDB(); err != nil {
		return nil, 0, err
	}
	where, args := auditSQLConditions(f, false)
	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT count(*) FROM user_request_audit_conversations c WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page, size := auditPage(f)
	args = append(args, size, int64(page-1)*int64(size))
	result, err := r.db.QueryContext(ctx, "SELECT c.summary FROM user_request_audit_conversations c WHERE "+where+fmt.Sprintf(" ORDER BY c.updated_at DESC,c.id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	rows, err := readAuditMetadataRows(result)
	if err == nil {
		err = r.labelUsers(ctx, rows)
	}
	return rows, total, err
}

// ListModelFacets reports the models actually present in the retained archive so
// callers select from evidence instead of guessing model strings. Request-level
// predicates are shared with listing/export, so the facets track the active
// time/group/protocol filters exactly.
func (r *FileUserRequestAuditRepository) ListModelFacets(ctx context.Context, f service.UserRequestAuditFilter) ([]service.UserRequestAuditModelFacet, error) {
	if err := r.requireDB(); err != nil {
		return nil, err
	}
	where, args := auditSQLConditions(f, true)
	result, err := r.db.QueryContext(ctx, `SELECT COALESCE(q.metadata->>'protocol',''), q.metadata->>'requested_model', count(*), count(DISTINCT q.conversation_id), max(q.created_at) FROM user_request_audit_request_index q JOIN user_request_audit_conversations c ON c.id=q.conversation_id WHERE `+where+` AND COALESCE(q.metadata->>'requested_model','') <> '' GROUP BY 1,2 ORDER BY count(*) DESC,1,2 LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = result.Close() }()
	out := make([]service.UserRequestAuditModelFacet, 0)
	for result.Next() {
		var facet service.UserRequestAuditModelFacet
		var latest any
		if err := result.Scan(&facet.Protocol, &facet.RequestedModel, &facet.Requests, &facet.Conversations, &latest); err != nil {
			return nil, err
		}
		switch value := latest.(type) {
		case time.Time:
			facet.LatestAt = value.UTC()
		case string:
			if parsed, e := time.Parse(time.RFC3339Nano, value); e == nil {
				facet.LatestAt = parsed.UTC()
			}
		}
		out = append(out, facet)
	}
	return out, result.Err()
}

func auditResolveConversationIDs(ctx context.Context, tx *sql.Tx, ids []int64) ([]int64, error) {
	out := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		var conversationID int64
		if err := tx.QueryRowContext(ctx, "SELECT conversation_id FROM user_request_audit_request_index WHERE id=$1", id).Scan(&conversationID); err != nil {
			return nil, err
		}
		if !seen[conversationID] {
			seen[conversationID] = true
			out = append(out, conversationID)
		}
	}
	return out, nil
}

// Only explicit open/export callers reach this decoder. Its transaction pins
// SQL membership; immutable chunks preserve the corresponding payload version.
func (r *FileUserRequestAuditRepository) loadConversation(ctx context.Context, tx *sql.Tx, id int64) (*service.UserRequestAuditConversation, error) {
	result, err := tx.QueryContext(ctx, "SELECT id,metadata,request_parent_id,request_prefix_bytes,request_bytes,request_sha256 FROM user_request_audit_request_index WHERE conversation_id=$1 ORDER BY created_at,id", id)
	if err != nil {
		return nil, err
	}
	rows := make([]*service.UserRequestAudit, 0)
	lineage := make(map[int64]auditRequestLineage)
	for result.Next() {
		var requestID int64
		var raw []byte
		var link auditRequestLineage
		if err = result.Scan(&requestID, &raw, &link.parent, &link.prefix, &link.size, &link.hash); err != nil {
			_ = result.Close()
			return nil, err
		}
		row := new(service.UserRequestAudit)
		if err = json.Unmarshal(raw, row); err != nil {
			_ = result.Close()
			return nil, err
		}
		row.ID = requestID
		rows = append(rows, row)
		lineage[requestID] = link
	}
	err = result.Err()
	_ = result.Close()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, sql.ErrNoRows
	}
	byID := make(map[int64]*service.UserRequestAudit, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	result, err = tx.QueryContext(ctx, `SELECT p.request_id,p.relative_path,p.kind FROM user_request_audit_payloads p JOIN user_request_audit_request_index q ON q.id=p.request_id WHERE q.conversation_id=$1 ORDER BY q.created_at,q.id,p.ordinal`, id)
	if err != nil {
		return nil, err
	}
	type chunk struct {
		id         int64
		path, kind string
	}
	chunks := make([]chunk, 0)
	for result.Next() {
		var c chunk
		if err = result.Scan(&c.id, &c.path, &c.kind); err != nil {
			_ = result.Close()
			return nil, err
		}
		chunks = append(chunks, c)
	}
	err = result.Err()
	_ = result.Close()
	if err != nil {
		return nil, err
	}
	requestChunks := make(map[int64]bool, len(rows))
	for _, c := range chunks {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		path, e := r.payloadPath(c.path)
		if e != nil {
			return nil, e
		}
		f, e := openAuditSnapshot(path)
		if e != nil {
			return nil, e
		}
		var payload auditColdPayload
		e = json.NewDecoder(io.LimitReader(f, 6*auditColdPayloadLimit+128)).Decode(&payload)
		closeErr := f.Close()
		if e != nil {
			return nil, fmt.Errorf("decode audit payload %s: %w", c.path, e)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(payload.ChatML) > auditColdPayloadLimit {
			return nil, fmt.Errorf("audit payload %s exceeds capture limit", c.path)
		}
		row := byID[c.id]
		if c.kind == "request" {
			if requestChunks[c.id] {
				return nil, fmt.Errorf("duplicate audit request chunk for %d", c.id)
			}
			requestChunks[c.id] = true
			row.RequestChatML = payload.ChatML
		} else {
			if row.ResponseChatML != "" && !strings.HasSuffix(row.ResponseChatML, "\n") && !strings.HasPrefix(payload.ChatML, "\n") {
				row.ResponseChatML += "\n"
			}
			row.ResponseChatML += payload.ChatML
		}
	}
	for _, row := range rows {
		if !requestChunks[row.ID] {
			return nil, fmt.Errorf("missing audit request chunk for %d", row.ID)
		}
	}
	if err = restoreAuditRequests(byID, lineage); err != nil {
		return nil, err
	}
	if err = auditLabelUsers(ctx, tx, rows); err != nil {
		return nil, err
	}
	return &service.UserRequestAuditConversation{ID: id, UserLabel: rows[0].UserLabel, RequestCount: len(rows), RequestStart: 1, RequestEnd: len(rows), Requests: rows, Messages: service.BuildUserRequestAuditTranscript(rows)}, nil
}

func (r *FileUserRequestAuditRepository) GetConversation(ctx context.Context, id int64) (*service.UserRequestAuditConversation, error) {
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
	doc, err := r.loadConversation(ctx, tx, ids[0])
	if err != nil {
		return nil, err
	}
	return doc, tx.Commit()
}

// GetConversationWindow reads one request range of a conversation. The reader
// lock is held for the same reason as a full read: immutable payload files must
// not be deleted while their paths are still being resolved from SQL.
func (r *FileUserRequestAuditRepository) GetConversationWindow(ctx context.Context, id int64, offset, limit int) (*service.UserRequestAuditConversation, error) {
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
	doc, _, _, err := r.loadConversationWindow(ctx, tx, ids[0], offset, limit)
	if err != nil {
		return nil, err
	}
	return doc, tx.Commit()
}

// Exports use one repeatable-read SQL snapshot and pin immutable files against
// local deletion. Capture and completion can publish concurrently throughout.
func (r *FileUserRequestAuditRepository) ExportConversations(ctx context.Context, f service.UserRequestAuditFilter, ids []int64, w io.Writer, format string) (int, error) {
	if err := r.requireDB(); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if format != "json" && format != "jsonl" {
		return 0, fmt.Errorf("unsupported audit export format %q", format)
	}
	if err := r.checkDisk(ctx); err != nil {
		return 0, err
	}
	r.payloadMu.RLock()
	defer r.payloadMu.RUnlock()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var summaries []*service.UserRequestAudit
	where, args := auditSQLConditions(f, false)
	if len(ids) > 0 {
		// The selection narrows the filtered set; filters always apply, so an
		// export of selected conversations honours the active model/group/time filters.
		var resolved []int64
		resolved, err = auditResolveConversationIDs(ctx, tx, ids)
		if err != nil {
			return 0, err
		}
		if len(resolved) == 0 {
			return 0, nil
		}
		placeholders := make([]string, 0, len(resolved))
		for _, id := range resolved {
			args = append(args, id)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		where += " AND c.id IN (" + strings.Join(placeholders, ",") + ")"
	}
	result, e := tx.QueryContext(ctx, "SELECT c.summary FROM user_request_audit_conversations c WHERE "+where+" ORDER BY c.updated_at DESC,c.id DESC", args...)
	if e != nil {
		return 0, e
	}
	summaries, err = readAuditMetadataRows(result)
	if err != nil {
		return 0, err
	}
	writer := auditExportContextWriter{ctx: ctx, writer: w}
	if format == "json" {
		if _, err = io.WriteString(writer, "[\n"); err != nil {
			return 0, err
		}
	}
	count := 0
	enc := json.NewEncoder(writer)
	for _, summary := range summaries {
		doc, e := r.loadConversation(ctx, tx, summary.ID)
		if e != nil {
			return count, e
		}
		summary.UserLabel = doc.UserLabel
		summary.ChatML = auditMessagesTranscript(doc.Messages)
		if format == "json" && count > 0 {
			if _, err = io.WriteString(writer, ",\n"); err != nil {
				return count, err
			}
		}
		if err = enc.Encode(struct {
			*service.UserRequestAudit
			Requests []*service.UserRequestAudit       `json:"requests"`
			Messages []service.UserRequestAuditMessage `json:"messages"`
		}{summary, doc.Requests, doc.Messages}); err != nil {
			return count, err
		}
		count++
	}
	if format == "json" {
		if _, err = io.WriteString(writer, "]\n"); err != nil {
			return count, err
		}
	}
	return count, tx.Commit()
}

type auditExportContextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w auditExportContextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}

func auditIDArguments(ids []int64) (string, []any) {
	names := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		names[i] = "$" + strconv.Itoa(i+1)
		args[i] = id
	}
	return strings.Join(names, ","), args
}
