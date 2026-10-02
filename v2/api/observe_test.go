package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type logLine struct {
	Msg       string `json:"msg"`
	RequestID string `json:"request_id"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Route     string `json:"route"`
	Status    int    `json:"status"`
	Bytes     int64  `json:"bytes"`
}

type harness struct {
	handler http.Handler
	logs    *bytes.Buffer
	reg     *prometheus.Registry
	spans   *tracetest.SpanRecorder
}

func newHarness(t *testing.T, mux *http.ServeMux, quiet ...string) *harness {
	t.Helper()
	h := &harness{
		logs:  &bytes.Buffer{},
		reg:   prometheus.NewRegistry(),
		spans: tracetest.NewSpanRecorder(),
	}
	h.handler = Observe(Config{
		Logger:         slog.New(slog.NewJSONHandler(h.logs, nil)),
		Registerer:     h.reg,
		TracerProvider: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(h.spans)),
		QuietRoutes:    quiet,
	})(mux)
	return h
}

func (h *harness) serve(method, target string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func (h *harness) lines(t *testing.T) []logLine {
	t.Helper()
	var lines []logLine
	for _, raw := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		if raw == "" {
			continue
		}
		var l logLine
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("decoding log line %q: %s", raw, err)
		}
		lines = append(lines, l)
	}
	return lines
}

func (h *harness) count(method, path, status string) float64 {
	families, _ := h.reg.Gather()
	for _, mf := range families {
		if mf.GetName() != "http_request_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["method"] == method && labels["path"] == path && labels["status"] == status {
				return float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return 0
}

func TestObserveRecordsStatusHoweverItIsWritten(t *testing.T) {
	cases := []struct {
		title   string
		handler http.HandlerFunc
		status  int
		group   string
	}{
		{
			title:   "raw WriteHeader",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted) },
			status:  http.StatusAccepted,
			group:   "2XX",
		},
		{
			title:   "implicit 200 from Write",
			handler: func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) },
			status:  http.StatusOK,
			group:   "2XX",
		},
		{
			title:   "nothing written",
			handler: func(w http.ResponseWriter, r *http.Request) {},
			status:  http.StatusOK,
			group:   "2XX",
		},
		{
			title: "only the first WriteHeader counts",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusAccepted)
				w.WriteHeader(http.StatusInternalServerError)
			},
			status: http.StatusAccepted,
			group:  "2XX",
		},
		{
			title:   "Problem helper",
			handler: func(w http.ResponseWriter, r *http.Request) { Error(w, r, "bad payload", http.StatusBadRequest) },
			status:  http.StatusBadRequest,
			group:   "4XX",
		},
	}

	for _, c := range cases {
		t.Run(c.title, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.Handle("POST /push", c.handler)
			h := newHarness(t, mux)

			rec := h.serve("POST", "/push", http.Header{"X-Request-Id": {"req-1"}})

			if rec.Code != c.status {
				t.Fatalf("expected response %d, got %d", c.status, rec.Code)
			}
			lines := h.lines(t)
			if len(lines) != 1 {
				t.Fatalf("expected 1 log line, got %d", len(lines))
			}
			want := logLine{Msg: "request", RequestID: "req-1", Method: "POST", Path: "/push", Route: "/push", Status: c.status, Bytes: int64(rec.Body.Len())}
			if lines[0] != want {
				t.Errorf("expected log line %+v, got %+v", want, lines[0])
			}
			if got := h.count("POST", "/push", c.group); got != 1 {
				t.Errorf("expected 1 observation with status=%q, got %v", c.group, got)
			}
		})
	}
}

func TestObserveUnmatchedRoutes(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("POST /push", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h := newHarness(t, mux)

	if rec := h.serve("GET", "/does-not-exist", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	if rec := h.serve("GET", "/push", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}

	if got := h.count("GET", "unmatched", "4XX"); got != 2 {
		t.Errorf("expected 2 unmatched 4XX observations, got %v", got)
	}
	for _, l := range h.lines(t) {
		if l.Route != "unmatched" {
			t.Errorf("expected route %q, got %q", "unmatched", l.Route)
		}
	}
}

func TestObserveQuietRoutesAreMeasuredButNotLogged(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {})
	h := newHarness(t, mux, "GET /healthz")

	h.serve("GET", "/healthz", nil)

	if lines := h.lines(t); len(lines) != 0 {
		t.Errorf("expected no log lines, got %+v", lines)
	}
	if got := h.count("GET", "/healthz", "2XX"); got != 1 {
		t.Errorf("expected 1 observation, got %v", got)
	}
}

func TestObserveExposesRequestState(t *testing.T) {
	var id string
	var before, after int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /push", func(w http.ResponseWriter, r *http.Request) {
		id = RequestID(r.Context())
		before = ResponseStatus(r.Context())
		w.WriteHeader(http.StatusTeapot)
		after = ResponseStatus(r.Context())
	})
	h := newHarness(t, mux)

	h.serve("POST", "/push", nil)

	if id == "" {
		t.Error("expected a generated request ID")
	}
	if before != 0 || after != http.StatusTeapot {
		t.Errorf("expected status 0 then %d, got %d then %d", http.StatusTeapot, before, after)
	}
	if lines := h.lines(t); lines[0].RequestID != id {
		t.Errorf("expected logged request ID %q, got %q", id, lines[0].RequestID)
	}
}

func TestObserveKeepsOptionalInterfaces(t *testing.T) {
	var flushed bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected the wrapped writer to implement http.Flusher")
		}
		f.Flush()
		flushed = true
	})
	h := newHarness(t, mux)

	h.serve("GET", "/stream", nil)

	if !flushed {
		t.Fatal("handler did not run")
	}
	if lines := h.lines(t); lines[0].Status != http.StatusOK {
		t.Errorf("expected Flush to count as an implicit 200, got %d", lines[0].Status)
	}
}

func TestObserveRecordsPanicsAs500(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /push", func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	h := newHarness(t, mux)

	func() {
		defer func() {
			if p := recover(); p != "boom" {
				t.Errorf("expected the panic to propagate, got %v", p)
			}
		}()
		h.serve("POST", "/push", nil)
	}()

	if lines := h.lines(t); len(lines) != 1 || lines[0].Status != http.StatusInternalServerError {
		t.Errorf("expected one log line with status 500, got %+v", lines)
	}
	if got := h.count("POST", "/push", "5XX"); got != 1 {
		t.Errorf("expected 1 5XX observation, got %v", got)
	}
}

func TestObserveNamesSpansAfterTheRoute(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /accounts/{id}", func(w http.ResponseWriter, r *http.Request) {})
	h := newHarness(t, mux)

	h.serve("GET", "/accounts/42", nil)

	spans := h.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if got := spans[0].Name(); got != "GET /accounts/{id}" {
		t.Errorf("expected span name %q, got %q", "GET /accounts/{id}", got)
	}
	var route string
	for _, a := range spans[0].Attributes() {
		if a.Key == "http.route" {
			route = a.Value.AsString()
		}
	}
	if route != "/accounts/{id}" {
		t.Errorf("expected http.route %q, got %q", "/accounts/{id}", route)
	}
}

func TestObserveCanBeCreatedTwiceOnOneRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	Observe(Config{Registerer: reg})
	Observe(Config{Registerer: reg})
}

func TestRoutePath(t *testing.T) {
	cases := map[string]string{
		"":                      "unmatched",
		"/push":                 "/push",
		"POST /push":            "/push",
		"GET example.com/push":  "/push",
		"GET /accounts/{id}":    "/accounts/{id}",
		"example.com/healthz":   "/healthz",
		"DELETE /items/{id...}": "/items/{id...}",
	}
	for pattern, want := range cases {
		if got := routePath(pattern); got != want {
			t.Errorf("routePath(%q) = %q, want %q", pattern, got, want)
		}
	}
}
