// Package postgres implements durable atomic workflow use cases in PostgreSQL.
package postgres

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"blotato/internal/app"
	"blotato/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	Pool   *pgxpool.Pool
	limits storageLimits
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool, limits: storageLimits{automations: 1000, events: 100000, receipts: 200000}}
}

var _ app.Repository = (*Store)(nil)
var _ app.Platform = (*Store)(nil)
var _ app.Simulator = (*Store)(nil)

// Migrate applies the schema once, transactionally, with cross-process exclusion.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(728172963)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`); err != nil {
		return err
	}
	for version, file := range []string{"001_initial.up.sql", "002_quotas.up.sql"} {
		v := version + 1
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, v).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		body, readErr := migrations.ReadFile("migrations/" + file)
		if readErr != nil {
			return readErr
		}
		if _, err = tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %d: %w", v, err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES($1)`, v); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// Bootstrap provisions an already-connected account for the local demonstration.
func (s *Store) Bootstrap(ctx context.Context, tenant, account string) error {
	if tenant == "" || account == "" {
		return domain.ErrInvalid
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO tenants(id) VALUES ($1) ON CONFLICT DO NOTHING`, tenant); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO connected_accounts(tenant_id,id,platform) VALUES ($1,$2,'instagram') ON CONFLICT DO NOTHING`, tenant, account); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }

func mapError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	var p *pgconn.PgError
	if errors.As(err, &p) {
		if p.Code == "23503" {
			return domain.ErrNotFound
		}
		if p.Code == "23505" {
			return domain.ErrConflict
		}
		if p.Code == "23514" {
			return domain.ErrInvalid
		}
	}
	return err
}

type scanner interface{ Scan(...any) error }

const automationColumns = `id,tenant_id,account_id,platform,keyword,comment_reply,email_prompt,link,enabled,created_at`

func scanAutomation(row scanner) (a domain.Automation, err error) {
	err = row.Scan(&a.ID, &a.TenantID, &a.AccountID, &a.Platform, &a.Keyword, &a.CommentReply, &a.EmailPrompt, &a.Link, &a.Enabled, &a.CreatedAt)
	return a, mapError(err)
}

