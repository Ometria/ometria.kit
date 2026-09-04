# Kit
[![Test](https://github.com/Ometria/ometria.kit/workflows/Test/badge.svg)](https://github.com/Ometria/ometria.kit/actions?query=workflow%3ATest)
[![codecov](https://codecov.io/gh/Ometria/ometria.kit/branch/main/graph/badge.svg)](https://codecov.io/gh/Ometria/ometria.kit)
[![PkgGoDev](https://pkg.go.dev/badge/github.com/Ometria/ometria.kit)](https://pkg.go.dev/github.com/Ometria/ometria.kit)

A collection of building blocks for Go apps 🧱.

## Packages

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

### package clientip

Resolves the real client IP of an incoming request behind one or more trusted proxies, by walking
the `X-Forwarded-For` chain and stopping at the first untrusted hop. Ported from
`ometria.js_tracker_pipeline`'s `getIPAddress`/`ipIsTrusted`, which has run this exact logic in
production on every tracked page view for years — extracted here so a second service needing the
same trust decision doesn't duplicate security-sensitive code a second time.

#### Example

```go
var trustedMasks []*net.IPNet
for _, cidr := range strings.Split(os.Getenv("TRUSTED_PROXY_CIDRS"), ",") {
    _, mask, _ := net.ParseCIDR(strings.TrimSpace(cidr))
    trustedMasks = append(trustedMasks, mask)
}

ip, _ := clientip.GetIPAddress(r, trustedMasks)
```
