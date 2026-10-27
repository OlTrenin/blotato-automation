// Package httpapi exposes the tenant-scoped operator and trusted ingress API.
// It deliberately does not accept unsigned public social-platform webhooks.
package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"blotato/internal/app"
	"blotato/internal/domain"
)

type Config struct {
	APIKeys           map[string]string // bearer credential -> tenant ID; copied at startup
	DemoMode          bool
	Logger            *slog.Logger
	RequestsPerSecond int
	RequestBurst      int
}

type server struct {
	repo   app.Repository
	sim    app.Simulator
	keys   map[[32]byte]string
	log    *slog.Logger
	limits map[string]*requestBucket
}

type tenantKey struct{}

func New(repo app.Repository, sim app.Simulator, cfg Config) http.Handler {
	s := &server{repo: repo, sim: sim, keys: make(map[[32]byte]string), log: cfg.Logger}
	if s.log == nil {
		s.log = slog.Default()
	}
	for token, tenant := range cfg.APIKeys {
		if token != "" && tenant != "" {
			s.keys[sha256.Sum256([]byte(token))] = tenant
		}
	}
	s.limits = make(map[string]*requestBucket)
	rate, burst := cfg.RequestsPerSecond, cfg.RequestBurst
	if rate <= 0 {
		rate = 100
	}
	if burst <= 0 {
		burst = 200
	}
	for _, tenant := range s.keys {
		s.limits[tenant] = newRequestBucket(rate, burst)
	}
	public := http.NewServeMux()
	public.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	public.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := s.repo.Ping(ctx); err != nil {
			write(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		write(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	private := http.NewServeMux()
	private.HandleFunc("POST /v1/automations", s.createAutomation)
	private.HandleFunc("GET /v1/automations", s.listAutomations)
	private.HandleFunc("GET /v1/automations/{id}", s.getAutomation)
	private.HandleFunc("PATCH /v1/automations/{id}", s.setEnabled)
	private.HandleFunc("POST /v1/events/comments", s.ingest("comment"))
	private.HandleFunc("POST /v1/events/messages", s.ingest("message"))
	private.HandleFunc("GET /v1/executions", s.listExecutions)
	private.HandleFunc("GET /v1/executions/{id}", s.getExecution)
	if cfg.DemoMode {
		private.HandleFunc("GET /v1/simulator/outbound", s.listOutbound)
		private.HandleFunc("POST /v1/simulator/failures", s.setFailure)
	}
	public.Handle("/v1/", s.authenticate(private))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		public.ServeHTTP(w, r)
	})
}

func (s *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := r.Header.Get("Authorization")
		credential, ok := strings.CutPrefix(value, "Bearer ")
		tenant, known := s.keys[sha256.Sum256([]byte(credential))]
		if !ok || credential == "" || !known {
			w.Header().Set("WWW-Authenticate", `Bearer realm="blotato"`)
			write(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		if !s.limits[tenant].allow() {
			w.Header().Set("Retry-After", "1")
			write(w, http.StatusTooManyRequests, map[string]string{"error": "request rate exceeded"})
			return
		}
		ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), tenantKey{}, tenant), 10*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func tenant(r *http.Request) string { return r.Context().Value(tenantKey{}).(string) }

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
		write(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			write(w, 413, map[string]string{"error": "JSON body exceeds 64 KiB"})
		} else {
			write(w, 400, map[string]string{"error": "invalid JSON body"})
		}
		return false
	}
	if !utf8.Valid(body) || !strictJSON(body) {
		write(w, 400, map[string]string{"error": "invalid or ambiguous JSON body"})
		return false
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			write(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "JSON body exceeds 64 KiB"})
		} else {
			write(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		}
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			write(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "JSON body exceeds 64 KiB"})
		} else {
			write(w, http.StatusBadRequest, map[string]string{"error": "exactly one JSON object required"})
		}
		return false
	}
	return true
}

func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *server) failure(w http.ResponseWriter, r *http.Request, err error) {
	status, message := http.StatusInternalServerError, "internal error"
	switch {
	case errors.Is(err, domain.ErrQuota):
		status, message = http.StatusTooManyRequests, "tenant storage quota exceeded"
	case errors.Is(err, domain.ErrInvalid):
		status, message = http.StatusBadRequest, "invalid input"
	case errors.Is(err, domain.ErrNotFound):
		status, message = http.StatusNotFound, "not found"
	case errors.Is(err, domain.ErrConflict):
		status, message = http.StatusConflict, "identity reused with different content"
	default:
		s.log.ErrorContext(r.Context(), "request failed", "method", r.Method, "route", r.Pattern, "error_class", "storage_error")
	}
	write(w, status, map[string]string{"error": message})
}

func validID(w http.ResponseWriter, r *http.Request) bool {
	if !domain.ValidIdentifier(r.PathValue("id")) {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid resource ID"})
		return false
	}
	return true
}

