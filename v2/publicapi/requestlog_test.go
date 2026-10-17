package publicapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var logTime = time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)

func TestRequestLog(t *testing.T) {
	const key = "secret-api-key-WXYZ"
	withAuth := func(h map[string]string) map[string]string {
		out := map[string]string{"X-Ometria-Auth": key, "X-Request-ID": "req-1"}
		for k, v := range h {
			out[k] = v
		}
		return out
	}

	tests := []struct {
		name   string
		method string
		target string
		header map[string]string
		body   string
		opts   []RequestLogOption
		want   *Record // nil: no record.
	}{
		{
			name: "no api key", method: "GET", target: "/v2/push/_errors",
			header: map[string]string{HeaderAccountID: "1", HeaderAccessScopes: "read:default"},
		},
		{
			name: "api key 0 is missing", method: "GET", target: "/v2/push/_errors",
			header: map[string]string{"X-Ometria-Auth": "0", HeaderAccountID: "1", HeaderAccessScopes: "read:default"},
		},
		{
			name: "no account", method: "GET", target: "/v2/push/_errors",
			header: withAuth(map[string]string{HeaderAccessScopes: "read:default"}),
		},
		{
			name: "success", method: "GET", target: "/v2/push/_errors/?request_id=x",
			header: withAuth(map[string]string{HeaderAccountID: "+1", HeaderAccessScopes: "read:default"}),
			want: &Record{
				RequestID: "req-1", Timestamp: logTime, APIKey: "WXYZ", AccountID: "1", Authenticated: true,
				Path: "/v2/push/_errors", IPAddress: "192.0.2.1", RequestHTTPMethod: "GET",
				RequestHostname: "example.com", ResponseHTTPCode: 200, ResponseContentType: "application/json",
				ResponseSize: int64(len(`{"account":1,"scopes":["read:default"]}` + "\n")),
			},
		},
		{
			name: "authorization header and full key", method: "GET", target: "/v2/push/_errors",
			header: map[string]string{"Authorization": key, HeaderAccountID: "1", HeaderAccessScopes: "read:default"},
			opts:   []RequestLogOption{WithFullAPIKey(), WithRequestID(func(*http.Request) string { return "custom" })},
			want: &Record{
				RequestID: "custom", Timestamp: logTime, APIKey: key, AccountID: "1", Authenticated: true,
				Path: "/v2/push/_errors", IPAddress: "192.0.2.1", RequestHTTPMethod: "GET",
				RequestHostname: "example.com", ResponseHTTPCode: 200, ResponseContentType: "application/json",
				ResponseSize: int64(len(`{"account":1,"scopes":["read:default"]}` + "\n")),
			},
		},
		{
			name: "auth error keeps header account", method: "GET", target: "/v2/push/_errors",
			header: withAuth(map[string]string{HeaderAccountID: "007", HeaderAccessScopes: "write:default"}),
			want: &Record{
				RequestID: "req-1", Timestamp: logTime, APIKey: "WXYZ", AccountID: "007",
				Path: "/v2/push/_errors", IPAddress: "192.0.2.1", RequestHTTPMethod: "GET",
				RequestHostname: "example.com", ResponseHTTPCode: 500, ResponseContentType: "application/json",
				ResponseSize:         int64(len(legacyBody("500", "Internal server error"))),
				ResponseErrorMessage: "Internal server error",
			},
		},
		{
			name: "authenticated account wins over x-ometria-account", method: "GET", target: "/v2/push/_errors",
			header: withAuth(map[string]string{"X-Ometria-Account": "9", HeaderAccountID: "1", HeaderAccessScopes: "write:default"}),
			want: &Record{
				RequestID: "req-1", Timestamp: logTime, APIKey: "WXYZ", AccountID: "1", Authenticated: true,
				Path: "/v2/push/_errors", IPAddress: "192.0.2.1", RequestHTTPMethod: "GET",
				RequestHostname: "example.com", ResponseHTTPCode: 403, ResponseContentType: "application/json",
				ResponseSize:         int64(len(legacyBody("403", "Forbidden: Incorrect access scopes"))),
				ResponseErrorMessage: "Forbidden: Incorrect access scopes",
			},
		},
		{
			name: "not found", method: "GET", target: "/v2/nope",
			header: withAuth(map[string]string{"X-Ometria-Account": "9"}),
			want: &Record{
				RequestID: "req-1", Timestamp: logTime, APIKey: "WXYZ", AccountID: "9",
				Path: "/v2/nope", IPAddress: "192.0.2.1", RequestHTTPMethod: "GET",
				RequestHostname: "example.com", ResponseHTTPCode: 404, ResponseContentType: "application/json",
				ResponseSize:         int64(len(legacyBody("404", `No route found for \u0022GET \/v2\/nope\/\u0022 (See http:\/\/docs.ometria.com\/ for support)`))),
				ResponseErrorMessage: `No route found for "GET /v2/nope/" (See http://docs.ometria.com/ for support)`,
			},
		},
		{
			name: "body is capped and handler error has no message", method: "POST", target: "/v2/push/_errors/fail",
			header: withAuth(map[string]string{HeaderAccountID: "1", HeaderAccessScopes: "write:default", "Content-Type": "application/json; charset=utf-8"}),
			body:   `[{"id":1}]`,
			opts:   []RequestLogOption{WithMaxBodyBytes(4)},
			want: &Record{
				RequestID: "req-1", Timestamp: logTime, APIKey: "WXYZ", AccountID: "1", Authenticated: true,
				Path: "/v2/push/_errors/fail", IPAddress: "192.0.2.1", RequestHTTPMethod: "POST",
				RequestContentType: "json", RequestBody: `[{"i`,
				RequestHostname: "example.com", ResponseHTTPCode: 400, ResponseContentType: "application/json",
				ResponseSize: int64(len(`{"status":"ERROR","code":400,"message":"Could not decode payload","timestamp":1792243297,"request":{"method":"POST","url":"\/v2\/push\/_errors\/fail\/"}}`)),
			},
		},
		{
			name: "head has no size", method: "HEAD", target: "/v2/push/_errors",
			header: withAuth(map[string]string{HeaderAccountID: "1", HeaderAccessScopes: "read:default"}),
			want: &Record{
				RequestID: "req-1", Timestamp: logTime, APIKey: "WXYZ", AccountID: "1", Authenticated: true,
				Path: "/v2/push/_errors", IPAddress: "192.0.2.1", RequestHTTPMethod: "HEAD",
				RequestHostname: "example.com", ResponseHTTPCode: 200, ResponseContentType: "application/json",
			},
		},
		{
			name: "panic", method: "GET", target: "/v2/push/_errors/panic",
			header: withAuth(map[string]string{HeaderAccountID: "1"}),
			want: &Record{
				RequestID: "req-1", Timestamp: logTime, APIKey: "WXYZ", AccountID: "1",
				Path: "/v2/push/_errors/panic", IPAddress: "192.0.2.1", RequestHTTPMethod: "GET",
				RequestHostname: "example.com", ResponseHTTPCode: 500, ResponseContentType: "application/json",
				ResponseSize:         int64(len(legacyBody("500", "Internal server error"))),
				ResponseErrorMessage: "Internal server error",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &MemorySink{}
			opts := append([]RequestLogOption{WithClock(func() time.Time { return logTime })}, tt.opts...)
			serve(newTestHandler(sink, opts...), tt.method, tt.target, tt.header, tt.body)

			got := sink.Records()
			if tt.want == nil {
				if len(got) != 0 {
					t.Fatalf("expected no record, got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("expected 1 record, got %d", len(got))
			}
			if got[0] != *tt.want {
				t.Errorf("record =\n%+v\nwant\n%+v", got[0], *tt.want)
			}
		})
	}
}

func TestRequestLogKeepsBody(t *testing.T) {
	var seen string
	h := RequestLog(&MemorySink{}, WithMaxBodyBytes(3))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = string(b)
	}))
	serve(h, "POST", "/", map[string]string{"X-Ometria-Auth": "k", HeaderAccountID: "1"}, "hello world")
	if seen != "hello world" {
		t.Errorf("handler read %q", seen)
	}
}

