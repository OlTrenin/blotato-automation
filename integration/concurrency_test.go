package integration_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"blotato/internal/domain"
)

func TestConcurrentClaimsAreExclusiveAndSkipLockedRows(t *testing.T) {
	t.Run("same_action", func(t *testing.T) {
		f := setup(t)
		f.create(t)
		f.ingest(t, "comment", comment("claim", "user-001"))
		const workers = 16
		start := make(chan struct{})
		results := make(chan *domain.Action, workers)
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				a, err := f.store.Claim(f.ctx, time.Minute)
				results <- a
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		claimed := 0
		for a := range results {
			if a != nil {
				claimed++
				if a.Attempts != 1 || a.LeaseToken == "" {
					t.Fatalf("invalid claim: %+v", a)
				}
			}
		}
		if claimed != 1 {
			t.Fatalf("concurrent claims=%d, want 1", claimed)
		}
	})
	t.Run("locked_action_is_skipped", func(t *testing.T) {
		f := setup(t)
		f.create(t)
		f.ingest(t, "comment", comment("locked", "user-001"))
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(f.ctx)
		var id string
		if err = tx.QueryRow(f.ctx, `SELECT id FROM actions FOR UPDATE`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if a, err := f.store.Claim(f.ctx, time.Minute); err != nil || a != nil {
			t.Fatalf("locked action claim=%+v err=%v", a, err)
		}
		if err = tx.Rollback(f.ctx); err != nil {
			t.Fatal(err)
		}
		if a, err := f.store.Claim(f.ctx, time.Minute); err != nil || a == nil || a.ID != id {
			t.Fatalf("unlocked claim=%+v err=%v", a, err)
		}
	})
}

func TestTwoWorkerLoopsDeliverOrderedActionsOnce(t *testing.T) {
	f := setup(t)
	f.create(t)
	const sessions = 8
	ids := make([]string, sessions)
	for i := range sessions {
		ids[i] = f.ingest(t, "comment", comment(fmt.Sprintf("workers-%d", i), fmt.Sprintf("worker-user-%d", i))).ExecutionID
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		worker := newWorker(t, f.store, f.store)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range sessions * 3 {
				worked, err := worker.WorkOnce(f.ctx)
				if err != nil {
					errs <- err
					return
				}
				if !worked {
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		e := f.execution(t, id)
		state(t, e, domain.WaitingEmail)
		if len(e.Actions) != 2 {
			t.Fatalf("execution %s has %d actions", id, len(e.Actions))
		}
		for _, a := range e.Actions {
			if a.Attempts != 1 || a.Status != "delivered" {
				t.Fatalf("duplicate or incomplete worker attempt: %+v", a)
			}
		}
	}
	if got := len(f.outbound(t)); got != sessions*2 {
		t.Fatalf("worker effects=%d, want %d", got, sessions*2)
	}
}

func TestConcurrentTriggersShareOneConversation(t *testing.T) {
	f := setup(t)
	f.create(t)
	const count = 16
	start := make(chan struct{})
	results := make(chan domain.EventResult, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := f.store.Ingest(f.ctx, tenant, "comment", comment(fmt.Sprint(i), "same-user"))
			results <- r
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	created, suppressed := 0, 0
	var id string
	for r := range results {
		if id == "" {
			id = r.ExecutionID
		}
		if r.ExecutionID != id {
			t.Fatalf("ambiguous conversation results: %+v", r)
		}
		switch r.Outcome {
		case "created":
			created++
		case "suppressed_active_session":
			suppressed++
		default:
			t.Fatalf("unexpected trigger outcome %+v", r)
		}
	}
	if created != 1 || suppressed != count-1 {
		t.Fatalf("created=%d suppressed=%d", created, suppressed)
	}
	e := f.execution(t, id)
	if len(e.Actions) != 1 {
		t.Fatalf("concurrent triggers scheduled %d actions", len(e.Actions))
	}
}

func TestConcurrentEmailsScheduleOneFinalActionAndKeepFirstEmail(t *testing.T) {
	f := setup(t)
	f.create(t)
	r := f.ingest(t, "comment", comment("concurrent-emails", "user-001"))
	f.work(t, 2)
	const count = 16
	start := make(chan struct{})
	wins := make(chan string, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			email := fmt.Sprintf("user%d@example.com", i)
			result, err := f.store.Ingest(f.ctx, tenant, "message", message(fmt.Sprint(i), "user-001", email))
			if result.Outcome == "email_received" {
				wins <- email
			}
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(wins)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var winner string
	winCount := 0
	for email := range wins {
		winner = email
		winCount++
	}
	if winCount != 1 {
		t.Fatalf("valid email winners=%d, want 1", winCount)
	}
	e := f.execution(t, r.ExecutionID)
	state(t, e, domain.FinalPending)
	if len(e.Actions) != 3 {
		t.Fatalf("concurrent emails scheduled %d actions, want 3", len(e.Actions))
	}
	var stored string
	if err := f.pool.QueryRow(f.ctx, `SELECT email FROM executions WHERE id=$1`, r.ExecutionID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != winner {
		t.Fatal("a later valid email overwrote the first accepted email")
	}
	f.work(t, 1)
	state(t, f.execution(t, r.ExecutionID), domain.Completed)
	if len(f.outbound(t)) != 3 {
		t.Fatal("concurrent messages duplicated final delivery")
	}
}

func TestPromptAcknowledgmentRacingEmailCannotLoseResume(t *testing.T) {
	f := setup(t)
	f.create(t)
	for i := 0; i < 8; i++ {
		user := fmt.Sprintf("racing-user-%d", i)
		r := f.ingest(t, "comment", comment(fmt.Sprintf("race-%d", i), user))
		f.work(t, 1)
		prompt, err := f.store.Claim(f.ctx, time.Minute)
		if err != nil || prompt == nil || prompt.Kind != domain.RequestEmail {
			t.Fatalf("prompt claim=%+v err=%v", prompt, err)
		}
		if err = f.store.Send(f.ctx, *prompt); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			ok, err := f.store.Delivered(f.ctx, *prompt, time.Hour)
			if err == nil && !ok {
				err = fmt.Errorf("prompt acknowledgment rejected")
			}
			errs <- err
		}()
		go func() {
			<-start
			_, err := f.store.Ingest(f.ctx, tenant, "message", message(fmt.Sprintf("racing-%d", i), user, "hello@example.com"))
			errs <- err
		}()
		close(start)
		for j := 0; j < 2; j++ {
			if err = <-errs; err != nil {
				t.Fatal(err)
			}
		}
		e := f.execution(t, r.ExecutionID)
		state(t, e, domain.FinalPending)
		if len(e.Actions) != 3 || !e.EmailReceived {
			t.Fatalf("lost concurrent resume: %+v", e)
		}
		f.work(t, 1)
		state(t, f.execution(t, r.ExecutionID), domain.Completed)
	}
}
