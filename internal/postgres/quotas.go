package postgres

import (
	"context"

	"blotato/internal/domain"
	"github.com/jackc/pgx/v5"
)

type storageLimits struct{ automations, events, receipts int64 }

// Counters and writes commit together. A single conditional UPDATE serializes
// admissions across replicas without scanning an ever-growing tenant history.
func (s *Store) reserveQuota(ctx context.Context, tx pgx.Tx, tenant, resource string) error {
	var limit int64
	switch resource {
	case "automations":
		limit = s.limits.automations
	case "events":
		limit = s.limits.events
	case "receipts":
		limit = s.limits.receipts
	default:
		return domain.ErrInvalid
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tenant_usage(tenant_id) VALUES($1) ON CONFLICT DO NOTHING`, tenant); err != nil {
		return mapError(err)
	}
	// resource is exclusively one of the constants above, never request input.
	tag, err := tx.Exec(ctx, `UPDATE tenant_usage SET `+resource+`=`+resource+`+1 WHERE tenant_id=$1 AND `+resource+`<$2`, tenant, limit)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrQuota
	}
	return nil
}
