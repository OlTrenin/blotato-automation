package integration_test

import (
	"testing"
	"time"

	"blotato/internal/domain"
	"blotato/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func (f *fixture) makeEligible(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE actions SET available_at=clock_timestamp()-interval '1 second' WHERE status='pending'`); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) abandonLease(t *testing.T, id string) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE actions SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerRecoversTemporaryAndAmbiguousProviderFailures(t *testing.T) {
	for _, mode := range []string{"transient", "after_success"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			f.create(t)
			if err := f.store.SetFailure(f.ctx, tenant, domain.FailurePlan{Kind: domain.ReplyComment, Mode: mode, Count: 1}); err != nil {
				t.Fatal(err)
			}
			r := f.ingest(t, "comment", comment("retry", "user-001"))
			f.work(t, 1)
			e := f.execution(t, r.ExecutionID)
			state(t, e, domain.InitialPending)
			if e.Actions[0].Status != "pending" || e.Actions[0].Attempts != 1 {
				t.Fatalf("failed attempt not durably retried: %+v", e.Actions)
			}
			wantEffects := 0
			if mode == "after_success" {
				wantEffects = 1
			}
			if got := len(f.outbound(t)); got != wantEffects {
				t.Fatalf("ambiguous boundary effects=%d, want=%d", got, wantEffects)
			}
			f.makeEligible(t)
			f.work(t, 2)
			state(t, f.execution(t, r.ExecutionID), domain.WaitingEmail)
			if got := len(f.outbound(t)); got != 2 {
				t.Fatalf("retry produced %d effects, want 2", got)
			}
			if got := f.execution(t, r.ExecutionID).Actions[0].Attempts; got != 2 {
				t.Fatalf("attempt count=%d", got)
			}
		})
	}
}

func TestDeliveryFailuresAfterEmailCapture(t *testing.T) {
	for _, kind := range []domain.ActionKind{domain.RequestEmail, domain.SendLink} {
		for _, mode := range []string{"transient", "after_success", "permanent"} {
			t.Run(string(kind)+"/"+mode, func(t *testing.T) {
				f := setup(t)
				f.create(t)
				r := f.ingest(t, "comment", comment("delivery-failure", "user-001"))
				pendingState := domain.InitialPending
				actionIndex := 1
				if kind == domain.RequestEmail {
					f.ingest(t, "message", message("early-email", "user-001", "hello@example.com"))
					f.work(t, 1)
				} else {
					f.work(t, 2)
					f.ingest(t, "message", message("final-email", "user-001", "hello@example.com"))
					pendingState = domain.FinalPending
					actionIndex = 2
				}
				if err := f.store.SetFailure(f.ctx, tenant, domain.FailurePlan{Kind: kind, Mode: mode, Count: 1}); err != nil {
					t.Fatal(err)
				}
				f.work(t, 1)
				e := f.execution(t, r.ExecutionID)
				if !e.EmailReceived || len(e.Actions) != actionIndex+1 {
					t.Fatalf("email lost or downstream action scheduled before acknowledgment: %+v", e)
				}
				action := e.Actions[actionIndex]
				if action.Kind != kind || action.Attempts != 1 {
					t.Fatalf("unexpected attempted action: %+v", action)
				}
				wantEffects := actionIndex
				if mode == "after_success" {
					wantEffects++
				}
				if got := len(f.outbound(t)); got != wantEffects {
					t.Fatalf("effects after failure=%d, want=%d", got, wantEffects)
				}
				if mode == "permanent" {
					state(t, e, domain.Failed)
					if action.Status != "failed" || e.Failure != "permanent_delivery_failure" {
						t.Fatalf("permanent failure not recorded: %+v", e)
					}
					if worked, err := f.worker.WorkOnce(f.ctx); err != nil || worked {
						t.Fatalf("terminal execution continued: worked=%v err=%v", worked, err)
					}
					return
				}
				state(t, e, pendingState)
				if action.Status != "pending" {
					t.Fatalf("failed attempt not queued for retry: %+v", action)
				}
				f.makeEligible(t)
				f.work(t, 1)
				e = f.execution(t, r.ExecutionID)
				retried := e.Actions[actionIndex]
				if retried.ID != action.ID || retried.Attempts != 2 || retried.Status != "delivered" {
					t.Fatalf("retry did not acknowledge the same logical action: %+v", retried)
				}
				if kind == domain.RequestEmail {
					state(t, e, domain.FinalPending)
					f.work(t, 1)
				}
				state(t, f.execution(t, r.ExecutionID), domain.Completed)
				out := f.outbound(t)
				if len(out) != 3 || out[0].Kind != domain.ReplyComment || out[1].Kind != domain.RequestEmail || out[2].Kind != domain.SendLink {
					t.Fatalf("retry duplicated or reordered effects: %+v", out)
				}
				if worked, err := f.worker.WorkOnce(f.ctx); err != nil || worked {
					t.Fatalf("completed execution continued: worked=%v err=%v", worked, err)
				}
			})
		}
	}
}

