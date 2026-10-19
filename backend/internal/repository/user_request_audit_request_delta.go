package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/MACOS-DO/sub4api/internal/service"
)

func auditRequestSHA(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// A request only borrows immutable request bytes, never an evolving completion.
// Candidate lookup is bounded SQL metadata. Neither a miss nor an old snapshot
// without byte proofs triggers a cold-file read during capture.
func auditRequestDelta(ctx context.Context, tx *sql.Tx, row *service.UserRequestAudit, proof service.UserRequestAuditContinuityProof) (any, int, error) {
	if !proof.RequestValid || proof.RequestDigest == "" || len(row.RequestChatML) == 0 {
		return nil, 0, nil
	}
	result, err := tx.QueryContext(ctx, `SELECT p.id,p.request_bytes,p.request_sha256,p.proof FROM user_request_audit_request_index p JOIN user_request_audit_request_index q ON q.conversation_id=p.conversation_id WHERE q.id=$1 AND p.id<q.id AND p.request_bytes>0 AND p.request_bytes<=$2 AND p.request_sha256<>'' AND CAST(p.metadata->>'user_id' AS BIGINT)=$3 AND CAST(p.metadata->>'api_key_id' AS BIGINT)=$4 ORDER BY p.id DESC LIMIT 256`, row.ID, len(row.RequestChatML), row.UserID, row.APIKeyID)
	if err != nil {
		return nil, 0, err
	}
	type candidate struct {
		id           int64
		size         int
		hash, actual string
		proof        []byte
	}
	candidates := make([]candidate, 0)
	for result.Next() {
		var c candidate
		if err = result.Scan(&c.id, &c.size, &c.hash, &c.proof); err != nil {
			_ = result.Close()
			return nil, 0, err
		}
		candidates = append(candidates, c)
	}
	err = result.Err()
	_ = result.Close()
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].size == candidates[j].size {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].size < candidates[j].size
	})
	// Hash the current request only once across all candidate byte boundaries.
	hasher := sha256.New()
	offset := 0
	for i := range candidates {
		c := &candidates[i]
		if c.size <= 0 || c.size > len(row.RequestChatML) {
			continue
		}
		_, _ = hasher.Write([]byte(row.RequestChatML[offset:c.size]))
		c.actual = hex.EncodeToString(hasher.Sum(nil))
		offset = c.size
	}
	for i := len(candidates) - 1; i >= 0; i-- {
		c := &candidates[i]
		if c.actual == "" || c.actual != c.hash {
			continue
		}
		var previous service.UserRequestAuditContinuityProof
		if err = json.Unmarshal(c.proof, &previous); err != nil {
			return nil, 0, err
		}
		if !previous.RequestValid || previous.RequestDigest == "" {
			continue
		}
		// Parse only this capture's bytes, never historical evidence. Exact
		// byte identity is mandatory even when normalization is equivalent.
		prefixProof := service.UserRequestAuditProof(&service.UserRequestAudit{RequestChatML: row.RequestChatML[:c.size], Protocol: row.Protocol, Metadata: row.Metadata})
		if prefixProof.RequestValid && prefixProof.RequestDigest == previous.RequestDigest {
			return c.id, c.size, nil
		}
	}
	return nil, 0, nil
}

type auditRequestLineage struct {
	parent sql.NullInt64
	prefix int
	size   int
	hash   string
}

// SQL and immutable chunks were read from the same pinned snapshot. Smaller
// parent IDs make reconstruction iterative and reject cycles before traversal.
func restoreAuditRequests(rows map[int64]*service.UserRequestAudit, lineage map[int64]auditRequestLineage) error {
	ids := make([]int64, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		row := rows[id]
		link := lineage[id]
		if link.size < 0 || link.size > auditColdPayloadLimit || link.prefix < 0 || link.prefix > link.size {
			return fmt.Errorf("invalid audit request bounds for %d", id)
		}
		if link.parent.Valid {
			parent := rows[link.parent.Int64]
			parentLink := lineage[link.parent.Int64]
			if link.parent.Int64 >= id || parent == nil || parent.UserID != row.UserID || parent.APIKeyID != row.APIKeyID || link.prefix == 0 || parentLink.hash == "" || link.hash == "" || link.prefix != len(parent.RequestChatML) || len(row.RequestChatML) != link.size-link.prefix {
				return fmt.Errorf("invalid audit request ancestry for %d", id)
			}
			row.RequestChatML = parent.RequestChatML + row.RequestChatML
		} else if link.prefix != 0 {
			return fmt.Errorf("orphan audit request prefix for %d", id)
		}
		// Empty hashes identify unchanged snapshots captured before migration 250.
		if link.hash != "" && (len(row.RequestChatML) != link.size || auditRequestSHA(row.RequestChatML) != link.hash) {
			return fmt.Errorf("audit request integrity mismatch for %d", id)
		}
	}
	return nil
}
