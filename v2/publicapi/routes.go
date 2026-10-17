package publicapi

import (
	"net/http"
	"path"
	"slices"
	"strings"
)

// Routes serves mux the way PHP's Silex app routes, so requests that match
// no route get PHP's responses rather than ServeMux's:
//
//   - A path with one trailing slash is served as the path without it, as
//     PHP adds a slash to every path and registers every route with one.
//     Register routes without a trailing slash.
//   - An unclean path ("//a", "/a//", "/a/../b") is a 404, where ServeMux
//     would redirect to the clean one.
//   - No route for the path: NotFound.
//   - Routes for the path, but none for an OPTIONS request: Options, which
//     PHP answers for every route without authentication.
//   - Routes for the path, but none for the method: MethodNotAllowed.
//
// The method is upper-cased before routing, as Symfony's getMethod does, so
// "get" is served by GET routes and "options" answered like OPTIONS. Unlike
// PHP, the handlers see the upper-cased method too: PHP's BaseController
// checks scopes against REQUEST_METHOD as sent, so a "get" there skips the
// read-scope check. That bypass is deliberately not reproduced here.
//
// In both of the last two, the methods are those of ServeMux's Allow header,
// less the HEAD it adds for GET routes. ServeMux sorts them; PHP lists them
// in the order the controller defines its routes, so a path with several
// methods may list them in a different order than PHP did.
func Routes(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) > 1 && strings.HasSuffix(p, "/") {
			p = p[:len(p)-1]
		}
		if p == "" || p[0] != '/' || path.Clean(p) != p {
			NotFound(w, r)
			return
		}

		routed := r
		if p != r.URL.Path {
			routed = withPath(r, p)
		}
		if m := phpMethod(r); m != r.Method {
			if routed == r {
				c := *r
				routed = &c
			}
			routed.Method = m
		}
		h, pattern := mux.Handler(routed)
		if pattern != "" {
			mux.ServeHTTP(w, routed)
			return
		}

		// ServeMux's own 404 or 405: find out which, and for which methods.
		probe := &probeWriter{header: http.Header{}}
		h.ServeHTTP(probe, routed)
		if probe.status != http.StatusMethodNotAllowed {
			NotFound(w, r)
			return
		}
		methods := routeMethods(probe.header.Get("Allow"))
		if routed.Method == http.MethodOptions {
			Options(w, r, methods)
			return
		}
		MethodNotAllowed(w, r, methods)
	})
}

// NotFound writes PHP's 404 for a path with no route, through Error:
//
//	404 No route found for "GET /v2/push/_errors/foo/" (See http://docs.ometria.com/ for support)
//
// The path is the raw path after PHP's rewrite, so it ends in a slash.
// Symfony adds ` (from "<referer>")` before the support link when the
// request has a Referer.
func NotFound(w http.ResponseWriter, r *http.Request) {
	msg := `No route found for "` + phpMethod(r) + " " + phpPathInfo(r) + `"`
	if ref := r.Header.Get("Referer"); phpTruthy(ref) {
		msg += ` (from "` + ref + `")`
	}
	Error(w, r, http.StatusNotFound, msg+" (See http://docs.ometria.com/ for support)")
}

// MethodNotAllowed writes PHP's 405 for a path whose routes, for methods,
// don't include the request's, through Error:
//
//	405 No route found for "POST /v2/push/_errors/": Method Not Allowed (Allow: GET, OPTIONS)
//
// PHP registers an OPTIONS route for every path, so OPTIONS is appended to
// methods unless they hold it. Like PHP, it sends no Allow header.
func MethodNotAllowed(w http.ResponseWriter, r *http.Request, methods []string) {
	if !slices.Contains(methods, http.MethodOptions) {
		methods = append(slices.Clip(methods), http.MethodOptions)
	}
	msg := `No route found for "` + phpMethod(r) + " " + phpPathInfo(r) +
		`": Method Not Allowed (Allow: ` + strings.Join(methods, ", ") + ")"
	Error(w, r, http.StatusMethodNotAllowed, msg)
}

// Options writes PHP's answer to OPTIONS for a path whose routes are for
// methods: a 200 with "Allow: GET" (methods joined by commas, without
// spaces), Content-Type "text/html; charset=UTF-8" and no body.
func Options(w http.ResponseWriter, r *http.Request, methods []string) {
	w.Header().Set("Allow", strings.Join(methods, ","))
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
}

// routeMethods turns ServeMux's Allow header into the route's methods.
func routeMethods(allow string) []string {
	var methods []string
	for m := range strings.SplitSeq(allow, ",") {
		if m = strings.TrimSpace(m); m != "" {
			methods = append(methods, m)
		}
	}
	if slices.Contains(methods, http.MethodGet) {
		methods = slices.DeleteFunc(methods, func(m string) bool { return m == http.MethodHead })
	}
	return methods
}

// withPath returns a shallow copy of r with path p.
func withPath(r *http.Request, p string) *http.Request {
	c := *r
	u := *r.URL
	u.Path, u.RawPath = p, ""
	if r.URL.RawPath != "" {
		u.RawPath = strings.TrimSuffix(r.URL.RawPath, "/")
	}
	c.URL = &u
	return &c
}

// probeWriter records the status and headers a handler writes, and drops
// the body.
type probeWriter struct {
	header http.Header
	status int
}

func (p *probeWriter) Header() http.Header { return p.header }

func (p *probeWriter) WriteHeader(code int) {
	if p.status == 0 {
		p.status = code
	}
}

func (p *probeWriter) Write(b []byte) (int, error) {
	p.WriteHeader(http.StatusOK)
	return len(b), nil
}
