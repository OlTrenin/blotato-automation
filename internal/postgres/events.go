package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"blotato/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Ingest acknowledges only after the canonical event and all workflow changes
// commit together. Neither a webhook receipt nor an execution can be half-saved.
func (s *Store) Ingest(ctx context.Context, tenant, kind string, event domain.Event) (domain.EventResult, error) {
	if err := domain.ValidateEvent(kind, event); err != nil {
		return domain.EventResult{}, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.EventResult{}, err
	}
	defer tx.Rollback(ctx)
	if err = lockConversation(ctx, tx, tenant, event.AccountID, event.Platform, event.UserID); err != nil {
		return domain.EventResult{}, err
	}
	var accountExists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM connected_accounts WHERE tenant_id=$1 AND id=$2 AND platform=$3)`, tenant, event.AccountID, event.Platform).Scan(&accountExists); err != nil {
		return domain.EventResult{}, err
	}
	if !accountExists {
		return domain.EventResult{}, domain.ErrNotFound
	}
	objectID := event.CommentID
	if kind == "message" {
		objectID = event.MessageID
	}
	// Event and object locks handle changed identity arriving under a different
	// user, which would otherwise evade the conversation lock.
	if err = lock(ctx, tx, "event", tenant, event.AccountID, kind, event.EventID); err != nil {
		return domain.EventResult{}, err
	}
	if err = lock(ctx, tx, "object", tenant, event.AccountID, kind, objectID); err != nil {
		return domain.EventResult{}, err
	}
	semantic := event
	semantic.EventID = ""
	encoded, err := json.Marshal(semantic)
	if err != nil {
		return domain.EventResult{}, err
	}
	hashBytes := sha256.Sum256(encoded)
	contentHash := hex.EncodeToString(hashBytes[:])
	var result domain.EventResult
	var existingHash, inboundID string
	err = tx.QueryRow(ctx, `SELECT i.id,r.content_hash,i.outcome,COALESCE(i.execution_id,'') FROM event_receipts r JOIN inbound_events i ON i.id=r.inbound_id WHERE r.tenant_id=$1 AND r.account_id=$2 AND r.kind=$3 AND r.event_id=$4`, tenant, event.AccountID, kind, event.EventID).Scan(&inboundID, &existingHash, &result.Outcome, &result.ExecutionID)
	if err == nil {
		if existingHash != contentHash {
			return result, domain.ErrConflict
		}
		result.Duplicate = true
		return result, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	err = tx.QueryRow(ctx, `SELECT id,content_hash,outcome,COALESCE(execution_id,'') FROM inbound_events WHERE tenant_id=$1 AND account_id=$2 AND kind=$3 AND object_id=$4`, tenant, event.AccountID, kind, objectID).Scan(&inboundID, &existingHash, &result.Outcome, &result.ExecutionID)
	if err == nil {
		if existingHash != contentHash {
			return result, domain.ErrConflict
		}
		if err = s.recordReceipt(ctx, tx, tenant, kind, event, inboundID, contentHash); err != nil {
			return result, err
		}
		result.Duplicate = true
		return result, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	// Expiry and matching share the conversation lock: a late DM cannot beat a
	// periodic sweeper and revive an already expired session.
	var expiredID string
	err = tx.QueryRow(ctx, `UPDATE executions SET state='EXPIRED',updated_at=clock_timestamp() WHERE tenant_id=$1 AND account_id=$2 AND platform=$3 AND user_id=$4 AND state='WAITING_FOR_EMAIL' AND expires_at<=clock_timestamp() RETURNING id`, tenant, event.AccountID, event.Platform, event.UserID).Scan(&expiredID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	active, activeErr := scanExecution(tx.QueryRow(ctx, `SELECT `+executionColumns+` FROM executions WHERE tenant_id=$1 AND account_id=$2 AND platform=$3 AND user_id=$4 AND state IN ('INITIAL_ACTIONS_PENDING','WAITING_FOR_EMAIL','FINAL_MESSAGE_PENDING') FOR UPDATE`, tenant, event.AccountID, event.Platform, event.UserID))
	if activeErr != nil && !errors.Is(activeErr, domain.ErrNotFound) {
		return result, activeErr
	}
	var fromState, toState domain.State
	if kind == "comment" {
		if activeErr == nil {
			result = domain.EventResult{Outcome: "suppressed_active_session", ExecutionID: active.ID}
		} else {
			result, err = s.ingestComment(ctx, tx, tenant, event)
			if err != nil {
				return result, err
			}
			if result.ExecutionID != "" {
				toState = domain.InitialPending
			}
		}
	} else if activeErr != nil {
		result.Outcome = "unmatched"
	} else {
		result.ExecutionID = active.ID
		if active.EmailReceived {
			result.Outcome = "email_already_received"
		} else if email, ok := domain.ExtractEmail(event.Text); !ok {
			result.Outcome = "invalid_email"
		} else {
			result.Outcome = "email_received"
			if active.State == domain.WaitingEmail {
				fromState = active.State
				toState = domain.FinalPending
				_, err = tx.Exec(ctx, `UPDATE executions SET email=$2,state='FINAL_MESSAGE_PENDING',expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, active.ID, email)
				if err == nil {
					err = schedule(ctx, tx, active, domain.SendLink)
				}
			} else {
				_, err = tx.Exec(ctx, `UPDATE executions SET email=$2,updated_at=clock_timestamp() WHERE id=$1`, active.ID, email)
			}
			if err != nil {
				return result, err
			}
		}
	}
	if err = s.reserveQuota(ctx, tx, tenant, "events"); err != nil {
		return result, err
	}
	inboundID = domain.NewID()
	_, err = tx.Exec(ctx, `INSERT INTO inbound_events(id,tenant_id,account_id,platform,kind,object_id,user_id,content_hash,outcome,execution_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''))`, inboundID, tenant, event.AccountID, event.Platform, kind, objectID, event.UserID, contentHash, result.Outcome, result.ExecutionID)
	if err != nil {
		return result, mapError(err)
	}
	if err = s.recordReceipt(ctx, tx, tenant, kind, event, inboundID, contentHash); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	if expiredID != "" {
		slog.InfoContext(ctx, "workflow transition", "execution_id", expiredID, "event_id", event.EventID, "from_state", domain.WaitingEmail, "state", domain.Expired)
	}
	if toState != "" {
		slog.InfoContext(ctx, "workflow transition", "execution_id", result.ExecutionID, "event_id", event.EventID, "from_state", fromState, "state", toState)
	}
	return result, nil
}

