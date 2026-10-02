package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProblem(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/accounts/42", nil)

	NotFound(rec, req,
		WithInstance("/accounts/42"),
		WithFields(map[string]any{"status": 500, "account_id": 42}),
	)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("expected problem content type, got %q", ct)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"type":       "about:blank",
		"title":      "Not Found",
		"detail":     "Not Found",
		"status":     float64(404), // a clashing extra field must not override it
		"instance":   "/accounts/42",
		"account_id": float64(42),
	}
	if len(body) != len(want) {
		t.Errorf("expected %v, got %v", want, body)
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("expected %s=%v, got %v", k, v, body[k])
		}
	}
}

func TestRespond(t *testing.T) {
	t.Run("with data", func(t *testing.T) {
		rec := httptest.NewRecorder()
		Respond(rec, nil, http.StatusCreated, map[string]string{"id": "42"})

		if rec.Code != http.StatusCreated || rec.Body.String() != `{"id":"42"}` {
			t.Errorf("unexpected response %d %q", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected JSON content type, got %q", ct)
		}
	})

	t.Run("without data", func(t *testing.T) {
		rec := httptest.NewRecorder()
		Respond(rec, nil, http.StatusNoContent, nil)

		if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Errorf("unexpected response %d %q", rec.Code, rec.Body.String())
		}
	})

	t.Run("unencodable data", func(t *testing.T) {
		rec := httptest.NewRecorder()
		Respond(rec, httptest.NewRequest("GET", "/", nil), http.StatusOK, func() {})

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", rec.Code)
		}
	})
}

func TestNewServerSetsTimeouts(t *testing.T) {
	srv := NewServer(":8080", http.NewServeMux())

	if srv.ReadHeaderTimeout != 10*time.Second || srv.IdleTimeout != 120*time.Second {
		t.Errorf("unexpected timeouts: header %s, idle %s", srv.ReadHeaderTimeout, srv.IdleTimeout)
	}
}
