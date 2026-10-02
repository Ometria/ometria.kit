package api

import (
	"net/http"
	"net/url"
	"path"
	"strings"
)

// V1Redirects keeps v1's (httptreemux's) handling of paths that only match a
// route once they're tidied up, for services migrating from v1:
//
//   - A trailing slash ("/push/") or an unclean path ("//push", "/a/../push")
//     whose tidied path has a route for the request's method gets a
//     301 Moved Permanently to that path, keeping the query string.
//     http.ServeMux on its own responds 404 to the first and 307 to the second.
//   - A trailing slash whose path has routes only for other methods gets a
//     405 Method Not Allowed, where http.ServeMux responds 404.
//
// Note that most clients follow a 301 for a POST by sending a GET, so these
// redirects keep v1's behaviour rather than making such requests succeed.
func V1Redirects(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested := r.URL.Path
		clean := cleanPath(requested)
		if clean == requested {
			if _, pattern := mux.Handler(r); pattern != "" {
				mux.ServeHTTP(w, r)
				return
			}
		}

		trimmed := clean
		if len(trimmed) > 1 {
			trimmed = strings.TrimSuffix(trimmed, "/")
		}
		if trimmed == requested {
			// Clean, no trailing slash and no route: ServeMux's 404 or 405.
			mux.ServeHTTP(w, r)
			return
		}

		for _, candidate := range []string{clean, trimmed} {
			if candidate == requested {
				continue
			}
			if _, pattern := mux.Handler(withPath(r, candidate)); pattern != "" {
				target := url.URL{Path: candidate, RawQuery: r.URL.RawQuery}
				http.Redirect(w, r, target.String(), http.StatusMovedPermanently)
				return
			}
		}

		// No route for the method: respond as the tidied path would, which
		// is a 405 if it has routes for other methods.
		tidied := withPath(r, trimmed)
		h, _ := mux.Handler(tidied)
		h.ServeHTTP(w, tidied)
	})
}

// withPath returns a shallow copy of r for path, for looking up routes.
func withPath(r *http.Request, p string) *http.Request {
	c := *r
	u := *r.URL
	u.Path, u.RawPath = p, ""
	c.URL = &u
	return &c
}

// cleanPath is net/http's canonical form of p: path.Clean, keeping a trailing
// slash.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if strings.HasSuffix(p, "/") && np != "/" {
		np += "/"
	}
	return np
}
