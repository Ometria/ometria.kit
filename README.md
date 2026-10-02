# Kit
[![Test](https://github.com/Ometria/ometria.kit/workflows/Test/badge.svg)](https://github.com/Ometria/ometria.kit/actions?query=workflow%3ATest)
[![codecov](https://codecov.io/gh/Ometria/ometria.kit/branch/main/graph/badge.svg)](https://codecov.io/gh/Ometria/ometria.kit)
[![PkgGoDev](https://pkg.go.dev/badge/github.com/Ometria/ometria.kit)](https://pkg.go.dev/github.com/Ometria/ometria.kit)

A collection of building blocks for Go apps 🧱.

## v2 (draft)

`github.com/Ometria/ometria.kit/v2/api` replaces the v1 framework with
middleware for the standard library's `http.ServeMux`: one request log line,
the same `http_request_duration_seconds` metric, and OpenTelemetry spans for
every request, with the status code captured from the `ResponseWriter` so
handlers can write responses however they like. See the package docs in
[`v2/api/doc.go`](v2/api/doc.go) for usage and a v1 migration guide.

```go
mux := http.NewServeMux()
mux.Handle("POST /push", pushHandler)

srv := api.NewServer(":8080", api.Observe(api.Config{Logger: logger})(mux))
srv.ListenAndServe()
```

## Packages (v1)

### package api

This package provides building blocks for HTTP APIs. There is one main interface and one main function that are used
to interact with this package.

- `API`: This interface defines an API, and concrete implementations of an API should be registered with a server, which is returned by;
- `NewServer`: This function takes an API, and returns a `http.Server`.

These two should be used in conjunction to provide a conformant experience across many HTTP APIs.

#### Example

```go
...

type MyAPI struct {
    logger *zap.SugaredLogger
}

func (a *MyAPI) Endpoints() []api.Endpoint {
    return []api.Endpoint{
        {"GET", "/:id", a.handleGet(), []api.Middleware{}},
    }
}

func (a *MyAPI) handleGet() http.Handler {
    var h http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
        api.Respond(w, r, http.StatusOK, nil)
    }
    return h
}

func main() {
    ...
    a := MyAPI{logger}
    srv := api.NewServer(":8080", logger, a)
    srv.ListenAndServe()
    ...
}
```
