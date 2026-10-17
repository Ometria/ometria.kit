package publicapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultMaxBodyBytes is how much of a request body RequestLog records by
// default.
const DefaultMaxBodyBytes = 64 << 10

// Record is one request in the API request log: the row PHP's finish hook
// writes, plus the request_hostname and response_error_message it adds for
// Firehose. Its JSON has PHP's keys, order and types; empty optional fields
// are null, as PHP's are.
type Record struct {
	RequestID string
	// Timestamp is when the request finished, written as PHP's date('c') in
	// UTC: "2026-10-17T09:30:00+00:00".
	Timestamp time.Time
	// APIKey is the last four characters of the client's API key, unless
	// RequestLog was given WithFullAPIKey.
	APIKey string
	// AccountID is the authenticated account's id, or else the
	// X-Ometria-Account or Ometria-Account-Id header. Like PHP, it is a JSON
	// number when Authenticate succeeded and a string otherwise.
	AccountID string
	// Authenticated reports whether AccountID came from Authenticate.
	Authenticated bool
	// Path is the request path without its trailing slashes ("" for "/").
	Path      string
	IPAddress string
	// RequestHTTPMethod is upper case, as Symfony's getMethod returns it.
	RequestHTTPMethod string
	// RequestContentType is Symfony's format name for the request's
	// Content-Type, such as "json" or "form", or "" (null) if it has none.
	RequestContentType string
	// RequestBody is at most the first WithMaxBodyBytes bytes of the body.
	RequestBody          string
	ResponseHTTPCode     int
	ResponseContentType  string
	ResponseSize         int64
	RequestHostname      string
	ResponseErrorMessage string
}

// MarshalJSON writes r as PHP's json_encode writes its row, keys in order.
func (r Record) MarshalJSON() ([]byte, error) {
	accountID, _ := json.Marshal(r.AccountID)
	if r.Authenticated {
		// Write the parsed integer, not the raw string: "+7" and "007" parse
		// but aren't JSON numbers.
		if n, err := strconv.Atoi(r.AccountID); err == nil {
			accountID = strconv.AppendInt(nil, int64(n), 10)
		}
	}
	return json.Marshal(struct {
		RequestID            string          `json:"request_id"`
		Timestamp            string          `json:"timestamp"`
		APIKey               string          `json:"api_key"`
		AccountID            json.RawMessage `json:"account_id"`
		Path                 string          `json:"path"`
		IPAddress            string          `json:"ip_address"`
		RequestHTTPMethod    string          `json:"request_http_method"`
		RequestContentType   *string         `json:"request_content_type"`
		RequestBody          string          `json:"request_body"`
		ResponseHTTPCode     int             `json:"response_http_code"`
		ResponseContentType  *string         `json:"response_content_type"`
		ResponseSize         int64           `json:"response_size"`
		RequestHostname      string          `json:"request_hostname"`
		ResponseErrorMessage *string         `json:"response_error_message"`
	}{
		RequestID:            r.RequestID,
		Timestamp:            r.Timestamp.UTC().Format("2006-01-02T15:04:05-07:00"),
		APIKey:               r.APIKey,
		AccountID:            accountID,
		Path:                 r.Path,
		IPAddress:            r.IPAddress,
		RequestHTTPMethod:    r.RequestHTTPMethod,
		RequestContentType:   nullable(r.RequestContentType),
		RequestBody:          r.RequestBody,
		ResponseHTTPCode:     r.ResponseHTTPCode,
		ResponseContentType:  nullable(r.ResponseContentType),
		ResponseSize:         r.ResponseSize,
		RequestHostname:      r.RequestHostname,
		ResponseErrorMessage: nullable(r.ResponseErrorMessage),
	})
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Sink receives RequestLog's records. Send is called on the request's
// goroutine before the response is complete, so it must not block: queue
// the record and send it elsewhere. It has no error to return because a
// failure to log must never fail the request.
type Sink interface {
	Send(ctx context.Context, rec Record)
}

// MemorySink keeps records in memory, for tests.
type MemorySink struct {
	mu      sync.Mutex
	records []Record
}

// Send implements Sink.
func (s *MemorySink) Send(_ context.Context, rec Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, rec)
}

// Records returns a copy of the records sent so far.
func (s *MemorySink) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.records...)
}

// RequestLogOption configures RequestLog.
type RequestLogOption func(*requestLogConfig)

type requestLogConfig struct {
	maxBody   int
	fullKey   bool
	requestID func(*http.Request) string
	clientIP  func(*http.Request) string
	now       func() time.Time
}

// WithMaxBodyBytes sets how much of the request body is recorded, default
// DefaultMaxBodyBytes. Handlers still read the whole body.
func WithMaxBodyBytes(n int) RequestLogOption {
	return func(c *requestLogConfig) { c.maxBody = n }
}

// WithFullAPIKey records the whole API key, as PHP does, instead of its last
// four characters, as ometria.api.push does for security.
func WithFullAPIKey() RequestLogOption {
	return func(c *requestLogConfig) { c.fullKey = true }
}

// WithRequestID sets how a request's id is found, such as api.RequestID.
// The default is the X-Request-ID header, or a random UUID without one.
// PHP generated a time-based UUID for every request.
func WithRequestID(f func(*http.Request) string) RequestLogOption {
	return func(c *requestLogConfig) { c.requestID = f }
}

