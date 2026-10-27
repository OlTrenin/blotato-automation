package integration_test

import (
	"errors"
	"net/http"
	"testing"

	"blotato/internal/domain"
)

func TestEarlyEmailIsBufferedUntilInitialPromptDelivery(t *testing.T) {
	f := setup(t)
	f.create(t)
	r := f.ingest(t, "comment", comment("early", "user-001"))
	f.ingest(t, "message", message("early", "user-001", "hello@example.com"))
	e := f.execution(t, r.ExecutionID)
	state(t, e, domain.InitialPending)
	if !e.EmailReceived || len(e.Actions) != 1 || e.Actions[0].Kind != domain.ReplyComment {
		t.Fatalf("early email advanced delivery prematurely: %+v", e)
	}
	f.work(t, 1)
	state(t, f.execution(t, r.ExecutionID), domain.InitialPending)
	f.work(t, 1)
	state(t, f.execution(t, r.ExecutionID), domain.FinalPending)
	f.work(t, 1)
	state(t, f.execution(t, r.ExecutionID), domain.Completed)
	out := f.outbound(t)
	if len(out) != 3 || out[0].Kind != domain.ReplyComment || out[1].Kind != domain.RequestEmail || out[2].Kind != domain.SendLink {
		t.Fatalf("delivery order: %+v", out)
	}
}

func TestMessageBeforeTriggerIsNeverRetrospectivelyAttached(t *testing.T) {
	f := setup(t)
	f.create(t)
	old := message("before", "user-001", "old@example.com")
	if r := f.ingest(t, "message", old); r.Outcome != "unmatched" {
		t.Fatalf("pre-trigger outcome: %+v", r)
	}
	r := f.ingest(t, "comment", comment("after", "user-001"))
	f.work(t, 2)
	if replay := f.ingest(t, "message", old); !replay.Duplicate || replay.Outcome != "unmatched" {
		t.Fatalf("historical replay: %+v", replay)
	}
	state(t, f.execution(t, r.ExecutionID), domain.WaitingEmail)
	if f.execution(t, r.ExecutionID).EmailReceived {
		t.Fatal("historical email was attached")
	}
	if r := f.ingest(t, "message", message("other-user", "user-002", "other@example.com")); r.Outcome != "unmatched" {
		t.Fatalf("other user outcome: %+v", r)
	}
	state(t, f.execution(t, r.ExecutionID), domain.WaitingEmail)
	if err := f.store.Bootstrap(f.ctx, tenant, "account-other"); err != nil {
		t.Fatal(err)
	}
	other := message("other-account", "user-001", "other@example.com")
	other.AccountID = "account-other"
	if r := f.ingest(t, "message", other); r.Outcome != "unmatched" {
		t.Fatalf("other account outcome: %+v", r)
	}
	f.ingest(t, "message", message("new", "user-001", "new@example.com"))
	f.work(t, 1)
	state(t, f.execution(t, r.ExecutionID), domain.Completed)
}

