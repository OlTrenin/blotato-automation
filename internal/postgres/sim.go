package postgres

import (
	"context"
	"errors"

	"blotato/internal/app"
	"blotato/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Send simulates a remote service in an independent transaction. The outbox
// transaction never includes this call. Its stable idempotency key is action.ID.
func (s *Store) Send(ctx context.Context, a domain.Action) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lock(ctx, tx, "simulated_action", a.ID); err != nil {
		return err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM simulator_effects WHERE action_id=$1)`, a.ID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return tx.Commit(ctx)
	}
	var mode string
	var remaining int
	err = tx.QueryRow(ctx, `SELECT mode,remaining FROM simulator_failures WHERE tenant_id=$1 AND kind=$2 FOR UPDATE`, a.TenantID, a.Kind).Scan(&mode, &remaining)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if remaining > 0 {
		if _, err = tx.Exec(ctx, `UPDATE simulator_failures SET remaining=remaining-1 WHERE tenant_id=$1 AND kind=$2`, a.TenantID, a.Kind); err != nil {
			return err
		}
		if mode != "after_success" {
			if err = tx.Commit(ctx); err != nil {
				return err
			}
			return &app.DeliveryError{Code: "simulated_" + mode, Permanent: mode == "permanent"}
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO simulator_effects(action_id,execution_id,tenant_id,account_id,user_id,comment_id,kind,body) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, a.ID, a.ExecutionID, a.TenantID, a.AccountID, a.UserID, a.CommentID, a.Kind, a.Body)
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if remaining > 0 && mode == "after_success" {
		return &app.DeliveryError{Code: "simulated_after_success"}
	}
	return nil
}

func (s *Store) ListOutbound(ctx context.Context, tenant string, page domain.Page) ([]domain.Outbound, error) {
	limit, afterTime, afterID, err := page.Values()
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT action_id,execution_id,account_id,user_id,comment_id,kind,body,created_at FROM simulator_effects WHERE tenant_id=$1 AND ($2='' OR (created_at,action_id)>($3,$2)) ORDER BY created_at,action_id LIMIT $4`, tenant, afterID, afterTime, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Outbound{}
	for rows.Next() {
		var o domain.Outbound
		if err = rows.Scan(&o.ID, &o.ExecutionID, &o.AccountID, &o.UserID, &o.CommentID, &o.Kind, &o.Body, &o.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, o)
	}
	return result, rows.Err()
}

func (s *Store) SetFailure(ctx context.Context, tenant string, p domain.FailurePlan) error {
	if p.Kind != domain.ReplyComment && p.Kind != domain.RequestEmail && p.Kind != domain.SendLink {
		return domain.ErrInvalid
	}
	if p.Mode != "transient" && p.Mode != "permanent" && p.Mode != "after_success" {
		return domain.ErrInvalid
	}
	if p.Count < 0 || p.Count > 20 {
		return domain.ErrInvalid
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO simulator_failures(tenant_id,kind,mode,remaining) VALUES($1,$2,$3,$4) ON CONFLICT(tenant_id,kind) DO UPDATE SET mode=EXCLUDED.mode,remaining=EXCLUDED.remaining`, tenant, p.Kind, p.Mode, p.Count)
	return mapError(err)
}
