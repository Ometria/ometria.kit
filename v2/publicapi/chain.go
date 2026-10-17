package publicapi

import "net/http"

// Middleware wraps a handler with extra behaviour.
type Middleware = func(http.Handler) http.Handler

// Chain composes middleware into one. The first runs outermost, so
// Chain(a, b)(h) is a(b(h)).
func Chain(mws ...Middleware) Middleware {
	return func(h http.Handler) http.Handler {
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i](h)
		}
		return h
	}
}

// phpTruthy reports whether PHP treats header value s as true, which is how
// PHP checks a header is present: "" and "0" are both false.
func phpTruthy(s string) bool {
	return s != "" && s != "0"
}

// headerValue returns the first truthy header of names, in order, as PHP's
// "$a ? $a : $b" chains do.
func headerValue(h http.Header, names ...string) string {
	for _, n := range names {
		if v := h.Get(n); phpTruthy(v) {
			return v
		}
	}
	return ""
}