func TestDeterministicAutomationAndActiveSessionSuppression(t *testing.T) {
	f := setup(t)
	oldest := f.create(t)
	secondInput := automation()
	secondInput.Link = "https://example.com/second"
	if _, err := f.store.CreateAutomation(f.ctx, tenant, secondInput); err != nil {
		t.Fatal(err)
	}
	irrelevant := comment("irrelevant", "user-001")
	irrelevant.Text = "prepricing"
	if r := f.ingest(t, "comment", irrelevant); r.Outcome != "unmatched" {
		t.Fatalf("irrelevant trigger: %+v", r)
	}
	r := f.ingest(t, "comment", comment("first", "user-001"))
	if e := f.execution(t, r.ExecutionID); e.AutomationID != oldest.ID {
		t.Fatalf("chose automation %s, expected oldest %s", e.AutomationID, oldest.ID)
	}
	suppressed := comment("second", "user-001")
	if s := f.ingest(t, "comment", suppressed); s.Outcome != "suppressed_active_session" || s.ExecutionID != r.ExecutionID {
		t.Fatalf("suppression: %+v", s)
	}
	// Disabling after creation must not cancel or change the existing snapshot.
	if _, err := f.store.SetEnabled(f.ctx, tenant, oldest.ID, false); err != nil {
		t.Fatal(err)
	}
	f.work(t, 2)
	f.ingest(t, "message", message("valid", "user-001", "hello@example.com"))
	f.work(t, 1)
	if e := f.execution(t, r.ExecutionID); e.Snapshot.Link != oldest.Link {
		t.Fatalf("snapshot changed: %+v", e.Snapshot)
	}
	if s := f.ingest(t, "comment", suppressed); !s.Duplicate || s.Outcome != "suppressed_active_session" {
		t.Fatalf("suppressed replay reopened: %+v", s)
	}
	newRun := f.ingest(t, "comment", comment("third", "user-001"))
	if e := f.execution(t, newRun.ExecutionID); e.AutomationID == oldest.ID {
		t.Fatal("disabled automation started a new execution")
	}
	all, err := f.store.ListExecutions(f.ctx, tenant, domain.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("execution count %d, want 2", len(all))
	}
}

func TestLogicalObjectDedupeAndImmutableEventIdentities(t *testing.T) {
	f := setup(t)
	f.create(t)
	original := comment("dedupe", "user-001")
	first := f.ingest(t, "comment", original)
	alias := original
	alias.EventID = "different-envelope-id"
	if r := f.ingest(t, "comment", alias); !r.Duplicate || r.ExecutionID != first.ExecutionID {
		t.Fatalf("logical duplicate: %+v", r)
	}
	for _, tc := range []domain.Event{
		func() domain.Event { e := original; e.Text = "changed"; return e }(),
		func() domain.Event { e := alias; e.UserID = "changed-user"; return e }(),
		func() domain.Event { e := original; e.EventID = "third-envelope-id"; e.Text = "changed"; return e }(),
	} {
		if _, err := f.store.Ingest(f.ctx, tenant, "comment", tc); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("changed identity error=%v", err)
		}
	}
	all, err := f.store.ListExecutions(f.ctx, tenant, domain.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || len(f.execution(t, first.ExecutionID).Actions) != 1 {
		t.Fatal("deduplication scheduled duplicate logical work")
	}
}

func TestTenantIsolationAndAccountOwnershipThroughHTTP(t *testing.T) {
	f := setup(t)
	f.create(t)
	r := f.ingest(t, "comment", comment("tenant-a", "same-social-user"))
	otherToken := "tenant-b-test-token-123456789"
	f.request(t, http.MethodGet, "/v1/executions/"+r.ExecutionID, otherToken, nil, http.StatusNotFound, nil)
	f.request(t, http.MethodGet, "/v1/executions", otherToken, nil, http.StatusOK, &[]domain.Execution{})
	f.request(t, http.MethodPost, "/v1/events/comments", otherToken, comment("unknown-account", "same-social-user"), http.StatusNotFound, nil)
	if err := f.store.Bootstrap(f.ctx, "tenant-b", "account-123"); err != nil {
		t.Fatal(err)
	}
	var otherAutomation domain.Automation
	f.request(t, http.MethodPost, "/v1/automations", otherToken, automation(), http.StatusCreated, &otherAutomation)
	var otherResult domain.EventResult
	f.request(t, http.MethodPost, "/v1/events/comments", otherToken, comment("tenant-a", "same-social-user"), http.StatusOK, &otherResult)
	if otherResult.ExecutionID == "" || otherResult.ExecutionID == r.ExecutionID {
		t.Fatalf("tenant sessions collided: %+v", otherResult)
	}
	f.request(t, http.MethodGet, "/v1/automations/"+otherAutomation.ID, token, nil, http.StatusNotFound, nil)
	f.request(t, http.MethodPost, "/v1/events/messages", otherToken, message("email", "same-social-user", "b@example.com"), http.StatusOK, nil)
	if f.execution(t, r.ExecutionID).EmailReceived {
		t.Fatal("other tenant's message resumed tenant-a workflow")
	}
}
