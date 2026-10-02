package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Config configures Observe. The zero value is usable.
type Config struct {
	// Logger receives one "request" line per request. Defaults to slog.Default().
	Logger *slog.Logger
	// Registerer is where http_request_duration_seconds is registered.
	// Defaults to prometheus.DefaultRegisterer.
	Registerer prometheus.Registerer
	// TracerProvider creates the request spans. Defaults to the global OTel
	// provider, which records nothing until one is configured.
	TracerProvider trace.TracerProvider
	// QuietRoutes lists route patterns, as registered with http.ServeMux
	// (e.g. "GET /healthz"), that are measured and traced but not logged.
	QuietRoutes []string
}

type ctxKey int

const keyRequestState ctxKey = iota

// requestState is shared between Observe and the handlers it wraps.
type requestState struct {
	id     string
	status int
	bytes  int64
}

// RequestID returns the ID of the request being served: the X-Request-ID
// header if the client sent one, otherwise a generated ID.
func RequestID(ctx context.Context) string {
	if s, ok := ctx.Value(keyRequestState).(*requestState); ok {
		return s.id
	}
	return ""
}

// ResponseStatus returns the status code written so far for the request being
// served, or 0 if nothing has been written yet.
func ResponseStatus(ctx context.Context) int {
	if s, ok := ctx.Value(keyRequestState).(*requestState); ok {
		return s.status
	}
	return 0
}

// Observe returns middleware that logs, measures and traces every request.
// Wrap the whole http.ServeMux with it so unmatched routes are observed too.
func Observe(cfg Config) func(http.Handler) http.Handler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	reg := cfg.Registerer
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	duration := registerDuration(reg)

	var otelOpts []otelhttp.Option
	if cfg.TracerProvider != nil {
		otelOpts = append(otelOpts, otelhttp.WithTracerProvider(cfg.TracerProvider))
	}

	return func(next http.Handler) http.Handler {
		observed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			state := &requestState{id: r.Header.Get("X-Request-ID")}
			if state.id == "" {
				state.id = rand.Text()
			}
			r = r.WithContext(context.WithValue(r.Context(), keyRequestState, state))

			defer func() {
				// net/http turns a panic into an aborted response; record it
				// as a 500 and let it carry on up.
				p := recover()
				if p != nil {
					state.status = http.StatusInternalServerError
				}
				if state.status == 0 {
					// Nothing written: net/http sends an empty 200.
					state.status = http.StatusOK
				}

				// r is the request the mux saw, so it carries the pattern.
				route := routePath(r.Pattern)
				duration.WithLabelValues(r.Method, route, statusGroup(state.status)).
					Observe(time.Since(start).Seconds())

				span := trace.SpanFromContext(r.Context())
				span.SetName(r.Method + " " + route)
				span.SetAttributes(attribute.String("http.route", route))

				if !slices.Contains(cfg.QuietRoutes, r.Pattern) {
					logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
						slog.String("request_id", state.id),
						slog.String("method", r.Method),
						slog.String("path", r.URL.Path),
						slog.String("route", route),
						slog.Int("status", state.status),
						slog.Int64("bytes", state.bytes),
						slog.Duration("duration", time.Since(start)),
					)
				}

				if p != nil {
					panic(p)
				}
			}()

			next.ServeHTTP(recordResponse(w, state), r)
		})

		return otelhttp.NewHandler(observed, "http.request", otelOpts...)
	}
}

// recordResponse wraps w so the status and size of the response are recorded
// on state. httpsnoop keeps any optional interfaces w implements (Flusher,
// Hijacker, ReaderFrom...), which a plain embedding wrapper would hide.
func recordResponse(w http.ResponseWriter, state *requestState) http.ResponseWriter {
	implicitOK := func() {
		if state.status == 0 {
			state.status = http.StatusOK
		}
	}
	return httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(code int) {
				// Only the first final status is sent; 1xx are informational.
				if state.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
					state.status = code
				}
				next(code)
			}
		},
		Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(b []byte) (int, error) {
				implicitOK()
				n, err := next(b)
				state.bytes += int64(n)
				return n, err
			}
		},
		ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(src io.Reader) (int64, error) {
				implicitOK()
				n, err := next(src)
				state.bytes += n
				return n, err
			}
		},
		Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() {
				implicitOK()
				next()
			}
		},
	})
}

// registerDuration registers the v1 request duration metric, unchanged so
// existing dashboards and alerts keep working, or reuses it if a previous
// Observe call already registered it.
func registerDuration(reg prometheus.Registerer) *prometheus.HistogramVec {
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP Request Duration",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path", "status"})

	if err := reg.Register(duration); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.HistogramVec)
		}
		panic(err)
	}
	return duration
}

// routePath strips the method (and host) from a ServeMux pattern, so
// "POST /push" becomes "/push". Requests that matched no route share one
// label value to keep metric cardinality bounded.
func routePath(pattern string) string {
	if pattern == "" {
		return "unmatched"
	}
	if _, after, ok := strings.Cut(pattern, " "); ok {
		pattern = after
	}
	if i := strings.Index(pattern, "/"); i > 0 {
		pattern = pattern[i:]
	}
	return pattern
}

func statusGroup(code int) string {
	return fmt.Sprintf("%dXX", code/100)
}