func TestRecordJSON(t *testing.T) {
	rec := Record{
		RequestID: "r", Timestamp: time.Date(2026, 10, 17, 10, 30, 0, 0, time.FixedZone("BST", 3600)),
		APIKey: "WXYZ", AccountID: "42", Authenticated: true, Path: "/v2/push/_errors",
		IPAddress: "192.0.2.1", RequestHTTPMethod: "GET", RequestHostname: "api.ometria.com",
		ResponseHTTPCode: 200, ResponseSize: 2,
	}
	want := `{"request_id":"r","timestamp":"2026-10-17T09:30:00+00:00","api_key":"WXYZ","account_id":42,` +
		`"path":"/v2/push/_errors","ip_address":"192.0.2.1","request_http_method":"GET","request_content_type":null,` +
		`"request_body":"","response_http_code":200,"response_content_type":null,"response_size":2,` +
		`"request_hostname":"api.ometria.com","response_error_message":null}`
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}

	rec.Authenticated = false
	b, _ = json.Marshal(rec)
	if !strings.Contains(string(b), `"account_id":"42"`) {
		t.Errorf("unauthenticated account id should be a string: %s", b)
	}
}

func TestResponseWriterUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := wrapWriter(rec)
	if err := http.NewResponseController(rw).Flush(); err != nil {
		t.Fatal(err)
	}
	if !rec.Flushed || rw.status != 200 {
		t.Errorf("flushed=%v status=%d", rec.Flushed, rw.status)
	}
}
