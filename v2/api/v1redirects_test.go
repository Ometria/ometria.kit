package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestV1Redirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /push", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted) })
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {})
	handler := V1Redirects(mux)

	cases := []struct {
		method, target string
		status         int
		location       string
	}{
		{"POST", "/push", http.StatusAccepted, ""},
		{"GET", "/push", http.StatusMethodNotAllowed, ""},
		{"GET", "/missing", http.StatusNotFound, ""},
		// Trailing slash.
		{"POST", "/push/", http.StatusPermanentRedirect, "/push"},
		{"POST", "/push/?dry_run=1", http.StatusPermanentRedirect, "/push?dry_run=1"},
		{"GET", "/push/", http.StatusMethodNotAllowed, ""},
		{"GET", "/healthz/", http.StatusPermanentRedirect, "/healthz"},
		{"HEAD", "/healthz/", http.StatusPermanentRedirect, "/healthz"},
		{"GET", "/missing/", http.StatusNotFound, ""},
		{"GET", "/static/", http.StatusOK, ""},
		// Unclean paths.
		{"POST", "//push", http.StatusPermanentRedirect, "/push"},
		{"POST", "/a/../push", http.StatusPermanentRedirect, "/push"},
		{"POST", "//push/", http.StatusPermanentRedirect, "/push"},
		{"GET", "/static//", http.StatusPermanentRedirect, "/static/"},
		{"GET", "//push", http.StatusMethodNotAllowed, ""},
	}

	for _, c := range cases {
		t.Run(c.method+" "+c.target, func(t *testing.T) {
			// Set the path directly: httptest.NewRequest would clean it.
			req := httptest.NewRequest(c.method, "/", nil)
			req.URL.Path, req.URL.RawQuery, _ = strings.Cut(c.target, "?")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != c.status {
				t.Fatalf("expected %d, got %d", c.status, rec.Code)
			}
			if got := rec.Header().Get("Location"); got != c.location {
				t.Errorf("expected Location %q, got %q", c.location, got)
			}
		})
	}
}

func TestV1RedirectsLetFollowedPOSTsSucceed(t *testing.T) {
	var body string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /push", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusAccepted)
	})
	srv := httptest.NewServer(V1Redirects(mux))
	defer srv.Close()

	resp, err := srv.Client().Post(srv.URL+"/push/", "application/json", strings.NewReader(`[{"id":1}]`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted || body != `[{"id":1}]` {
		t.Errorf("expected the followed POST to reach /push with its body, got %d and %q", resp.StatusCode, body)
	}
}

func TestV1RedirectsLeavesServeMuxSubtreeRedirectAlone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {})

	direct, wrapped := httptest.NewRecorder(), httptest.NewRecorder()
	mux.ServeHTTP(direct, httptest.NewRequest("GET", "/static", nil))
	V1Redirects(mux).ServeHTTP(wrapped, httptest.NewRequest("GET", "/static", nil))

	if wrapped.Code != direct.Code || wrapped.Header().Get("Location") != direct.Header().Get("Location") {
		t.Errorf("expected %d to %q, got %d to %q",
			direct.Code, direct.Header().Get("Location"), wrapped.Code, wrapped.Header().Get("Location"))
	}
}

func TestV1RedirectsKeepsRouteForObserve(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /push", func(w http.ResponseWriter, r *http.Request) {})
	h := newHarness(t, mux)
	h.handler = Observe(Config{Logger: slog.New(slog.DiscardHandler), Registerer: h.reg})(V1Redirects(mux))

	h.serve("POST", "/push", nil)

	if got := h.count("POST", "/push", "2XX"); got != 1 {
		t.Errorf("expected the route to be visible through V1Redirects, got %v observations", got)
	}
}
