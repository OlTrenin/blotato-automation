package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"blotato/internal/domain"
)

func TestHTTPPaginationTraversesWithoutDuplicates(t *testing.T) {
	f := setup(t)
	for i := 0; i < 5; i++ {
		f.create(t)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=x", "after=broken", "limit=1&limit=2"} {
		f.request(t, "GET", "/v1/automations?"+query, token, nil, 400, nil)
	}
	after := ""
	seen := map[string]bool{}
	for page := 0; page < 3; page++ {
		req, err := http.NewRequest("GET", f.server.URL+"/v1/automations?limit=2&after="+after, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var values []domain.Automation
		if res.StatusCode != 200 {
			t.Fatalf("list status %d", res.StatusCode)
		}
		err = json.NewDecoder(res.Body).Decode(&values)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(values) > 2 {
			t.Fatal("unbounded response")
		}
		for _, a := range values {
			if seen[a.ID] {
				t.Fatal("duplicate across pages")
			}
			seen[a.ID] = true
		}
		after = res.Header.Get("X-Next-Cursor")
		if page < 2 && after == "" {
			t.Fatal("missing continuation")
		}
	}
	if len(seen) != 5 || after != "" {
		t.Fatalf("incomplete traversal: %d", len(seen))
	}
}

func TestPaginationTimestampTiesAndTenantIsolationForEveryList(t *testing.T) {
	f := setup(t)
	for i := 0; i < 3; i++ {
		f.create(t)
		f.ingest(t, "comment", comment(fmt.Sprint(i), fmt.Sprint(i)))
	}
	if err := f.store.Bootstrap(f.ctx, "tenant-b", "account-123"); err != nil {
		t.Fatal(err)
	}
	foreignAutomation, err := f.store.CreateAutomation(f.ctx, "tenant-b", automation())
	if err != nil {
		t.Fatal(err)
	}
	foreignExecution, err := f.store.Ingest(f.ctx, "tenant-b", "comment", comment("foreign", "foreign"))
	if err != nil {
		t.Fatal(err)
	}
	f.work(t, 8)
	// Timestamp ties are legal; the stable ID tie-breaker must prevent omissions.
	for _, table := range []string{"automations", "executions", "simulator_effects"} {
		if _, err := f.pool.Exec(f.ctx, "UPDATE "+table+" SET created_at='2026-01-01T00:00:00Z'"); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		path  string
		count int
	}{{"/v1/automations", 3}, {"/v1/executions", 3}, {"/v1/simulator/outbound", 6}} {
		t.Run(tc.path, func(t *testing.T) {
			after := ""
			seen := map[string]bool{}
			for page := 0; page < 10; page++ {
				req, err := http.NewRequestWithContext(f.ctx, "GET", f.server.URL+tc.path+"?limit=2&after="+after, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+token)
				response, err := f.server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				var items []struct {
					ID          string `json:"id"`
					ExecutionID string `json:"execution_id"`
				}
				err = json.NewDecoder(response.Body).Decode(&items)
				response.Body.Close()
				if err != nil || response.StatusCode != 200 {
					t.Fatalf("page: %v, status %d", err, response.StatusCode)
				}
				if len(items) > 2 {
					t.Fatal("unbounded page")
				}
				for _, item := range items {
					if seen[item.ID] {
						t.Fatal("duplicated ID")
					}
					if item.ID == foreignAutomation.ID || item.ID == foreignExecution.ExecutionID || item.ExecutionID == foreignExecution.ExecutionID {
						t.Fatal("foreign tenant leaked")
					}
					seen[item.ID] = true
				}
				after = response.Header.Get("X-Next-Cursor")
				if after == "" {
					break
				}
			}
			if after != "" || len(seen) != tc.count {
				t.Fatalf("pagination lost data: %d, expected %d", len(seen), tc.count)
			}
		})
	}
}
