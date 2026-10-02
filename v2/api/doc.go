// Package api provides the building blocks Ometria HTTP services share: one
// request log line, RED metrics and tracing for every request, RFC 7807
// problem responses, and an http.Server with safe defaults.
//
// Unlike v1 it is not a framework. Routing is left to the standard library's
// http.ServeMux, and handlers write responses however they like: Observe
// captures the status code from the ResponseWriter itself, so nothing depends
// on handlers calling a particular helper.
//
//	mux := http.NewServeMux()
//	mux.Handle("POST /push", pushHandler)
//	mux.HandleFunc("GET /healthz", healthz)
//
//	handler := api.Observe(api.Config{
//		Logger:      logger,
//		QuietRoutes: []string{"GET /healthz"},
//	})(mux)
//
//	srv := api.NewServer(":8080", handler)
//	srv.ListenAndServe()
//
// # Migrating from v1
//
//   - api.NewServer(addr, logger, a) and Endpoints() become an http.ServeMux
//     wrapped in Observe, passed to NewServer. Paths like "/accounts/:id"
//     become "GET /accounts/{id}", and api.URLParam(r, "id") becomes
//     r.PathValue("id").
//   - Wrap the mux in V1Redirects to keep httptreemux's redirects for
//     trailing slashes and unclean paths, as 308s instead of 301s so POSTs
//     survive them; http.ServeMux alone responds 404 to "/push/" and 307 to
//     "//push".
//   - Endpoint.SuppressLogs becomes Config.QuietRoutes.
//   - CorsMiddleware is gone: wrap the handler with github.com/rs/cors
//     directly, e.g. cors.Default().Handler(h).
//   - Respond, Problem, Error, NotFound and the ProblemExtra options keep their
//     v1 signatures, so only the import path changes.
//   - Loggers are *slog.Logger. Create them with the logging package, whose
//     output matches zap's production JSON, so log queries keep working.
//   - api.LoggerFromRequest(r, l) becomes l.With("request_id",
//     api.RequestID(r.Context())).
//   - The http_request_duration_seconds metric keeps its name and labels, so
//     existing dashboards and alerts keep working.
package api
