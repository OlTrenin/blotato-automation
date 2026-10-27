package integration_test

import (
	"errors"
	"testing"
	"time"

	"blotato/internal/domain"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestEventAndActionSchedulingRollbackTogether(t *testing.T) {
	f := setup(t)
	f.create(t)
	if _, err := f.pool.Exec(f.ctx, `ALTER TABLE inbound_events ADD CONSTRAINT injected_inbox_failure CHECK (outcome <> 'created')`); err != nil {
		t.Fatal(err)
	}
	c := comment("rollback", "user-001")
	if _, err := f.store.Ingest(f.ctx, tenant, "comment", c); err == nil {
		t.Fatal("injected inbox failure unexpectedly committed")
	}
	all, err := f.store.ListExecutions(f.ctx, tenant, domain.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatal("failed inbox insert left an execution behind")
	}
	var actions, events, receipts int
	if err = f.pool.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM actions),(SELECT count(*) FROM inbound_events),(SELECT count(*) FROM event_receipts)`).Scan(&actions, &events, &receipts); err != nil {
		t.Fatal(err)
	}
	if actions != 0 || events != 0 || receipts != 0 {
		t.Fatalf("partial commit actions=%d events=%d receipts=%d", actions, events, receipts)
	}
	if _, err = f.pool.Exec(f.ctx, `ALTER TABLE inbound_events DROP CONSTRAINT injected_inbox_failure`); err != nil {
		t.Fatal(err)
	}
	if r := f.ingest(t, "comment", c); r.Duplicate || r.Outcome != "created" {
		t.Fatalf("rolled-back receipt blocked retry: %+v", r)
	}
}

func TestDeliveryAcknowledgmentAndNextActionRollbackTogether(t *testing.T) {
	f := setup(t)
	f.create(t)
	r := f.ingest(t, "comment", comment("ack-rollback", "user-001"))
	a, err := f.store.Claim(f.ctx, time.Minute)
	if err != nil || a == nil {
		t.Fatalf("claim=%+v err=%v", a, err)
	}
	if err = f.store.Send(f.ctx, *a); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(f.ctx, `ALTER TABLE actions ADD CONSTRAINT injected_schedule_failure CHECK (kind <> 'request_email')`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.Delivered(f.ctx, *a, time.Hour); err == nil {
		t.Fatal("injected scheduling failure unexpectedly committed")
	}
	e := f.execution(t, r.ExecutionID)
	state(t, e, domain.InitialPending)
	if len(e.Actions) != 1 || e.Actions[0].Status != "processing" {
		t.Fatalf("partial acknowledgment commit: %+v", e.Actions)
	}
	if len(f.outbound(t)) != 1 {
		t.Fatal("external effect should remain independent of failed local transaction")
	}
	if _, err = f.pool.Exec(f.ctx, `ALTER TABLE actions DROP CONSTRAINT injected_schedule_failure`); err != nil {
		t.Fatal(err)
	}
	if accepted, err := f.store.Delivered(f.ctx, *a, time.Hour); err != nil || !accepted {
		t.Fatalf("ack retry=%v err=%v", accepted, err)
	}
	f.work(t, 1)
	state(t, f.execution(t, r.ExecutionID), domain.WaitingEmail)
	if len(f.outbound(t)) != 2 {
		t.Fatal("ack recovery duplicated remote reply")
	}
}

func postgresCode(t *testing.T, err error, want string) {
	t.Helper()
	var p *pgconn.PgError
	if !errors.As(err, &p) || p.Code != want {
		t.Fatalf("database error=%v, want SQLSTATE %s", err, want)
	}
}

func TestDatabaseEnforcesActionUniquenessAndCompletionRequiresEmail(t *testing.T) {
	f := setup(t)
	f.create(t)
	r := f.ingest(t, "comment", comment("constraints", "user-001"))
	_, err := f.pool.Exec(f.ctx, `INSERT INTO actions(id,execution_id,tenant_id,account_id,platform,user_id,comment_id,kind,body) SELECT $1,execution_id,tenant_id,account_id,platform,user_id,comment_id,kind,body FROM actions WHERE execution_id=$2`, domain.NewID(), r.ExecutionID)
	postgresCode(t, err, "23505")
	_, err = f.pool.Exec(f.ctx, `UPDATE executions SET state='COMPLETED' WHERE id=$1`, r.ExecutionID)
	postgresCode(t, err, "23514")
	state(t, f.execution(t, r.ExecutionID), domain.InitialPending)
}
