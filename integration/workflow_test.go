package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"blotato/internal/app"
	"blotato/internal/domain"
	"blotato/internal/httpapi"
	"blotato/internal/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const tenant = "tenant-a"
const token = "tenant-a-test-token-123456789"

type fixture struct {
	ctx    context.Context
	store  *postgres.Store
	pool   *pgxpool.Pool
	server *httptest.Server
	worker *app.Worker
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is unset; real PostgreSQL integration tests were not executed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "qa_" + domain.NewID()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 32
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, e := admin.Exec(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); e != nil {
			t.Errorf("cleanup schema: %v", e)
		}
		admin.Close()
	})
	if err = postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	s := postgres.New(pool)
	if err = s.Bootstrap(ctx, tenant, "account-123"); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := &fixture{ctx: ctx, store: s, pool: pool}
	f.server = httptest.NewServer(httpapi.New(s, s, httpapi.Config{APIKeys: map[string]string{token: tenant, "tenant-b-test-token-123456789": "tenant-b"}, DemoMode: true, Logger: logger}))
	t.Cleanup(f.server.Close)
	f.worker = newWorker(t, s, s)
	return f
}

func newWorker(t *testing.T, s app.Repository, p app.Platform) *app.Worker {
	t.Helper()
	cfg := app.DefaultWorkerConfig()
	cfg.LeaseDuration = 2 * time.Second
	cfg.SendTimeout = 100 * time.Millisecond
	cfg.PollInterval = time.Millisecond
	cfg.BaseBackoff = time.Millisecond
	cfg.MaxBackoff = 5 * time.Millisecond
	cfg.WaitTTL = time.Hour
	cfg.MaxAttempts = 3
	w, err := app.NewWorker(s, p, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (f *fixture) request(t *testing.T, method, path, credential string, body any, status int, out any) []byte {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequestWithContext(f.ctx, method, f.server.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Content-Type", "application/json")
	res, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != status {
		t.Fatalf("%s %s status=%d, want=%d: %s", method, path, res.StatusCode, status, data)
	}
	if out != nil {
		if err = json.Unmarshal(data, out); err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
	}
	return data
}

func automation() domain.AutomationInput {
	return domain.AutomationInput{AccountID: "account-123", Platform: "instagram", Keyword: "pricing", CommentReply: "Sent you a DM!", EmailPrompt: "What is your email address?", Link: "https://example.com/pricing"}
}
func comment(id, user string) domain.Event {
	return domain.Event{EventID: "comment-event-" + id, Platform: "instagram", AccountID: "account-123", PostID: "post-456", CommentID: "comment-" + id, UserID: user, Text: "pricing"}
}
func message(id, user, text string) domain.Event {
	return domain.Event{EventID: "message-event-" + id, Platform: "instagram", AccountID: "account-123", MessageID: "message-" + id, UserID: user, Text: text}
}
func (f *fixture) create(t *testing.T) domain.Automation {
	t.Helper()
	a, err := f.store.CreateAutomation(f.ctx, tenant, automation())
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func (f *fixture) ingest(t *testing.T, kind string, e domain.Event) domain.EventResult {
	t.Helper()
	r, err := f.store.Ingest(f.ctx, tenant, kind, e)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func (f *fixture) execution(t *testing.T, id string) domain.Execution {
	t.Helper()
	e, err := f.store.GetExecution(f.ctx, tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func (f *fixture) work(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		worked, err := f.worker.WorkOnce(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			t.Fatalf("no eligible work at iteration %d/%d", i, n)
		}
	}
}
func (f *fixture) outbound(t *testing.T) []domain.Outbound {
	t.Helper()
	out, err := f.store.ListOutbound(f.ctx, tenant, domain.Page{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func state(t *testing.T, e domain.Execution, want domain.State) {
	t.Helper()
	if e.State != want {
		t.Fatalf("execution %s state=%s want=%s", e.ID, e.State, want)
	}
}

func TestHTTPFullAutomationAndDuplicateReplay(t *testing.T) {
	f := setup(t)
	var a domain.Automation
	f.request(t, http.MethodPost, "/v1/automations", token, automation(), http.StatusCreated, &a)
	if a.ID == "" {
		t.Fatal("automation ID missing")
	}
	f.request(t, http.MethodGet, "/v1/automations/"+a.ID, token, nil, http.StatusOK, &a)
	var created domain.EventResult
	c := comment("001", "user-001")
	f.request(t, http.MethodPost, "/v1/events/comments", token, c, http.StatusOK, &created)
	state(t, f.execution(t, created.ExecutionID), domain.InitialPending)
	f.work(t, 2)
	state(t, f.execution(t, created.ExecutionID), domain.WaitingEmail)
	out := f.outbound(t)
	if len(out) != 2 || out[0].Kind != domain.ReplyComment || out[1].Kind != domain.RequestEmail {
		t.Fatalf("initial outbound order: %+v", out)
	}
	f.request(t, http.MethodPost, "/v1/events/messages", token, message("invalid", "user-001", "not an email"), http.StatusOK, nil)
	state(t, f.execution(t, created.ExecutionID), domain.WaitingEmail)
	if len(f.outbound(t)) != 2 {
		t.Fatal("invalid email sent a link")
	}
	m := message("valid", "user-001", "My email is hello@example.com")
	f.request(t, http.MethodPost, "/v1/events/messages", token, m, http.StatusOK, nil)
	state(t, f.execution(t, created.ExecutionID), domain.FinalPending)
	f.work(t, 1)
	var completed domain.Execution
	body := f.request(t, http.MethodGet, "/v1/executions/"+created.ExecutionID, token, nil, http.StatusOK, &completed)
	state(t, completed, domain.Completed)
	if bytes.Contains(body, []byte("hello@example.com")) {
		t.Fatal("inspection leaked captured email")
	}
	out = f.outbound(t)
	if len(out) != 3 || out[2].Kind != domain.SendLink || out[2].Body != a.Link {
		t.Fatalf("final outbound: %+v", out)
	}
	for _, tc := range []struct {
		path string
		e    domain.Event
	}{{"comments", c}, {"messages", m}} {
		var replay domain.EventResult
		f.request(t, http.MethodPost, "/v1/events/"+tc.path, token, tc.e, http.StatusOK, &replay)
		if !replay.Duplicate || replay.ExecutionID != created.ExecutionID {
			t.Fatalf("replay=%+v", replay)
		}
	}
	if worked, err := f.worker.WorkOnce(f.ctx); err != nil || worked {
		t.Fatalf("duplicate scheduled more work: %v %v", worked, err)
	}
	var list []domain.Execution
	f.request(t, http.MethodGet, "/v1/executions", token, nil, http.StatusOK, &list)
	if len(list) != 1 || len(f.outbound(t)) != 3 {
		t.Fatal("duplicate created extra execution or delivery")
	}
}
