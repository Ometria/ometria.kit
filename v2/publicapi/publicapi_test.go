package publicapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// goldenTime is the timestamp of the PHP responses these tests compare with.
var goldenTime = time.Unix(1792243297, 0)

var testEncoder = UseEncoder(LegacyEncoder{Now: func() time.Time { return goldenTime }})

var testAccounts = AccountLookupFunc(func(_ context.Context, id int) (Account, error) {
	switch id {
	case 1:
		return Account{ID: 1, IsActive: true, DbShard: "shard1", DbName: "ecomm_1"}, nil
	case 3:
		// What api-accounts' status filter does with inactive accounts.
		return Account{}, ErrAccountNotFound
	case 5:
		return Account{}, errors.New("api-accounts: 500")
	case 6:
		// Unreachable with the real api-accounts, but handled.
		return Account{ID: 6, IsActive: false}, nil
	}
	return Account{}, ErrAccountNotFound
})

// newTestHandler serves "GET /v2/push/_errors" as a PHP-compatible service
// would, echoing the account and scopes it was authenticated with.
func newTestHandler(sink Sink, opts ...RequestLogOption) http.Handler {
	authed := Chain(Authenticate(testAccounts), RequireScopeByMethod())
	mux := http.NewServeMux()
	mux.Handle("GET /v2/push/_errors", authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acc, _ := AccountFromContext(r.Context())
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"account": acc.ID, "scopes": ScopesFromContext(r.Context())}) //nolint:errcheck
	})))
	mux.Handle("POST /v2/push/_errors/fail", authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body) //nolint:errcheck
		Error(w, r, http.StatusBadRequest, "Could not decode payload", WithRequest())
	})))
	mux.Handle("GET /v2/push/_errors/panic", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	mws := []Middleware{testEncoder}
	if sink != nil {
		mws = append(mws, RequestLog(sink, opts...))
	}
	mws = append(mws, Recover(slog.New(slog.NewTextHandler(io.Discard, nil))))
	return Chain(mws...)(Routes(mux))
}

