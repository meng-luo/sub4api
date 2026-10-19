package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/MACOS-DO/sub4api/internal/service"
)

// A stored request is either the whole history or an appended delta over its
// parent. Rebuilding a long conversation naively is quadratic: request k
// contains the entire history, so each row copies and re-hashes the growing
// text. Windowed reads need exact bytes only for requests inside the window, so
// delta chains are walked once and only selected requests are materialized.

// loadConversationWindow reads requests [skip, skip+limit) of one conversation,
// oldest first. Payload chunks are opened only for those requests plus, when a
// boundary is needed, the single preceding request that supplies window-relative
// history so the rendered transcript is identical to a full read.
func (r *FileUserRequestAuditRepository) loadConversationWindow(ctx context.Context, tx *sql.Tx, id int64, skip, limit int) (*service.UserRequestAuditConversation, int, int, error) {
	skip, limit = service.ConversationWindowLimits(skip, limit)
	var total int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM user_request_audit_request_index WHERE conversation_id=$1", id).Scan(&total); err != nil {
		return nil, 0, 0, err
	}
	if skip >= total {
		return nil, 0, 0, sql.ErrNoRows
	}
	if limit > total-skip {
		limit = total - skip
	}
	// Include only the preceding ID needed for transcript replay detection.
	start, count := skip, limit
	if start > 0 {
		start--
		count++
	}
	ids, err := tx.QueryContext(ctx, "SELECT id FROM user_request_audit_request_index WHERE conversation_id=$1 ORDER BY created_at,id LIMIT $2 OFFSET $3", id, count, start)
	if err != nil {
		return nil, 0, 0, err
	}
	all := make([]int64, 0, count)
	for ids.Next() {
		var requestID int64
		if err = ids.Scan(&requestID); err != nil {
			_ = ids.Close()
			return nil, 0, 0, err
		}
		all = append(all, requestID)
	}
	err = ids.Err()
	_ = ids.Close()
	if err != nil {
		return nil, 0, 0, err
	}
	if len(all) != count {
		return nil, 0, 0, fmt.Errorf("audit window index changed during read")
	}
	window := all
	if skip > 0 {
		window = all[1:]
	}

	placeholders, args := auditIDArguments(window)
	result, err := tx.QueryContext(ctx, `SELECT id,metadata,request_parent_id,request_prefix_bytes,request_bytes,request_sha256 FROM user_request_audit_request_index WHERE id IN (`+placeholders+`) ORDER BY created_at,id`, args...)
	if err != nil {
		return nil, 0, 0, err
	}
	byID := make(map[int64]*service.UserRequestAudit, len(window))
	lineage := make(map[int64]auditRequestLineage, len(window))
	ordered := make([]int64, 0, len(window))
	for result.Next() {
		var requestID int64
		var raw []byte
		var link auditRequestLineage
		if err = result.Scan(&requestID, &raw, &link.parent, &link.prefix, &link.size, &link.hash); err != nil {
			_ = result.Close()
			return nil, 0, 0, err
		}
		row := new(service.UserRequestAudit)
		if err = json.Unmarshal(raw, row); err != nil {
			_ = result.Close()
			return nil, 0, 0, err
		}
		row.ID = requestID
		byID[requestID] = row
		lineage[requestID] = link
		ordered = append(ordered, requestID)
	}
	err = result.Err()
	_ = result.Close()
	if err != nil {
		return nil, 0, 0, err
	}
	if len(ordered) != len(window) {
		return nil, 0, 0, fmt.Errorf("audit window lost %d of %d requests", len(window)-len(ordered), len(window))
	}

	// Selected request/response chunks only.
	placeholders, args = auditIDArguments(window)
	result, err = tx.QueryContext(ctx, `SELECT request_id,relative_path,kind FROM user_request_audit_payloads WHERE request_id IN (`+placeholders+`) ORDER BY request_id,ordinal`, args...)
	if err != nil {
		return nil, 0, 0, err
	}
	gotRequest := make(map[int64]bool, len(window))
	for result.Next() {
		var requestID int64
		var path, kind string
		if err = result.Scan(&requestID, &path, &kind); err != nil {
			_ = result.Close()
			return nil, 0, 0, err
		}
		text, e := readAuditPayload(ctx, r, path)
		if e != nil {
			_ = result.Close()
			return nil, 0, 0, e
		}
		row := byID[requestID]
		if kind == "request" {
			if gotRequest[requestID] {
				_ = result.Close()
				return nil, 0, 0, fmt.Errorf("duplicate audit request chunk for %d", requestID)
			}
			gotRequest[requestID] = true
			row.RequestChatML = text
		} else {
			if row.ResponseChatML != "" && !endsWithNewline(row.ResponseChatML) && !startsWithNewline(text) {
				row.ResponseChatML += "\n"
			}
			row.ResponseChatML += text
		}
	}
	err = result.Err()
	_ = result.Close()
	if err != nil {
		return nil, 0, 0, err
	}
	for _, requestID := range ordered {
		if !gotRequest[requestID] {
			return nil, 0, 0, fmt.Errorf("missing audit request chunk for %d", requestID)
		}
	}

	// Delta rows carry only the appended suffix, so a request whose parent is
	// outside the window needs exactly one ancestor document. In-window parents
	// chain without any additional read.
	cached := make(map[int64]string, 2)
	for _, requestID := range ordered {
		row := byID[requestID]
		link := lineage[requestID]
		if link.size < 0 || link.size > auditColdPayloadLimit || link.prefix < 0 || link.prefix > link.size {
			return nil, 0, 0, fmt.Errorf("invalid audit request bounds for %d", requestID)
		}
		if link.parent.Valid {
			var parentText string
			if parent := byID[link.parent.Int64]; parent != nil {
				parentText = parent.RequestChatML
			} else if text, ok := cached[link.parent.Int64]; ok {
				parentText = text
			} else {
				text, e := r.loadAncestorRequest(ctx, tx, id, link.parent.Int64)
				if e != nil {
					return nil, 0, 0, e
				}
				cached[link.parent.Int64] = text
				parentText = text
			}
			if link.prefix == 0 || len(parentText) < link.prefix {
				return nil, 0, 0, fmt.Errorf("invalid audit request ancestry for %d", requestID)
			}
			// A delta row stores size-prefix bytes; a full row stores all size bytes.
			if stored := len(row.RequestChatML); stored == link.size {
			} else if stored == link.size-link.prefix {
				row.RequestChatML = parentText[:link.prefix] + row.RequestChatML
			} else {
				return nil, 0, 0, fmt.Errorf("audit request ancestry gap for %d", requestID)
			}
			cached[requestID] = row.RequestChatML
		} else if link.prefix != 0 {
			return nil, 0, 0, fmt.Errorf("orphan audit request prefix for %d", requestID)
		}
		if link.hash != "" && (len(row.RequestChatML) != link.size || auditRequestSHA(row.RequestChatML) != link.hash) {
			return nil, 0, 0, fmt.Errorf("audit request integrity mismatch for %d", requestID)
		}
	}

	rows := make([]*service.UserRequestAudit, 0, len(ordered))
	for _, requestID := range ordered {
		rows = append(rows, byID[requestID])
	}
	if err = auditLabelUsers(ctx, tx, rows); err != nil {
		return nil, 0, 0, err
	}

	// Reproducing the transcript requires the history the first windowed request
	// replays: the immediately preceding request plus its response. One bounded
	// read, identical semantics to a full conversation build.
	var seed *service.UserRequestAudit
	if skip > 0 {
		seed, err = r.loadSingleRequest(ctx, tx, id, all[0])
	}
	if err != nil {
		return nil, 0, 0, err
	}
	doc := &service.UserRequestAuditConversation{
		ID:           id,
		UserLabel:    rows[0].UserLabel,
		RequestCount: total,
		RequestStart: skip + 1,
		RequestEnd:   skip + len(rows),
		Requests:     rows,
		Messages:     service.BuildUserRequestAuditTranscriptFrom(rows, seed),
	}
	return doc, skip, skip + len(rows), nil
}

