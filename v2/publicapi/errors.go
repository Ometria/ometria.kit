package publicapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// ErrorInfo is an error response for an Encoder to write.
type ErrorInfo struct {
	Code    int
	Message string
	// IncludeRequest asks for the request's method and URL in the body, as
	// PHP's apiResponseError adds them. Set it with WithRequest.
	IncludeRequest bool
}

// Encoder writes error responses. Install one with UseEncoder.
type Encoder interface {
	EncodeError(w http.ResponseWriter, r *http.Request, e ErrorInfo)
}

// ErrorOption changes an error response.
type ErrorOption func(*ErrorInfo)

// WithRequest makes the error look like one from PHP's apiResponseError,
// which controllers return for their own errors, rather than one from PHP's
// error handler, which catches exceptions: LegacyEncoder appends
// "request":{"method":...,"url":...} and escapes as plain json_encode does,
// and RequestLog leaves response_error_message empty, as PHP only records
// the messages of exceptions.
func WithRequest() ErrorOption {
	return func(e *ErrorInfo) { e.IncludeRequest = true }
}

type encoderKey struct{}

// UseEncoder returns middleware that makes Error, and every error this
// package writes, use enc. Put it outside the other middleware.
func UseEncoder(enc Encoder) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), encoderKey{}, enc)))
		})
	}
}

// Error writes an error response with code and msg, using the encoder
// installed with UseEncoder, or LegacyEncoder if there is none.
func Error(w http.ResponseWriter, r *http.Request, code int, msg string, opts ...ErrorOption) {
	e := ErrorInfo{Code: code, Message: msg}
	for _, o := range opts {
		o(&e)
	}
	if s := logStateFrom(r.Context()); s != nil && !e.IncludeRequest {
		// PHP's $_SERVER['API_LAST_ERROR_MESSAGE'].
		s.errorMessage = msg
	}

	enc, ok := r.Context().Value(encoderKey{}).(Encoder)
	if !ok {
		enc = LegacyEncoder{}
	}
	enc.EncodeError(w, r, e)
}

// LegacyEncoder writes the error bodies of PHP's ometria.api:
//
//	{"status":"ERROR","code":403,"message":"Forbidden: Incorrect access scopes","timestamp":1792243297}
//
// with Content-Type application/json. The bytes match PHP's: messages are
// escaped as Symfony's JsonResponse does (json_encode with JSON_HEX_TAG,
// JSON_HEX_APOS, JSON_HEX_QUOT and JSON_HEX_AMP, so `"` is " and "/" is
// \/), or as plain json_encode for WithRequest errors, which append
//
//	"request":{"method":"GET","url":"/v2/push/_errors/?x=1/"}
//
// The url is the request URI after PHP's rewrite (see Routes), mangled query
// string included. Invalid UTF-8, which makes PHP's json_encode fail, is
// replaced with U+FFFD.
type LegacyEncoder struct {
	// Now returns the timestamp to write. Defaults to time.Now.
	Now func() time.Time
}

// EncodeError implements Encoder.
func (enc LegacyEncoder) EncodeError(w http.ResponseWriter, r *http.Request, e ErrorInfo) {
	now := time.Now
	if enc.Now != nil {
		now = enc.Now
	}
	hex := !e.IncludeRequest

	b := make([]byte, 0, 128)
	b = append(b, `{"status":"ERROR","code":`...)
	b = strconv.AppendInt(b, int64(e.Code), 10)
	b = append(b, `,"message":`...)
	b = appendPHPString(b, e.Message, hex)
	b = append(b, `,"timestamp":`...)
	b = strconv.AppendInt(b, now().Unix(), 10)
	if e.IncludeRequest {
		b = append(b, `,"request":{"method":`...)
		b = appendPHPString(b, phpMethod(r), false)
		b = append(b, `,"url":`...)
		b = appendPHPString(b, phpRequestURI(r), false)
		b = append(b, '}')
	}
	b = append(b, '}')

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Code)
	//nolint:errcheck
	w.Write(b)
}

// ProblemEncoder writes RFC 7807 problem responses, for public APIs with no
// PHP clients to stay compatible with:
//
//	{"type":"about:blank","title":"Forbidden","status":403,"detail":"Forbidden: Incorrect access scopes"}
//
// WithRequest errors also carry the request URI as "instance".
type ProblemEncoder struct{}

// EncodeError implements Encoder.
func (ProblemEncoder) EncodeError(w http.ResponseWriter, r *http.Request, e ErrorInfo) {
	p := struct {
		Type     string `json:"type"`
		Title    string `json:"title"`
		Status   int    `json:"status"`
		Detail   string `json:"detail"`
		Instance string `json:"instance,omitempty"`
	}{
		Type:   "about:blank",
		Title:  http.StatusText(e.Code),
		Status: e.Code,
		Detail: e.Message,
	}
	if e.IncludeRequest {
		p.Instance = r.URL.RequestURI()
	}
	b, _ := json.Marshal(p) // Strings and ints always encode.

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(e.Code)
	//nolint:errcheck
	w.Write(b)
}

// Recover returns middleware that turns a panic into PHP's response to an
// uncaught exception, a 500 "Internal server error", and logs it to logger
// (slog.Default() if nil). If the response has already started, or the panic
// is http.ErrAbortHandler, it lets the panic carry on so net/http aborts the
// connection.
func Recover(logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := wrapWriter(w)
			defer func() {
				p := recover()
				if p == nil {
					return
				}
				if p == http.ErrAbortHandler || rw.status != 0 {
					panic(p)
				}
				logger.ErrorContext(r.Context(), "panic serving request",
					slog.Any("panic", p),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())),
				)
				Error(rw, r, http.StatusInternalServerError, msgInternalServerError)
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

// phpMethod is Symfony's Request::getMethod, less the X-HTTP-Method-Override
// header Symfony honours on POSTs, which this package ignores.
func phpMethod(r *http.Request) string {
	return strings.ToUpper(r.Method)
}
