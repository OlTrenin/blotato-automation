package integration_test

import (
	"encoding/json"
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