// loadSingleRequest resolves one request's full text and response outside any
// window. Delta chains are replayed root-first, costing the ancestor's own size
// exactly once.
func (r *FileUserRequestAuditRepository) loadSingleRequest(ctx context.Context, tx *sql.Tx, conversationID, requestID int64) (*service.UserRequestAudit, error) {
	row := new(service.UserRequestAudit)
	var raw []byte
	var link auditRequestLineage
	err := tx.QueryRowContext(ctx, `SELECT metadata,request_parent_id,request_prefix_bytes,request_bytes,request_sha256 FROM user_request_audit_request_index WHERE id=$1 AND conversation_id=$2`, requestID, conversationID).
		Scan(&raw, &link.parent, &link.prefix, &link.size, &link.hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("missing audit request %d", requestID)
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, row); err != nil {
		return nil, err
	}
	row.ID = requestID
	if row.RequestChatML, err = r.loadAncestorRequest(ctx, tx, conversationID, requestID); err != nil {
		return nil, err
	}
	placeholders, args := auditIDArguments([]int64{requestID})
	result, err := tx.QueryContext(ctx, `SELECT relative_path FROM user_request_audit_payloads WHERE request_id IN (`+placeholders+`) AND kind<>'request' ORDER BY ordinal`, args...)
	if err != nil {
		return nil, err
	}
	for result.Next() {
		var path string
		if err = result.Scan(&path); err != nil {
			_ = result.Close()
			return nil, err
		}
		text, e := readAuditPayload(ctx, r, path)
		if e != nil {
			_ = result.Close()
			return nil, e
		}
		if row.ResponseChatML != "" && !endsWithNewline(row.ResponseChatML) && !startsWithNewline(text) {
			row.ResponseChatML += "\n"
		}
		row.ResponseChatML += text
	}
	err = result.Err()
	_ = result.Close()
	if err != nil {
		return nil, err
	}
	return row, nil
}

