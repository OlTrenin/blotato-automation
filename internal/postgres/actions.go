package postgres

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"blotato/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Claim is a short transaction. Platform I/O occurs after it commits. Expired
// leases consume another attempt so repeated crashes eventually exhaust retries.
func (s *Store) Claim(ctx context.Context, lease time.Duration) (*domain.Action, error) {
	if lease <= 0 {
		return nil, domain.ErrInvalid
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `SELECT a.id FROM actions a JOIN executions e ON e.id=a.execution_id
		WHERE e.state IN ('INITIAL_ACTIONS_PENDING','FINAL_MESSAGE_PENDING') AND
		((a.status='pending' AND a.available_at<=clock_timestamp()) OR
		 (a.status='processing' AND a.lease_until<=clock_timestamp()))
		ORDER BY a.available_at,a.id FOR UPDATE OF a SKIP LOCKED LIMIT 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a, err := scanAction(tx.QueryRow(ctx, `UPDATE actions SET status='processing',attempts=attempts+1,
		lease_token=$2,lease_until=clock_timestamp()+($3::double precision*interval '1 second') WHERE id=$1 RETURNING `+actionColumns,
		id, domain.NewID(), lease.Seconds()))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &a, nil
}

// lockDelivery follows the same order as ingestion: conversation, execution,
// action. The durable action, not the caller's body/kind, determines effects.
func lockDelivery(ctx context.Context, tx pgx.Tx, a domain.Action) (domain.Execution, domain.Action, bool, error) {
	if err := lockConversation(ctx, tx, a.TenantID, a.AccountID, a.Platform, a.UserID); err != nil {
		return domain.Execution{}, domain.Action{}, false, err
	}
	e, err := scanExecution(tx.QueryRow(ctx, `SELECT `+executionColumns+` FROM executions WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, a.TenantID, a.ExecutionID))
	if err != nil {
		return e, domain.Action{}, false, err
	}
	current, err := scanAction(tx.QueryRow(ctx, `SELECT `+actionColumns+` FROM actions WHERE tenant_id=$1 AND execution_id=$2 AND id=$3 AND status='processing' AND lease_token=$4 AND lease_until>clock_timestamp() FOR UPDATE`, a.TenantID, a.ExecutionID, a.ID, a.LeaseToken))
	if errors.Is(err, domain.ErrNotFound) {
		return e, current, false, nil
	}
	if err != nil {
		return e, current, false, err
	}
	return e, current, e.State.Active(), nil
}

func (s *Store) Delivered(ctx context.Context, a domain.Action, waitTTL time.Duration) (bool, error) {
	if waitTTL <= 0 {
		return false, domain.ErrInvalid
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	e, current, valid, err := lockDelivery(ctx, tx, a)
	if err != nil {
		return false, err
	}
	if !valid {
		return false, nil
	}
	state, next, err := domain.Advance(e.State, current.Kind, e.EmailReceived)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE actions SET status='delivered',lease_token=NULL,lease_until=NULL,delivered_at=clock_timestamp(),last_error='' WHERE id=$1`, current.ID)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE executions SET state=$2,updated_at=clock_timestamp(),
		expires_at=CASE WHEN $2='WAITING_FOR_EMAIL' THEN clock_timestamp()+($3::double precision*interval '1 second') ELSE NULL END WHERE id=$1`, e.ID, state, waitTTL.Seconds())
	if err != nil {
		return false, err
	}
	if next != nil {
		if err = schedule(ctx, tx, e, *next); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	slog.InfoContext(ctx, "workflow transition", "execution_id", e.ID, "action_id", current.ID, "from_state", e.State, "state", state)
	return true, nil
}

func (s *Store) Retry(ctx context.Context, a domain.Action, code string, delay time.Duration, permanent bool) (bool, error) {
	if delay < 0 {
		return false, domain.ErrInvalid
	}
	// The error class is intentionally restricted: raw provider errors may
	// contain private message bodies, addresses or credentials.
	if !domain.ValidIdentifier(code) || len(code) > 100 {
		code = "delivery_failure"
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	e, current, valid, err := lockDelivery(ctx, tx, a)
	if err != nil {
		return false, err
	}
	if !valid {
		return false, nil
	}
	status := "pending"
	if permanent {
		status = "failed"
	}
	_, err = tx.Exec(ctx, `UPDATE actions SET status=$2,lease_token=NULL,lease_until=NULL,last_error=$3,
		available_at=clock_timestamp()+($4::double precision*interval '1 second') WHERE id=$1`, current.ID, status, code, delay.Seconds())
	if err != nil {
		return false, err
	}
	if permanent {
		_, err = tx.Exec(ctx, `UPDATE executions SET state='FAILED',failure=$2,expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, e.ID, code)
		if err != nil {
			return false, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	if permanent {
		slog.WarnContext(ctx, "workflow transition", "execution_id", e.ID, "action_id", current.ID, "from_state", e.State, "state", domain.Failed, "error_class", code)
	}
	return true, nil
}

// Expire uses a bounded batch; every candidate is rechecked after acquiring its
// conversation lock. A valid DM and expiry cannot both win.
func (s *Store) Expire(ctx context.Context) (int64, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,tenant_id,account_id,platform,user_id FROM executions WHERE state='WAITING_FOR_EMAIL' AND expires_at<=clock_timestamp() ORDER BY expires_at,id LIMIT 100`)
	if err != nil {
		return 0, err
	}
	type candidate struct{ id, tenant, account, platform, user string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.tenant, &c.account, &c.platform, &c.user); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	var count int64
	for _, c := range candidates {
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			return count, err
		}
		if err = lockConversation(ctx, tx, c.tenant, c.account, c.platform, c.user); err != nil {
			tx.Rollback(ctx)
			return count, err
		}
		tag, err := tx.Exec(ctx, `UPDATE executions SET state='EXPIRED',updated_at=clock_timestamp() WHERE id=$1 AND state='WAITING_FOR_EMAIL' AND expires_at<=clock_timestamp()`, c.id)
		if err != nil {
			tx.Rollback(ctx)
			return count, err
		}
		if err = tx.Commit(ctx); err != nil {
			return count, err
		}
		count += tag.RowsAffected()
		if tag.RowsAffected() > 0 {
			slog.InfoContext(ctx, "workflow transition", "execution_id", c.id, "from_state", domain.WaitingEmail, "state", domain.Expired)
		}
	}
	return count, nil
}
