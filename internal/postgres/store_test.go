package postgres

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"blotato/internal/domain"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL required for real PostgreSQL tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_store_" + domain.NewID()
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	if err = Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	s := New(pool)
	if err = s.Bootstrap(ctx, "tenant-a", "account-123"); err != nil {
		t.Fatal(err)
	}
	return s
}

func createExecution(t *testing.T, s *Store, user string) domain.EventResult {
	t.Helper()
	ctx := context.Background()
	_, err := s.CreateAutomation(ctx, "tenant-a", domain.AutomationInput{AccountID: "account-123", Platform: "instagram", Keyword: "pricing", CommentReply: "Sent you a DM!", EmailPrompt: "What is your email?", Link: "https://example.com/pricing"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Ingest(ctx, "tenant-a", "comment", domain.Event{EventID: domain.NewID(), Platform: "instagram", AccountID: "account-123", PostID: "post-1", CommentID: domain.NewID(), UserID: user, Text: "pricing"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ExecutionID == "" {
		t.Fatalf("no execution: %+v", r)
	}
	return r
}

func TestMigrationCanBeReappliedAndReversed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := Migrate(ctx, s.Pool); err != nil {
		t.Fatal(err)
	}
	down, err := migrations.ReadFile("migrations/001_initial.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	quotaDown, err := migrations.ReadFile("migrations/002_quotas.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(quotaDown)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(down)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, s.Pool); err != nil {
		t.Fatal(err)
	}
	if err = s.Bootstrap(ctx, "tenant-a", "account-123"); err != nil {
		t.Fatal(err)
	}
	createExecution(t, s, "user-1")
}

func TestDatabaseEnforcesTenantAndActiveConversationConstraints(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := createExecution(t, s, "user-1")
	if err := s.Bootstrap(ctx, "tenant-b", "account-123"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Pool.Exec(ctx, `UPDATE executions SET tenant_id='tenant-b' WHERE id=$1`, r.ExecutionID)
	var p *pgconn.PgError
	if !errors.As(err, &p) || p.Code != "23503" {
		t.Fatalf("expected cross-tenant foreign-key violation, got %v", err)
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO executions(id,tenant_id,automation_id,account_id,platform,user_id,comment_id,state,snapshot) SELECT $2,tenant_id,automation_id,account_id,platform,user_id,'other-comment',state,snapshot FROM executions WHERE id=$1`, r.ExecutionID, domain.NewID())
	if !errors.As(err, &p) || p.Code != "23505" {
		t.Fatalf("expected active-conversation unique violation, got %v", err)
	}
}

func TestQuotasAreAtomicAndReplaysRemainAvailable(t *testing.T) {
	s := testStore(t)
	s.limits = storageLimits{automations: 1, events: 1, receipts: 2}
	ctx := context.Background()
	input := domain.AutomationInput{AccountID: "account-123", Platform: "instagram", Keyword: "pricing", CommentReply: "Reply", EmailPrompt: "Email?", Link: "https://example.com"}
	if _, err := s.CreateAutomation(ctx, "tenant-a", input); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAutomation(ctx, "tenant-a", input); !errors.Is(err, domain.ErrQuota) {
		t.Fatalf("automation quota: %v", err)
	}
	event := domain.Event{EventID: "one", Platform: "instagram", AccountID: "account-123", UserID: "user", PostID: "post", CommentID: "one", Text: "unmatched"}
	if _, err := s.Ingest(ctx, "tenant-a", "comment", event); err != nil {
		t.Fatal(err)
	}
	if result, err := s.Ingest(ctx, "tenant-a", "comment", event); err != nil || !result.Duplicate {
		t.Fatalf("replay quota: %v", err)
	}
	event.EventID = "alias"
	if result, err := s.Ingest(ctx, "tenant-a", "comment", event); err != nil || !result.Duplicate {
		t.Fatalf("alias quota: %v", err)
	}
	event.EventID = "excess-alias"
	if _, err := s.Ingest(ctx, "tenant-a", "comment", event); !errors.Is(err, domain.ErrQuota) {
		t.Fatalf("receipt quota: %v", err)
	}
	event.EventID = "new"
	event.CommentID = "new"
	if _, err := s.Ingest(ctx, "tenant-a", "comment", event); !errors.Is(err, domain.ErrQuota) {
		t.Fatalf("event quota: %v", err)
	}
	event.EventID = "one"
	event.CommentID = "one"
	if result, err := s.Ingest(ctx, "tenant-a", "comment", event); err != nil || !result.Duplicate {
		t.Fatalf("failed write corrupted replay: %v", err)
	}
}

func TestConcurrentQuotaAdmissionAcrossStores(t *testing.T) {
	first := testStore(t)
	second := New(first.Pool)
	first.limits.automations = 2
	second.limits.automations = 2
	var accepted atomic.Int32
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			store := first
			if i%2 == 0 {
				store = second
			}
			_, err := store.CreateAutomation(context.Background(), "tenant-a", domain.AutomationInput{AccountID: "account-123", Platform: "instagram", Keyword: "pricing", CommentReply: "Reply", EmailPrompt: "Email?", Link: "https://example.com"})
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, domain.ErrQuota) {
				t.Errorf("admission: %v", err)
			}
		}(i)
	}
	group.Wait()
	if accepted.Load() != 2 {
		t.Fatalf("quota overshot: %d", accepted.Load())
	}
}

func TestQuotaMigrationBackfillsExistingDataWithoutResettingUsage(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	input := domain.AutomationInput{AccountID: "account-123", Platform: "instagram", Keyword: "pricing", CommentReply: "Reply", EmailPrompt: "Email?", Link: "https://example.com"}
	for i := 0; i < 2; i++ {
		if _, err := s.CreateAutomation(ctx, "tenant-a", input); err != nil {
			t.Fatal(err)
		}
	}
	event := domain.Event{EventID: "one", Platform: "instagram", AccountID: "account-123", UserID: "u", PostID: "p", CommentID: "c", Text: "unmatched"}
	if _, err := s.Ingest(ctx, "tenant-a", "comment", event); err != nil {
		t.Fatal(err)
	}
	event.EventID = "alias"
	if _, err := s.Ingest(ctx, "tenant-a", "comment", event); err != nil {
		t.Fatal(err)
	}
	// Restore the previous schema while retaining its actual business data.
	down, err := migrations.ReadFile("migrations/002_quotas.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, string(down)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = Migrate(ctx, s.Pool); err != nil {
			t.Fatal(err)
		}
	}
	s.limits = storageLimits{automations: 2, events: 1, receipts: 2}
	if _, err = s.CreateAutomation(ctx, "tenant-a", input); !errors.Is(err, domain.ErrQuota) {
		t.Fatalf("backfilled automation quota: %v", err)
	}
	if result, err := s.Ingest(ctx, "tenant-a", "comment", event); err != nil || !result.Duplicate {
		t.Fatalf("historical replay: %v", err)
	}
	event.EventID = "another-alias"
	if _, err = s.Ingest(ctx, "tenant-a", "comment", event); !errors.Is(err, domain.ErrQuota) {
		t.Fatalf("backfilled receipts: %v", err)
	}
	event.CommentID = "another-comment"
	if _, err = s.Ingest(ctx, "tenant-a", "comment", event); !errors.Is(err, domain.ErrQuota) {
		t.Fatalf("backfilled canonical events: %v", err)
	}
}

func TestUpgradePreservesMatchingBeyondNewAutomationQuota(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	down, err := migrations.ReadFile("migrations/002_quotas.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, string(down)); err != nil {
		t.Fatal(err)
	}
	// A previous version allowed more than the new quota. Existing definitions
	// must keep working after upgrade even though new admissions are blocked.
	_, err = s.Pool.Exec(ctx, `INSERT INTO automations(id,tenant_id,account_id,platform,keyword,comment_reply,email_prompt,link,created_at)
      SELECT 'legacy-'||n,'tenant-a','account-123','instagram',CASE WHEN n=1001 THEN 'needle' ELSE 'other' END,
        'Reply','Email?','https://example.com',timestamptz '2026-01-01' + n * interval '1 second' FROM generate_series(1,1001) n`)
	if err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, s.Pool); err != nil {
		t.Fatal(err)
	}
	result, err := s.Ingest(ctx, "tenant-a", "comment", domain.Event{EventID: "legacy-event", Platform: "instagram", AccountID: "account-123", UserID: "u", PostID: "p", CommentID: "c", Text: "needle"})
	if err != nil || result.Outcome != "created" {
		t.Fatalf("historical definition lost: %+v, %v", result, err)
	}
	execution, err := s.GetExecution(ctx, "tenant-a", result.ExecutionID)
	if err != nil || execution.AutomationID != "legacy-1001" {
		t.Fatalf("wrong match: %+v, %v", execution, err)
	}
}