// loadAncestorRequest walks a delta chain to its root and replays it downward,
// returning only the requested request's exact bytes.
func (r *FileUserRequestAuditRepository) loadAncestorRequest(ctx context.Context, tx *sql.Tx, conversationID, requestID int64) (string, error) {
	type node struct {
		id      int64
		parent  sql.NullInt64
		prefix  int
		size    int
		hash    string
		stored  string
		missing bool
	}
	chain := make([]node, 0, 8)
	seen := make(map[int64]bool, 8)
	current := requestID
	for {
		if current <= 0 || seen[current] {
			return "", fmt.Errorf("invalid audit request ancestry at %d", current)
		}
		seen[current] = true
		n := node{id: current}
		err := tx.QueryRowContext(ctx, `SELECT request_parent_id,request_prefix_bytes,request_bytes,request_sha256 FROM user_request_audit_request_index WHERE id=$1 AND conversation_id=$2`, current, conversationID).
			Scan(&n.parent, &n.prefix, &n.size, &n.hash)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("missing audit request ancestor %d", current)
		}
		if err != nil {
			return "", err
		}
		if n.parent.Valid && n.parent.Int64 >= n.id {
			return "", fmt.Errorf("audit request ancestry cycle at %d", n.id)
		}
		if n.stored, n.missing, err = r.readStoredRequest(ctx, tx, current); err != nil {
			return "", err
		}
		if n.missing {
			return "", fmt.Errorf("missing audit request chunk for %d", n.id)
		}
		chain = append(chain, n)
		if !n.parent.Valid {
			break
		}
		current = n.parent.Int64
	}
	text := ""
	for i := len(chain) - 1; i >= 0; i-- {
		n := chain[i]
		if !n.parent.Valid {
			if n.prefix != 0 {
				return "", fmt.Errorf("orphan audit request prefix for %d", n.id)
			}
			text = n.stored
		} else if len(n.stored) == n.size {
			text = n.stored
		} else if n.prefix == len(text) && len(n.stored) == n.size-n.prefix {
			text += n.stored
		} else {
			return "", fmt.Errorf("audit request ancestry gap for %d", n.id)
		}
		if n.hash != "" && (len(text) != n.size || auditRequestSHA(text) != n.hash) {
			return "", fmt.Errorf("audit request integrity mismatch for %d", n.id)
		}
	}
	return text, nil
}

func (r *FileUserRequestAuditRepository) readStoredRequest(ctx context.Context, tx *sql.Tx, requestID int64) (string, bool, error) {
	var path string
	err := tx.QueryRowContext(ctx, "SELECT relative_path FROM user_request_audit_payloads WHERE request_id=$1 AND kind='request' ORDER BY ordinal LIMIT 1", requestID).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	text, err := readAuditPayload(ctx, r, path)
	return text, false, err
}

func readAuditPayload(ctx context.Context, r *FileUserRequestAuditRepository, relative string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := r.payloadPath(relative)
	if err != nil {
		return "", err
	}
	f, err := openAuditSnapshot(path)
	if err != nil {
		return "", err
	}
	var payload auditColdPayload
	err = json.NewDecoder(io.LimitReader(f, 6*auditColdPayloadLimit+128)).Decode(&payload)
	closeErr := f.Close()
	if err != nil {
		return "", fmt.Errorf("decode audit payload %s: %w", relative, err)
	}
	if closeErr != nil {
		return "", closeErr
	}
	if len(payload.ChatML) > auditColdPayloadLimit {
		return "", fmt.Errorf("audit payload %s exceeds capture limit", relative)
	}
	return payload.ChatML, nil
}

func endsWithNewline(s string) bool   { return s != "" && s[len(s)-1] == '\n' }
func startsWithNewline(s string) bool { return s != "" && s[0] == '\n' }