func TestCrashRecoveryBeforeSendAndAfterSuccessfulSend(t *testing.T) {
	for _, sendBeforeCrash := range []bool{false, true} {
		name := "before_send"
		if sendBeforeCrash {
			name = "after_send_before_ack"
		}
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.create(t)
			r := f.ingest(t, "comment", comment("crash", "user-001"))
			claimed, err := f.store.Claim(f.ctx, time.Minute)
			if err != nil || claimed == nil {
				t.Fatalf("claim=%+v err=%v", claimed, err)
			}
			if sendBeforeCrash {
				if err = f.store.Send(f.ctx, *claimed); err != nil {
					t.Fatal(err)
				}
			}
			// Fault injection models a killed process leaving a committed claim;
			// a new connection pool, repository and worker recover that durable row.
			f.abandonLease(t, claimed.ID)
			reopened, err := pgxpool.NewWithConfig(f.ctx, f.pool.Config())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(reopened.Close)
			resumed := postgres.New(reopened)
			worker := newWorker(t, resumed, resumed)
			if worked, err := worker.WorkOnce(f.ctx); err != nil || !worked {
				t.Fatalf("restart recovery=%v err=%v", worked, err)
			}
			if accepted, err := f.store.Delivered(f.ctx, *claimed, time.Hour); err != nil || accepted {
				t.Fatalf("stale acknowledgment accepted=%v err=%v", accepted, err)
			}
			if accepted, err := f.store.Retry(f.ctx, *claimed, "stale_worker", 0, true); err != nil || accepted {
				t.Fatalf("stale failure accepted=%v err=%v", accepted, err)
			}
			if worked, err := worker.WorkOnce(f.ctx); err != nil || !worked {
				t.Fatalf("resume prompt=%v err=%v", worked, err)
			}
			state(t, f.execution(t, r.ExecutionID), domain.WaitingEmail)
			if got := len(f.outbound(t)); got != 2 {
				t.Fatalf("recovered effects=%d, want 2", got)
			}
			if _, err = resumed.Ingest(f.ctx, tenant, "message", message("restart-email", "user-001", "hello@example.com")); err != nil {
				t.Fatal(err)
			}
			if worked, err := worker.WorkOnce(f.ctx); err != nil || !worked {
				t.Fatalf("restart final=%v err=%v", worked, err)
			}
			state(t, f.execution(t, r.ExecutionID), domain.Completed)
		})
	}
}

func TestPermanentFailureAndExpiredLastAttemptBecomeTerminal(t *testing.T) {
	t.Run("permanent_provider_error", func(t *testing.T) {
		f := setup(t)
		f.create(t)
		if err := f.store.SetFailure(f.ctx, tenant, domain.FailurePlan{Kind: domain.ReplyComment, Mode: "permanent", Count: 1}); err != nil {
			t.Fatal(err)
		}
		r := f.ingest(t, "comment", comment("permanent", "user-001"))
		f.work(t, 1)
		e := f.execution(t, r.ExecutionID)
		state(t, e, domain.Failed)
		if len(e.Actions) != 1 || e.Actions[0].Status != "failed" || e.Failure == "" {
			t.Fatalf("terminal failure invisible: %+v", e)
		}
		if len(f.outbound(t)) != 0 {
			t.Fatal("permanent error sent downstream actions")
		}
		if worked, err := f.worker.WorkOnce(f.ctx); err != nil || worked {
			t.Fatalf("terminal action reclaimed=%v err=%v", worked, err)
		}
	})
	t.Run("crash_on_last_attempt", func(t *testing.T) {
		f := setup(t)
		f.create(t)
		r := f.ingest(t, "comment", comment("last-attempt", "user-001"))
		a, err := f.store.Claim(f.ctx, time.Minute)
		if err != nil || a == nil {
			t.Fatalf("claim=%+v err=%v", a, err)
		}
		if _, err = f.pool.Exec(f.ctx, `UPDATE actions SET attempts=3,lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, a.ID); err != nil {
			t.Fatal(err)
		}
		f.work(t, 1)
		e := f.execution(t, r.ExecutionID)
		state(t, e, domain.Failed)
		if e.Failure != "attempts_exhausted" || e.Actions[0].Status != "failed" {
			t.Fatalf("abandoned final attempt stranded: %+v", e)
		}
		if len(f.outbound(t)) != 0 {
			t.Fatal("recovery exceeded maximum provider attempts")
		}
	})
	t.Run("finite_transient_attempts", func(t *testing.T) {
		f := setup(t)
		f.create(t)
		if err := f.store.SetFailure(f.ctx, tenant, domain.FailurePlan{Kind: domain.ReplyComment, Mode: "transient", Count: 20}); err != nil {
			t.Fatal(err)
		}
		r := f.ingest(t, "comment", comment("exhausted", "user-001"))
		for i := 0; i < 3; i++ {
			f.makeEligible(t)
			f.work(t, 1)
		}
		e := f.execution(t, r.ExecutionID)
		state(t, e, domain.Failed)
		if e.Actions[0].Attempts != 3 || e.Failure != "attempts_exhausted" {
			t.Fatalf("attempt policy: %+v", e)
		}
	})
}

func TestLateEmailExpiresWithoutWaitingForSweeper(t *testing.T) {
	f := setup(t)
	f.create(t)
	r := f.ingest(t, "comment", comment("expire", "user-001"))
	f.work(t, 2)
	if _, err := f.pool.Exec(f.ctx, `UPDATE executions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, r.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if late := f.ingest(t, "message", message("late", "user-001", "late@example.com")); late.Outcome != "unmatched" {
		t.Fatalf("late email revived session: %+v", late)
	}
	state(t, f.execution(t, r.ExecutionID), domain.Expired)
	if len(f.outbound(t)) != 2 {
		t.Fatal("expired session sent link")
	}
	newRun := f.ingest(t, "comment", comment("after-expiry", "user-001"))
	if newRun.ExecutionID == "" || newRun.ExecutionID == r.ExecutionID {
		t.Fatalf("expired active slot not released: %+v", newRun)
	}
	f.work(t, 2)
	if _, err := f.pool.Exec(f.ctx, `UPDATE executions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, newRun.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if n, err := f.store.Expire(f.ctx); err != nil || n != 1 {
		t.Fatalf("expiry sweep count=%d err=%v", n, err)
	}
	state(t, f.execution(t, newRun.ExecutionID), domain.Expired)
}
