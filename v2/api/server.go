package api

import (
	"net/http"
	"time"
)

// NewServer returns an http.Server for handler with timeouts that protect
// against slow or idle clients holding connections open.
//
// Read and write timeouts are left unset because a sensible value depends on
// the service's request sizes and handler latency; set ReadTimeout and
// WriteTimeout on the returned server where it makes sense.
func NewServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}
