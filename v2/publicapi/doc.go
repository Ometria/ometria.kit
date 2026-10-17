// Package publicapi provides the layer Ometria's public APIs share on top of
// package api: authentication from the API gateway's headers, access scopes,
// the error bodies clients already depend on, and the per-request API log.
//
// It exists so endpoints can move from the PHP ometria.api into Go without
// clients noticing: statuses, messages and error bodies match PHP's
// (www/api.ometria.com/index.php, Api2\Controllers\BaseController and
// Api2\MessageFormatterTrait). It only uses the standard library.
//
// # Trust boundary
//
// Authenticate trusts the Ometria-Account-Id and Ometria-Access-Scopes
// headers completely; it does not check the API key. Only use it on routes
// where Skipper drops any client-sent Ometria-* headers before its auth
// webhook validates the key and sets them. Exposing a route that uses this
// package any other way lets a client choose its account and scopes.
//
// # Usage
//
// PHP routes a request before it authenticates it, so an unknown path is a
// 404 and a wrong method a 405 even without auth headers. Keep that order by
// wrapping each route's handler in Authenticate and RequireScopeByMethod, and
// the whole mux in Routes, RequestLog and Recover:
//
//	authed := publicapi.Chain(
//		publicapi.Authenticate(accounts),
//		publicapi.RequireScopeByMethod(),
//	)
//
//	mux := http.NewServeMux()
//	mux.Handle("GET /v2/push/_errors", authed(errorsHandler))
//
//	handler := publicapi.Chain(
//		api.Observe(api.Config{Logger: logger}),
//		publicapi.RequestLog(sink, publicapi.WithRequestID(func(r *http.Request) string {
//			return api.RequestID(r.Context())
//		})),
//		publicapi.Recover(logger),
//	)(publicapi.Routes(mux))
//
// Register routes without a trailing slash: Routes serves "/v2/push/_errors"
// and "/v2/push/_errors/" alike, as PHP does, instead of redirecting.
//
// Handlers report errors with Error, which writes the body of the encoder
// installed with UseEncoder, LegacyEncoder by default. Error(w, r, 400, msg)
// matches an exception caught by PHP's error handler; add WithRequest() to
// match a controller's apiResponseError, which also echoes the method and URL.
//
// # Differences from PHP
//
// PHP turns an account lookup failure into a fatal error ("APIError" is not
// imported in BaseController), whose shutdown handler writes a 500 "Internal
// server error" body. Authenticate writes the same response, but RequestLog
// still emits a record for it, where PHP's finish hook never runs. Other
// differences are noted where they apply.
package publicapi