func (s *Store) recordReceipt(ctx context.Context, tx pgx.Tx, tenant, kind string, event domain.Event, inboundID, contentHash string) error {
	if err := s.reserveQuota(ctx, tx, tenant, "receipts"); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO event_receipts(tenant_id,account_id,kind,event_id,inbound_id,content_hash) VALUES($1,$2,$3,$4,$5,$6)`, tenant, event.AccountID, kind, event.EventID, inboundID, contentHash)
	return mapError(err)
}

func (s *Store) ingestComment(ctx context.Context, tx pgx.Tx, tenant string, event domain.Event) (domain.EventResult, error) {
	// Walk bounded pages so historical tenants above the new admission cap
	// retain their definitions. Stop at the first match in priority order.
	var chosen *domain.Automation
	var afterTime time.Time
	afterID := ""
	for chosen == nil {
		rows, err := tx.Query(ctx, `SELECT `+automationColumns+` FROM automations
            WHERE tenant_id=$1 AND account_id=$2 AND platform=$3 AND enabled
              AND ($4='' OR (created_at,id)>($5,$4))
            ORDER BY created_at,id LIMIT 100`, tenant, event.AccountID, event.Platform, afterID, afterTime)
		if err != nil {
			return domain.EventResult{}, err
		}
		count := 0
		for rows.Next() {
			a, scanErr := scanAutomation(rows)
			if scanErr != nil {
				rows.Close()
				return domain.EventResult{}, scanErr
			}
			count++
			afterTime, afterID = a.CreatedAt, a.ID
			if domain.MatchKeyword(event.Text, a.Keyword) {
				chosen = &a
				break
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return domain.EventResult{}, err
		}
		if count < 100 {
			break
		}
	}
	if chosen == nil {
		return domain.EventResult{Outcome: "unmatched"}, nil
	}
	snapshot, err := json.Marshal(chosen)
	if err != nil {
		return domain.EventResult{}, err
	}
	e := domain.Execution{ID: domain.NewID(), TenantID: tenant, AutomationID: chosen.ID, AccountID: event.AccountID, Platform: event.Platform, UserID: event.UserID, CommentID: event.CommentID, State: domain.InitialPending, Snapshot: *chosen}
	_, err = tx.Exec(ctx, `INSERT INTO executions(id,tenant_id,automation_id,account_id,platform,user_id,comment_id,state,snapshot) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, e.ID, tenant, chosen.ID, event.AccountID, event.Platform, event.UserID, event.CommentID, e.State, snapshot)
	if err != nil {
		return domain.EventResult{}, mapError(err)
	}
	if err = schedule(ctx, tx, e, domain.ReplyComment); err != nil {
		return domain.EventResult{}, err
	}
	return domain.EventResult{Outcome: "created", ExecutionID: e.ID}, nil
}