func (s *server) createAutomation(w http.ResponseWriter, r *http.Request) {
	var in domain.AutomationInput
	if !decode(w, r, &in) {
		return
	}
	if err := domain.ValidateAutomation(in); err != nil {
		s.failure(w, r, err)
		return
	}
	value, err := s.repo.CreateAutomation(r.Context(), tenant(r), in)
	if err != nil {
		s.failure(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/automations/"+value.ID)
	write(w, http.StatusCreated, value)
}
func (s *server) listAutomations(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}

	value, err := s.repo.ListAutomations(r.Context(), tenant(r), page)
	if err != nil {
		s.failure(w, r, err)
		return
	}
	if value == nil {
		value = []domain.Automation{}
	}
	if len(value) == page.Limit {
		last := value[len(value)-1]
		w.Header().Set("X-Next-Cursor", domain.NextCursor(last.CreatedAt, last.ID))
	}
	write(w, http.StatusOK, value)
}
func (s *server) getAutomation(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) {
		return
	}
	value, err := s.repo.GetAutomation(r.Context(), tenant(r), r.PathValue("id"))
	if err != nil {
		s.failure(w, r, err)
		return
	}
	write(w, http.StatusOK, value)
}
func (s *server) setEnabled(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) {
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Enabled == nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "enabled boolean required"})
		return
	}
	value, err := s.repo.SetEnabled(r.Context(), tenant(r), r.PathValue("id"), *in.Enabled)
	if err != nil {
		s.failure(w, r, err)
		return
	}
	write(w, http.StatusOK, value)
}
func (s *server) ingest(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in domain.Event
		if !decode(w, r, &in) {
			return
		}
		if err := domain.ValidateEvent(kind, in); err != nil {
			s.failure(w, r, err)
			return
		}
		value, err := s.repo.Ingest(r.Context(), tenant(r), kind, in)
		if err != nil {
			s.failure(w, r, err)
			return
		}
		s.log.InfoContext(r.Context(), "event committed", "tenant_id", tenant(r), "event_id", in.EventID, "execution_id", value.ExecutionID, "kind", kind, "outcome", value.Outcome, "duplicate", value.Duplicate)
		write(w, http.StatusOK, value)
	}
}
func (s *server) listExecutions(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}

	value, err := s.repo.ListExecutions(r.Context(), tenant(r), page)
	if err != nil {
		s.failure(w, r, err)
		return
	}
	if value == nil {
		value = []domain.Execution{}
	}
	if len(value) == page.Limit {
		last := value[len(value)-1]
		w.Header().Set("X-Next-Cursor", domain.NextCursor(last.CreatedAt, last.ID))
	}
	write(w, http.StatusOK, value)
}
func (s *server) getExecution(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) {
		return
	}
	value, err := s.repo.GetExecution(r.Context(), tenant(r), r.PathValue("id"))
	if err != nil {
		s.failure(w, r, err)
		return
	}
	write(w, http.StatusOK, value)
}
func (s *server) listOutbound(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}

	value, err := s.sim.ListOutbound(r.Context(), tenant(r), page)
	if err != nil {
		s.failure(w, r, err)
		return
	}
	if value == nil {
		value = []domain.Outbound{}
	}
	if len(value) == page.Limit {
		last := value[len(value)-1]
		w.Header().Set("X-Next-Cursor", domain.NextCursor(last.CreatedAt, last.ID))
	}
	write(w, http.StatusOK, value)
}
func (s *server) setFailure(w http.ResponseWriter, r *http.Request) {
	var in domain.FailurePlan
	if !decode(w, r, &in) {
		return
	}
	if (in.Kind != domain.ReplyComment && in.Kind != domain.RequestEmail && in.Kind != domain.SendLink) || (in.Mode != "transient" && in.Mode != "permanent" && in.Mode != "after_success") || in.Count < 0 || in.Count > 20 {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid failure plan"})
		return
	}
	if err := s.sim.SetFailure(r.Context(), tenant(r), in); err != nil {
		s.failure(w, r, err)
		return
	}
	write(w, http.StatusOK, map[string]string{"status": "configured"})
}

func readPage(w http.ResponseWriter, r *http.Request) (domain.Page, bool) {
	p := domain.Page{Limit: domain.MaxPageSize, After: r.URL.Query().Get("after")}
	if values, ok := r.URL.Query()["limit"]; ok {
		n, err := strconv.Atoi(values[0])
		if err != nil || len(values) != 1 || n < 1 || n > domain.MaxPageSize {
			write(w, 400, map[string]string{"error": "limit must be 1-100"})
			return p, false
		}
		p.Limit = n
	}
	if len(r.URL.Query()["after"]) > 1 {
		write(w, 400, map[string]string{"error": "one cursor required"})
		return p, false
	}
	if _, _, _, err := p.Values(); err != nil {
		write(w, 400, map[string]string{"error": "invalid cursor"})
		return p, false
	}
	return p, true
}