func (s *Store) CreateAutomation(ctx context.Context, tenant string, in domain.AutomationInput) (domain.Automation, error) {
	if err := domain.ValidateAutomation(in); err != nil {
		return domain.Automation{}, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Automation{}, err
	}
	defer tx.Rollback(ctx)
	if err = s.reserveQuota(ctx, tx, tenant, "automations"); err != nil {
		return domain.Automation{}, err
	}
	value, err := scanAutomation(tx.QueryRow(ctx, `INSERT INTO automations(id,tenant_id,account_id,platform,keyword,comment_reply,email_prompt,link) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+automationColumns, domain.NewID(), tenant, in.AccountID, in.Platform, in.Keyword, in.CommentReply, in.EmailPrompt, in.Link))
	if err != nil {
		return value, err
	}
	return value, tx.Commit(ctx)
}
func (s *Store) ListAutomations(ctx context.Context, tenant string, page domain.Page) ([]domain.Automation, error) {
	limit, afterTime, afterID, err := page.Values()
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+automationColumns+` FROM automations WHERE tenant_id=$1 AND ($2='' OR (created_at,id)>($3,$2)) ORDER BY created_at,id LIMIT $4`, tenant, afterID, afterTime, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Automation{}
	for rows.Next() {
		a, err := scanAutomation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}
func (s *Store) GetAutomation(ctx context.Context, tenant, id string) (domain.Automation, error) {
	return scanAutomation(s.Pool.QueryRow(ctx, `SELECT `+automationColumns+` FROM automations WHERE tenant_id=$1 AND id=$2`, tenant, id))
}
func (s *Store) SetEnabled(ctx context.Context, tenant, id string, enabled bool) (domain.Automation, error) {
	return scanAutomation(s.Pool.QueryRow(ctx, `UPDATE automations SET enabled=$3 WHERE tenant_id=$1 AND id=$2 RETURNING `+automationColumns, tenant, id, enabled))
}

// Length-prefixing prevents identifier separators from aliasing lock names.
func lockKey(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
	}
	return b.String()
}
func lock(ctx context.Context, tx pgx.Tx, parts ...string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey(parts...))
	return err
}
func lockConversation(ctx context.Context, tx pgx.Tx, tenant, account, platform, user string) error {
	return lock(ctx, tx, "conversation", tenant, account, platform, user)
}

const executionColumns = `id,tenant_id,automation_id,account_id,platform,user_id,comment_id,state,snapshot,email IS NOT NULL,created_at,updated_at,expires_at,failure`

func scanExecution(row scanner) (e domain.Execution, err error) {
	var snapshot []byte
	err = row.Scan(&e.ID, &e.TenantID, &e.AutomationID, &e.AccountID, &e.Platform, &e.UserID, &e.CommentID, &e.State, &snapshot, &e.EmailReceived, &e.CreatedAt, &e.UpdatedAt, &e.ExpiresAt, &e.Failure)
	if err != nil {
		return e, mapError(err)
	}
	err = json.Unmarshal(snapshot, &e.Snapshot)
	e.Snapshot.TenantID = e.TenantID
	return e, err
}

const actionColumns = `id,execution_id,tenant_id,account_id,platform,user_id,comment_id,kind,body,status,attempts,COALESCE(lease_token,''),available_at,lease_until,last_error`

func scanAction(row scanner) (a domain.Action, err error) {
	err = row.Scan(&a.ID, &a.ExecutionID, &a.TenantID, &a.AccountID, &a.Platform, &a.UserID, &a.CommentID, &a.Kind, &a.Body, &a.Status, &a.Attempts, &a.LeaseToken, &a.AvailableAt, &a.LeaseUntil, &a.LastError)
	return a, mapError(err)
}

func schedule(ctx context.Context, tx pgx.Tx, e domain.Execution, kind domain.ActionKind) error {
	body := e.Snapshot.CommentReply
	if kind == domain.RequestEmail {
		body = e.Snapshot.EmailPrompt
	}
	if kind == domain.SendLink {
		body = e.Snapshot.Link
	}
	_, err := tx.Exec(ctx, `INSERT INTO actions(id,execution_id,tenant_id,account_id,platform,user_id,comment_id,kind,body) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (execution_id,kind) DO NOTHING`, domain.NewID(), e.ID, e.TenantID, e.AccountID, e.Platform, e.UserID, e.CommentID, kind, body)
	return err
}

func (s *Store) GetExecution(ctx context.Context, tenant, id string) (domain.Execution, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.Execution{}, err
	}
	defer tx.Rollback(ctx)
	e, err := scanExecution(tx.QueryRow(ctx, `SELECT `+executionColumns+` FROM executions WHERE tenant_id=$1 AND id=$2`, tenant, id))
	if err != nil {
		return e, err
	}
	rows, err := tx.Query(ctx, `SELECT `+actionColumns+` FROM actions WHERE tenant_id=$1 AND execution_id=$2 ORDER BY created_at,id`, tenant, id)
	if err != nil {
		return e, err
	}
	e.Actions = []domain.Action{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			rows.Close()
			return e, err
		}
		e.Actions = append(e.Actions, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return e, err
	}
	return e, tx.Commit(ctx)
}
func (s *Store) ListExecutions(ctx context.Context, tenant string, page domain.Page) ([]domain.Execution, error) {
	limit, afterTime, afterID, err := page.Values()
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+executionColumns+` FROM executions WHERE tenant_id=$1 AND ($2='' OR (created_at,id)>($3,$2)) ORDER BY created_at,id LIMIT $4`, tenant, afterID, afterTime, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Execution{}
	for rows.Next() {
		e, err := scanExecution(rows)
		if err != nil {
			return nil, err
		}
		e.Actions = []domain.Action{}
		result = append(result, e)
	}
	return result, rows.Err()
}
