package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"blotato/internal/app"
	"blotato/internal/domain"
	"blotato/internal/httpapi"
)

const testToken = "test-credential-at-least-16"

func request(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func handler(repo app.Repository, demo bool) http.Handler {
	return httpapi.New(repo, nil, httpapi.Config{APIKeys: map[string]string{testToken: "tenant-a"}, DemoMode: demo, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

func TestPrivateRoutesRequireAuthenticationBeforeAccess(t *testing.T) {
	h := handler(nil, true)
	for _, path := range []string{"/v1/automations", "/v1/executions", "/v1/simulator/outbound", "/v1/events/comments"} {
		for _, token := range []string{"", "incorrect"} {
			if w := request(h, http.MethodGet, path, "", token); w.Code != http.StatusUnauthorized {
				t.Fatalf("%s: got %d", path, w.Code)
			}
		}
	}
	if w := request(h, http.MethodGet, "/healthz", "", ""); w.Code != http.StatusOK {
		t.Fatalf("liveness requires auth: %d", w.Code)
	}
}

func TestRejectsInvalidJSONBeforeCallingRepository(t *testing.T) {
	h := handler(nil, true)
	for _, tc := range []struct {
		name, path, body string
		status           int
	}{
		{"syntax", "/v1/automations", `{`, 400},
		{"unknown field", "/v1/automations", `{"unknown":true}`, 400},
		{"trailing body", "/v1/automations", `{} {}`, 400},
		{"array", "/v1/automations", `[]`, 400},
		{"null", "/v1/automations", `null`, 400},
		{"missing required fields", "/v1/events/comments", `{}`, 400},
		{"oversize", "/v1/events/messages", `{"text":"` + strings.Repeat("x", 70<<10) + `"}`, 413},
		{"oversize trailing whitespace", "/v1/automations", `{}` + strings.Repeat(" ", 70<<10), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(h, http.MethodPost, tc.path, tc.body, testToken)
			if w.Code != tc.status {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
		})
	}
	w := request(h, http.MethodPatch, "/v1/automations/id", `{}`, testToken)
	if w.Code != 400 {
		t.Fatalf("missing enabled accepted: %d", w.Code)
	}
	w = request(h, http.MethodPatch, "/v1/automations/id", `{"enabled":null}`, testToken)
	if w.Code != 400 {
		t.Fatalf("null enabled accepted: %d", w.Code)
	}
}

func TestRequiresJSONContentType(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/automations", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	handler(nil, true).ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestSimulatorControlsAreAbsentOutsideDemoMode(t *testing.T) {
	for _, tc := range []struct{ method, path, body string }{{http.MethodGet, "/v1/simulator/outbound", ""}, {http.MethodPost, "/v1/simulator/failures", `{}`}} {
		if w := request(handler(nil, false), tc.method, tc.path, tc.body, testToken); w.Code != 404 {
			t.Fatalf("development route exposed: %d", w.Code)
		}
	}
}

type scopedRepository struct {
	app.Repository
	received []string
	err      error
}

func (s *scopedRepository) ListAutomations(_ context.Context, tenant string, _ domain.Page) ([]domain.Automation, error) {
	s.received = append(s.received, tenant)
	return nil, s.err
}

func TestBearerIdentityDeterminesTenant(t *testing.T) {
	repo := &scopedRepository{}
	keys := map[string]string{testToken: "tenant-a", "another-credential-at-least-16": "tenant-b"}
	h := httpapi.New(repo, nil, httpapi.Config{APIKeys: keys})
	// Mutating the caller's map cannot change the running authentication table.
	keys[testToken] = "tenant-b"
	for _, token := range []string{testToken, "another-credential-at-least-16"} {
		w := request(h, http.MethodGet, "/v1/automations", "", token)
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
			t.Fatalf("got %d: %s", w.Code, w.Body.String())
		}
	}
	if strings.Join(repo.received, ",") != "tenant-a,tenant-b" {
		t.Fatalf("wrong tenant scope: %v", repo.received)
	}
}

func TestStorageErrorDetailsNeverReachResponseOrLogs(t *testing.T) {
	var logs bytes.Buffer
	repo := &scopedRepository{err: errors.New("email=private@example.com password=secret")}
	h := httpapi.New(repo, nil, httpapi.Config{APIKeys: map[string]string{testToken: "tenant-a"}, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	w := request(h, http.MethodGet, "/v1/automations", "", testToken)
	if w.Code != 500 {
		t.Fatalf("got %d", w.Code)
	}
	for _, s := range []string{w.Body.String(), logs.String()} {
		if strings.Contains(s, "private@example.com") || strings.Contains(s, "secret") {
			t.Fatal("sensitive error leaked")
		}
	}
}

func TestDomainErrorsHaveStableHTTPStatuses(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{domain.ErrInvalid, 400}, {domain.ErrNotFound, 404}, {domain.ErrConflict, 409}, {domain.ErrQuota, 429}} {
		w := request(handler(&scopedRepository{err: tc.err}, true), http.MethodGet, "/v1/automations", "", testToken)
		if w.Code != tc.status {
			t.Fatalf("%v: got %d", tc.err, w.Code)
		}
	}
}

func (s *scopedRepository) CreateAutomation(_ context.Context, _ string, _ domain.AutomationInput) (domain.Automation, error) {
	return domain.Automation{}, s.err
}

func TestStrictJSONAndMediaTypes(t *testing.T) {
	body := `{"platform":"instagram","account_id":"account-123","keyword":"pricing","comment_reply":"Reply","email_prompt":"Email?","link":"https://example.com"}`
	for _, raw := range []string{
		strings.TrimSuffix(body, "}") + `,"keyword":"other"}`,
		strings.TrimSuffix(body, "}") + `,"KEYWORD":"other"}`,
		strings.Replace(body, `"Reply"`, "\"bad\xfftext\"", 1),
		strings.Replace(body, `"Reply"`, `"bad\ud800text"`, 1),
	} {
		if w := request(handler(&scopedRepository{}, true), "POST", "/v1/automations", raw, testToken); w.Code != 400 {
			t.Errorf("accepted ambiguous/corrupt JSON: %d %s", w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", "/v1/automations", strings.NewReader(body))
	r.Header.Set("Content-Type", "Application/JSON; charset=utf-8")
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	handler(&scopedRepository{}, true).ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("valid media type: got %d", w.Code)
	}
}

func TestRateLimitIsSharedByTenantAndDoesNotGrowWithAttackers(t *testing.T) {
	repo := &scopedRepository{}
	h := httpapi.New(repo, nil, httpapi.Config{APIKeys: map[string]string{testToken: "a", "second-token": "a", "other-token": "b"}, RequestsPerSecond: 1, RequestBurst: 2})
	for _, key := range []string{testToken, "second-token"} {
		if w := request(h, "GET", "/v1/automations", "", key); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if w := request(h, "GET", "/v1/automations", "", testToken); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("tenant limit not enforced")
	}
	if w := request(h, "GET", "/v1/automations", "", "other-token"); w.Code != 200 {
		t.Fatal("another tenant affected")
	}
	if w := request(h, "GET", "/v1/automations", "", "invented"); w.Code != 401 {
		t.Fatal("invalid credential accepted")
	}
}