func serve(h http.Handler, method, target string, header map[string]string, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// nginx400 is nginx's golden 400 page, CRLF line endings and all.
const nginx400 = "<html>\r\n<head><title>400 Bad Request</title></head>\r\n<body>\r\n<center><h1>400 Bad Request</h1></center>\r\n<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n"

func legacyBody(code, msg string) string {
	return `{"status":"ERROR","code":` + code + `,"message":"` + msg + `","timestamp":1792243297}`
}

func TestResponses(t *testing.T) {
	auth := func(account, scopes string) map[string]string {
		h := map[string]string{}
		if account != "" {
			h[HeaderAccountID] = account
		}
		if scopes != "" {
			h[HeaderAccessScopes] = scopes
		}
		return h
	}

	tests := []struct {
		name        string
		method      string
		target      string
		header      map[string]string
		status      int
		contentType string
		allow       string
		body        string // Exact, PHP's bytes for the error cases.
	}{
		// Authenticate.
		{"missing account header", "GET", "/v2/push/_errors", auth("", "read:default"),
			403, "application/json", "", legacyBody("403", "Forbidden: Ometria-Account-Id header missing")},
		{"missing both headers", "GET", "/v2/push/_errors", auth("", ""),
			403, "application/json", "", legacyBody("403", "Forbidden: Ometria-Account-Id header missing")},
		{"account header 0 is missing", "GET", "/v2/push/_errors", auth("0", "read:default"),
			403, "application/json", "", legacyBody("403", "Forbidden: Ometria-Account-Id header missing")},
		{"missing scopes header", "GET", "/v2/push/_errors", auth("1", ""),
			403, "application/json", "", legacyBody("403", "Forbidden: Ometria-Access-Scopes header missing")},
		{"scopes header 0 is missing", "GET", "/v2/push/_errors", auth("1", "0"),
			403, "application/json", "", legacyBody("403", "Forbidden: Ometria-Access-Scopes header missing")},
		{"inactive account", "GET", "/v2/push/_errors", auth("3", "read:default"),
			500, "application/json", "", legacyBody("500", "Internal server error")},
		{"unknown account", "GET", "/v2/push/_errors", auth("4", "read:default"),
			500, "application/json", "", legacyBody("500", "Internal server error")},
		{"lookup fails", "GET", "/v2/push/_errors", auth("5", "read:default"),
			500, "application/json", "", legacyBody("500", "Internal server error")},
		{"non-numeric account", "GET", "/v2/push/_errors", auth("abc", "read:default"),
			500, "application/json", "", legacyBody("500", "Internal server error")},
		{"found but inactive account", "GET", "/v2/push/_errors", auth("6", "read:default"),
			403, "application/json", "", legacyBody("403", "Forbidden: Authorization failed")},
		{"account id parsed like api-accounts", "GET", "/v2/push/_errors", auth("+001", "read:default"),
			200, "application/json", "", `{"account":1,"scopes":["read:default"]}` + "\n"},

		// RequireScopeByMethod.
		{"read scope", "GET", "/v2/push/_errors", auth("1", "read:default"),
			200, "application/json", "", `{"account":1,"scopes":["read:default"]}` + "\n"},
		{"read and write scopes", "GET", "/v2/push/_errors", auth("1", "write:default,read:default"),
			200, "application/json", "", `{"account":1,"scopes":["write:default","read:default"]}` + "\n"},
		{"write scope only", "GET", "/v2/push/_errors", auth("1", "write:default"),
			403, "application/json", "", legacyBody("403", "Forbidden: Incorrect access scopes")},
		{"scopes with space", "GET", "/v2/push/_errors", auth("1", "write:default, read:default"),
			403, "application/json", "", legacyBody("403", "Forbidden: Incorrect access scopes")},
		{"post needs write", "POST", "/v2/push/_errors/fail", auth("1", "read:default"),
			403, "application/json", "", legacyBody("403", "Forbidden: Incorrect access scopes")},
		{"head needs no scope", "HEAD", "/v2/push/_errors", auth("1", "write:default"),
			200, "application/json", "", ""},

		// Routes.
		{"trailing slash", "GET", "/v2/push/_errors/", auth("1", "read:default"),
			200, "application/json", "", `{"account":1,"scopes":["read:default"]}` + "\n"},
		{"two trailing slashes", "GET", "/v2/push/_errors//", auth("1", "read:default"),
			404, "application/json", "", legacyBody("404", `No route found for \u0022GET \/v2\/push\/_errors\/\/\u0022 (See http:\/\/docs.ometria.com\/ for support)`)},
		{"unclean path", "GET", "//v2/push/_errors", auth("1", "read:default"),
			404, "application/json", "", legacyBody("404", `No route found for \u0022GET \/\/v2\/push\/_errors\/\u0022 (See http:\/\/docs.ometria.com\/ for support)`)},
		{"unknown subpath", "GET", "/v2/push/_errors/foo", auth("1", "read:default"),
			404, "application/json", "", legacyBody("404", `No route found for \u0022GET \/v2\/push\/_errors\/foo\/\u0022 (See http:\/\/docs.ometria.com\/ for support)`)},
		{"unknown subpath with query", "GET", "/v2/push/_errors/foo?a=1", nil,
			404, "application/json", "", legacyBody("404", `No route found for \u0022GET \/v2\/push\/_errors\/foo\/\u0022 (See http:\/\/docs.ometria.com\/ for support)`)},
		{"unknown path with referer", "GET", "/nope", map[string]string{"Referer": "https://x.test/a"},
			404, "application/json", "", legacyBody("404", `No route found for \u0022GET \/nope\/\u0022 (from \u0022https:\/\/x.test\/a\u0022) (See http:\/\/docs.ometria.com\/ for support)`)},
		{"post on get route", "POST", "/v2/push/_errors", auth("1", "read:default"),
			405, "application/json", "", legacyBody("405", `No route found for \u0022POST \/v2\/push\/_errors\/\u0022: Method Not Allowed (Allow: GET, OPTIONS)`)},
		{"delete on get route", "DELETE", "/v2/push/_errors", auth("1", "read:default"),
			405, "application/json", "", legacyBody("405", `No route found for \u0022DELETE \/v2\/push\/_errors\/\u0022: Method Not Allowed (Allow: GET, OPTIONS)`)},
		{"wrong method without auth", "PUT", "/v2/push/_errors", nil,
			405, "application/json", "", legacyBody("405", `No route found for \u0022PUT \/v2\/push\/_errors\/\u0022: Method Not Allowed (Allow: GET, OPTIONS)`)},
		{"options", "OPTIONS", "/v2/push/_errors", nil,
			200, "text/html; charset=UTF-8", "GET", ""},
		{"options on unknown path", "OPTIONS", "/v2/nope", nil,
			404, "application/json", "", legacyBody("404", `No route found for \u0022OPTIONS \/v2\/nope\/\u0022 (See http:\/\/docs.ometria.com\/ for support)`)},

		// nginx, in front of PHP, rejects methods that aren't all upper case.
		{"lowercase get", "get", "/v2/push/_errors", auth("1", "read:default"),
			400, "text/html", "", nginx400},
		{"lowercase get without scope", "get", "/v2/push/_errors", auth("1", "write:default"),
			400, "text/html", "", nginx400},
		{"mixed case", "Get", "/v2/push/_errors", nil,
			400, "text/html", "", nginx400},
		{"odd case", "gEt", "/v2/nope", nil,
			400, "text/html", "", nginx400},
		{"lowercase options", "options", "/v2/push/_errors", nil,
			400, "text/html", "", nginx400},

		// Handler errors.
		{"apiResponseError", "POST", "/v2/push/_errors/fail?x=a/b", auth("1", "write:default"),
			400, "application/json", "", `{"status":"ERROR","code":400,"message":"Could not decode payload","timestamp":1792243297,"request":{"method":"POST","url":"\/v2\/push\/_errors\/fail\/?x=a\/b\/"}}`},
		{"panic", "GET", "/v2/push/_errors/panic", nil,
			500, "application/json", "", legacyBody("500", "Internal server error")},
	}

	h := newTestHandler(nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(h, tt.method, tt.target, tt.header, "")
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
			if ct := rec.Header().Get("Content-Type"); ct != tt.contentType {
				t.Errorf("Content-Type = %q, want %q", ct, tt.contentType)
			}
			if allow := rec.Header().Get("Allow"); allow != tt.allow {
				t.Errorf("Allow = %q, want %q", allow, tt.allow)
			}
			body := rec.Body.String()
			if tt.method == http.MethodHead {
				body = "" // net/http drops it; the recorder doesn't.
			}
			if body != tt.body {
				t.Errorf("body =\n%s\nwant\n%s", body, tt.body)
			}
		})
	}
}