// WithClientIP sets how the client's IP address is found. The default is
// the connection's remote address, as Symfony's getClientIp returns
// REMOTE_ADDR when no trusted proxies are set, which ometria.api doesn't.
// ometria.api.push uses the first valid X-Forwarded-For address instead.
func WithClientIP(f func(*http.Request) string) RequestLogOption {
	return func(c *requestLogConfig) { c.clientIP = f }
}

// WithClock sets the clock for record timestamps, for tests.
func WithClock(now func() time.Time) RequestLogOption {
	return func(c *requestLogConfig) { c.now = now }
}

type logStateKey struct{}

// logState is what the inner middleware tell RequestLog about the request.
type logState struct {
	accountID    string // Set by Authenticate.
	errorMessage string // Set by Error.
}

func logStateFrom(ctx context.Context) *logState {
	s, _ := ctx.Value(logStateKey{}).(*logState)
	return s
}

// RequestLog returns middleware that sends sink one Record per request, as
// PHP's finish hook does, for requests with an API key (the X-Ometria-Auth
// header, else Authorization) and an account id (the authenticated account,
// else the X-Ometria-Account header, else Ometria-Account-Id). Put it
// outside Routes and Authenticate so it sees their errors.
//
// It reads up to WithMaxBodyBytes of the request body before calling next,
// which still reads the whole body.
//
// Unlike PHP, it records requests that ended in a fatal error, such as a
// failed account lookup. PHP's HEAD responses have no content when the hook
// runs, so their response_size is 0 here too.
func RequestLog(sink Sink, opts ...RequestLogOption) Middleware {
	cfg := requestLogConfig{
		maxBody:   DefaultMaxBodyBytes,
		requestID: defaultRequestID,
		clientIP:  remoteIP,
		now:       time.Now,
	}
	for _, o := range opts {
		o(&cfg)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKey := headerValue(r.Header, "X-Ometria-Auth", "Authorization")
			if apiKey == "" || headerValue(r.Header, "X-Ometria-Account", HeaderAccountID) == "" {
				// PHP returns early unless both are present; Authenticate
				// can't supply an account without the account header.
				next.ServeHTTP(w, r)
				return
			}

			body := peekBody(r, cfg.maxBody)
			state := &logState{}
			rw := wrapWriter(w)
			r = r.WithContext(context.WithValue(r.Context(), logStateKey{}, state))

			defer func() {
				p := recover()

				rec := Record{
					RequestID:            cfg.requestID(r),
					Timestamp:            cfg.now(),
					APIKey:               apiKey,
					AccountID:            state.accountID,
					Authenticated:        state.accountID != "",
					Path:                 strings.TrimRight(phpPathInfo(r), "/"),
					IPAddress:            cfg.clientIP(r),
					RequestHTTPMethod:    phpMethod(r),
					RequestContentType:   phpContentFormat(r.Header.Get("Content-Type")),
					RequestBody:          string(body),
					ResponseHTTPCode:     rw.status,
					ResponseContentType:  rw.Header().Get("Content-Type"),
					ResponseSize:         rw.size,
					RequestHostname:      r.Host,
					ResponseErrorMessage: state.errorMessage,
				}
				if !cfg.fullKey && len(rec.APIKey) > 4 {
					rec.APIKey = rec.APIKey[len(rec.APIKey)-4:]
				}
				if !rec.Authenticated {
					rec.AccountID = headerValue(r.Header, "X-Ometria-Account", HeaderAccountID)
				}
				if rec.ResponseHTTPCode == 0 {
					rec.ResponseHTTPCode = http.StatusOK
				}
				if p != nil {
					rec.ResponseHTTPCode = http.StatusInternalServerError
				}
				if r.Method == http.MethodHead {
					rec.ResponseSize = 0
				}
				sink.Send(context.WithoutCancel(r.Context()), rec)

				if p != nil {
					panic(p)
				}
			}()

			next.ServeHTTP(rw, r)
		})
	}
}

// peekBody returns up to limit bytes of r's body, and puts them back in front
// of the rest of it for the handler.
func peekBody(r *http.Request, limit int) []byte {
	if r.Body == nil || r.Body == http.NoBody || limit <= 0 {
		return nil
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)))
	if err != nil {
		// Let the handler see the error when it reads past what we have.
		r.Body = readCloser{io.MultiReader(bytes.NewReader(buf), errReader{err}), r.Body}
		return buf
	}
	r.Body = readCloser{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}
	return buf
}

type readCloser struct {
	io.Reader
	io.Closer
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func defaultRequestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-ID"); id != "" {
		return id
	}
	return newUUID()
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	var u [16]byte
	rand.Read(u[:]) //nolint:errcheck // crypto/rand.Read never fails.
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	var s [36]byte
	hex.Encode(s[0:8], u[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], u[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], u[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], u[8:10])
	s[23] = '-'
	hex.Encode(s[24:], u[10:])
	return string(s[:])
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// responseWriter records the status and size of a response. Unwrap lets
// http.ResponseController reach the optional interfaces of the writer it
// wraps.
type responseWriter struct {
	http.ResponseWriter
	status int
	size   int64
}

// wrapWriter wraps w, or returns it if it's already wrapped.
func wrapWriter(w http.ResponseWriter) *responseWriter {
	if rw, ok := w.(*responseWriter); ok {
		return rw
	}
	return &responseWriter{ResponseWriter: w}
}

func (w *responseWriter) WriteHeader(code int) {
	if w.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.size += int64(n)
	return n, err
}

func (w *responseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	//nolint:errcheck // Flushing is best effort, as with http.Flusher.
	http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
