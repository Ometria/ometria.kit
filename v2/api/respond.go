package api

import (
	"encoding/json"
	"net/http"
)

// Respond writes code and, if data is non-nil, data encoded as JSON. Handlers
// don't have to use it: Observe records the status however it is written.
//
// r is unused; it keeps the v1 signature so migrating only changes the import.
func Respond(w http.ResponseWriter, r *http.Request, code int, data any) {
	if data == nil {
		w.WriteHeader(code)
		return
	}

	body, err := json.Marshal(data)
	if err != nil {
		Error(w, r, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(code)
	//nolint:errcheck
	w.Write(body)
}

// NotFound replies with an RFC 7807 404 problem response.
func NotFound(w http.ResponseWriter, r *http.Request, extras ...ProblemExtra) {
	title := http.StatusText(http.StatusNotFound)
	Problem(w, r, title, title, http.StatusNotFound, extras...)
}

// Error replies with an RFC 7807 problem response whose detail is err.
func Error(w http.ResponseWriter, r *http.Request, err string, code int, extras ...ProblemExtra) {
	Problem(w, r, http.StatusText(code), err, code, extras...)
}

// Problem replies with a response that follows RFC 7807
// (https://tools.ietf.org/html/rfc7807). It should be used for error responses.
func Problem(w http.ResponseWriter, r *http.Request, title, detail string, code int, extras ...ProblemExtra) {
	p := problem{
		"type":   "about:blank", // RFC 7807's default type.
		"title":  title,
		"detail": detail,
		"status": code,
	}
	for _, e := range extras {
		e(p)
	}

	w.Header().Set("Content-Type", "application/problem+json")
	Respond(w, r, code, p)
}

// problem holds the fields of an RFC 7807 problem response.
type problem map[string]any

// ProblemExtra adds extra information to a problem response.
type ProblemExtra func(problem)

// WithType sets the problem's type URI.
func WithType(t string) ProblemExtra {
	return func(p problem) { p["type"] = t }
}

// WithDetail sets the problem's detail.
func WithDetail(d string) ProblemExtra {
	return func(p problem) { p["detail"] = d }
}

// WithInstance sets the problem's instance URI.
func WithInstance(i string) ProblemExtra {
	return func(p problem) { p["instance"] = i }
}

// WithFields adds extra fields to the problem response. Fields that clash
// with the standard ones (type, title, detail, status) are ignored.
func WithFields(fields map[string]any) ProblemExtra {
	return func(p problem) {
		for k, v := range fields {
			switch k {
			case "type", "title", "detail", "status":
				continue
			}
			if _, exists := p[k]; !exists {
				p[k] = v
			}
		}
	}
}