func TestProblemEncoder(t *testing.T) {
	tests := []struct {
		name string
		opts []ErrorOption
		want string
	}{
		{"plain", nil, `{"type":"about:blank","title":"Forbidden","status":403,"detail":"Forbidden: Incorrect access scopes"}`},
		{"with request", []ErrorOption{WithRequest()}, `{"type":"about:blank","title":"Forbidden","status":403,"detail":"Forbidden: Incorrect access scopes","instance":"/v2/x?a=1"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := UseEncoder(ProblemEncoder{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Error(w, r, http.StatusForbidden, msgIncorrectScopes, tt.opts...)
			}))
			rec := serve(h, "GET", "/v2/x?a=1", nil, "")
			if rec.Code != 403 || rec.Header().Get("Content-Type") != "application/problem+json" || rec.Body.String() != tt.want {
				t.Errorf("got %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
			}
		})
	}
}

func TestErrorDefaultsToLegacy(t *testing.T) {
	rec := httptest.NewRecorder()
	Error(rec, httptest.NewRequest("GET", "/", nil), http.StatusTeapot, "short")
	var body struct {
		Status    string `json:"status"`
		Code      int    `json:"code"`
		Message   string `json:"message"`
		Timestamp int64  `json:"timestamp"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 418 || body.Status != "ERROR" || body.Code != 418 || body.Message != "short" ||
		time.Since(time.Unix(body.Timestamp, 0)) > time.Minute {
		t.Errorf("unexpected response %d %s", rec.Code, rec.Body.String())
	}
}

func TestAppendPHPString(t *testing.T) {
	tests := []struct {
		in       string
		hex, std string
	}{
		{`a"b`, `"a\u0022b"`, `"a\"b"`},
		{`a/b\c`, `"a\/b\\c"`, `"a\/b\\c"`},
		{`<&'>`, `"\u003C\u0026\u0027\u003E"`, `"<&'>"`},
		{"\n\t\x01\x7f", `"\n\t\u0001` + "\x7f" + `"`, `"\n\t\u0001` + "\x7f" + `"`},
		{"\u00e9\u20ac\U0001F600", `"\u00e9\u20ac\ud83d\ude00"`, `"\u00e9\u20ac\ud83d\ude00"`},
		{"bad\xff", `"bad\ufffd"`, `"bad\ufffd"`},
	}
	for _, tt := range tests {
		if got := string(appendPHPString(nil, tt.in, true)); got != tt.hex {
			t.Errorf("hex %q: got %s, want %s", tt.in, got, tt.hex)
		}
		for _, hex := range []bool{true, false} {
			if got := appendPHPString(nil, tt.in, hex); !json.Valid(got) {
				t.Errorf("hex=%v %q: %s is not valid JSON", hex, tt.in, got)
			}
		}
		if got := string(appendPHPString(nil, tt.in, false)); got != tt.std {
			t.Errorf("std %q: got %s, want %s", tt.in, got, tt.std)
		}
	}
}

func TestPHPRequestURI(t *testing.T) {
	tests := []struct{ in, uri, path string }{
		{"/", "/", "/"},
		{"/a", "/a/", "/a/"},
		{"/a/", "/a/", "/a/"},
		{"/a?x=1", "/a/?x=1/", "/a/"},
		{"/a/?x=1", "/a/?x=1/", "/a/"},
		{"/a?x=/", "/a/?x=/", "/a/"},
		{"/a%2Fb", "/a%2Fb/", "/a%2Fb/"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", tt.in, nil)
		if got := phpRequestURI(r); got != tt.uri {
			t.Errorf("phpRequestURI(%q) = %q, want %q", tt.in, got, tt.uri)
		}
		if got := phpPathInfo(r); got != tt.path {
			t.Errorf("phpPathInfo(%q) = %q, want %q", tt.in, got, tt.path)
		}
	}
}

func TestPHPContentFormat(t *testing.T) {
	tests := map[string]string{
		"":                                  "",
		"application/json":                  "json",
		"application/json; charset=utf-8":   "json",
		"Application/JSON":                  "",
		"application/x-www-form-urlencoded": "form",
		"multipart/form-data; boundary=x":   "",
		"text/plain":                        "txt",
		"application/vnd.api+json":          "",
	}
	for in, want := range tests {
		if got := phpContentFormat(in); got != want {
			t.Errorf("phpContentFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequireScopeByMethod(t *testing.T) {
	tests := []struct {
		method string
		scopes []string
		ok     bool
	}{
		{"GET", []string{"read:default"}, true},
		{"GET", []string{"write:default"}, false},
		{"GET", nil, false},
		{"POST", []string{"write:default"}, true},
		{"PUT", []string{"read:default"}, false},
		{"PATCH", []string{"read:default"}, false},
		{"DELETE", []string{"write:default"}, true},
		{"HEAD", nil, true},
		{"OPTIONS", nil, true},
		// PHP compares REQUEST_METHOD as sent, so "get" would need no scope
		// there were nginx not in front; here it needs the same as GET.
		{"get", nil, false},
		{"get", []string{"read:default"}, true},
		{"post", []string{"read:default"}, false},
		{"Delete", []string{"write:default"}, true},
	}
	h := testEncoder(RequireScopeByMethod()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, "/", nil)
		if tt.scopes != nil {
			req = req.WithContext(ContextWithAuth(req.Context(), Account{ID: 1}, tt.scopes))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if ok := rec.Code == 200; ok != tt.ok {
			t.Errorf("%s %v: got %d %s", tt.method, tt.scopes, rec.Code, rec.Body.String())
		}
		if !tt.ok && rec.Body.String() != legacyBody("403", "Forbidden: Incorrect access scopes") {
			t.Errorf("%s %v: body %s", tt.method, tt.scopes, rec.Body.String())
		}
	}
}

func TestChainOrder(t *testing.T) {
	var order []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	Chain(mw("a"), mw("b"), mw("c"))(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if strings.Join(order, "") != "abc" {
		t.Errorf("order = %v", order)
	}
}

func TestErrorAfterPartialWrite(t *testing.T) {
	logs := &strings.Builder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	defer slog.SetDefault(prev)

	for _, tt := range []struct {
		name  string
		write func(http.ResponseWriter)
		body  string
	}{
		{"headers", func(w http.ResponseWriter) { w.WriteHeader(http.StatusAccepted) }, ""},
		{"body", func(w http.ResponseWriter) { w.Write([]byte(`{"partial":`)) }, `{"partial":`}, //nolint:errcheck
	} {
		t.Run(tt.name, func(t *testing.T) {
			logs.Reset()
			sink := &MemorySink{}
			h := Chain(testEncoder, RequestLog(sink), Recover(nil))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tt.write(w)
				Error(w, r, http.StatusBadRequest, "too late")
			}))
			rec := serve(h, "GET", "/", map[string]string{"X-Ometria-Auth": "k", HeaderAccountID: "1"}, "")
			want := http.StatusAccepted
			if tt.body != "" {
				want = http.StatusOK
			}
			if rec.Code != want || rec.Body.String() != tt.body {
				t.Errorf("response = %d %q, want %d %q", rec.Code, rec.Body.String(), want, tt.body)
			}
			if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "too late") {
				t.Errorf("expected an error log, got %q", logs.String())
			}
			if recs := sink.Records(); len(recs) != 1 || recs[0].ResponseErrorMessage != "" || recs[0].ResponseHTTPCode != want {
				t.Errorf("records = %+v", recs)
			}
		})
	}

	// A plain ResponseWriter can't tell, so Error writes as before.
	rec := httptest.NewRecorder()
	rec.Write([]byte("x")) //nolint:errcheck
	Error(rec, httptest.NewRequest("GET", "/", nil), http.StatusBadRequest, "msg")
	if !strings.Contains(rec.Body.String(), `"message":"msg"`) {
		t.Errorf("plain writer body = %q", rec.Body.String())
	}
}
