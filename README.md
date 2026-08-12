# caerus-framework-http

[![CI](https://github.com/caerus-framework/caerus-framework-http/actions/workflows/ci.yml/badge.svg)](https://github.com/caerus-framework/caerus-framework-http/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/caerus-framework/caerus-framework-http/graph/badge.svg)](https://codecov.io/gh/caerus-framework/caerus-framework-http)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

`caerus-framework-http` owns the HTTP serving lifecycle for Caerus services:
configuration, timeouts, graceful drain, request telemetry, and common stdlib
middleware. It serves any Go `net/http`-compatible handler.

The router and routes remain app-owned. Echo, Gin, chi, `http.ServeMux`, and
GraphQL handlers can be registered without making this module depend on them.

## Wiring

Docs: [`docs/wiring-and-health.md`](docs/wiring-and-health.md) ·
[`docs/reload.md`](docs/reload.md) · [`docs/long-lived-connections.md`](docs/long-lived-connections.md) ·
[`docs/examples.md`](docs/examples.md) · [`docs/errors.md`](docs/errors.md)

### App-owned consumer (golden path)

Declare the HTTP chassis beside the data-plane components. The app resolves it
at `Init`, builds its own router, composes middleware, and registers the final
handler.

```go
httpServer := cf_http.New(
    cf_http.WithConfigSource("http", "config/http.json"),
)

fw := cf.New(&cf.FrameworkOptions{
    Components: []cf.CaerusComponent{
        postgres,
        valkey,
        httpServer,
        app.New(),
    },
})
```

```go
func (a *API) GetDependencies() []string {
    return []string{cf_http.ComponentName}
}

func (a *API) Init(ctx context.Context, fw *cf.CaerusFramework) error {
    server, ok := cf.Get[*cf_http.Server](fw)
    if !ok {
        return errors.New("http component missing")
    }

    mux := http.NewServeMux()
    registerRoutes(mux)
    handler := cf_http.Chain(
        cf_http.Metrics(server),
        cf_http.RequestID(),
        cf_http.Recover(a.Logger, nil),
    )(mux)
    server.SetHandler(handler)
    return nil
}
```

The same boundary works with Echo, Gin, chi, or a GraphQL `http.Handler`. Router
specific route labels are supplied by the app through the documented `Record`
or GraphQL helpers. REST series carry `http_instrumentation=middleware|app`
(middleware = `cf_http.Metrics`; app = explicit `Record` / router shim). That
is normal for REST — watch `route="unknown"` and double-counting, not
`middleware` itself. See [docs/wiring-and-health.md](docs/wiring-and-health.md).

### Simple wiring

For a one-off binary, add the component directly and resolve it with
`cf.MustGet` after initialization.

## Configuration

`WithConfigSource("http", "config/http.json")` self-registers the `http` source.
The source uses the `HTTP_` environment prefix and the `--http` file-path flag.
Address and server timeouts are restart-required; metrics enablement reloads
live. `restart_policy` (`handled` default, or `immediate`) selects what happens
when a restart-required setting changes on reload — see
[`docs/reload.md`](docs/reload.md). TLS, PROXY protocol, and forwarded-header
normalization belong to the Ingress, mesh, reverse proxy, or load balancer in
front of this component.

## Security middleware

Optional `CORS`, `CSRF`, and `Compression` middleware live in this module.
They are opt-in and typed: you only get the behavior you configure.

### CORS: one rule, enforced at build time

CORS has a rule like a club bouncer: **"you can't say everyone is allowed
(`*`) and bring your cookies (credentials) at the same time."** Browsers
reject that combination — `Access-Control-Allow-Origin: *` plus
`Access-Control-Allow-Credentials: true` never works, and any server that
sends both is inviting a security review.

Prefer **`CORSConfig.Validate()`** in `Init` (or wiring) and return the error
(`ErrCORSCredentialsWildcard`). `CORS(cfg)` still **panics** on the same rule
as a last-line construction guard — the function returns only `Middleware`,
not `(Middleware, error)`, so a missed Validate cannot serve a browser-rejected
policy:

```go
cfg := cf_http.CORSConfig{
    AllowCredentials: true,
    AllowedOrigins:   []string{"*"},
}
if err := cfg.Validate(); err != nil {
    return err // framework Init path
}
cors := cf_http.CORS(cfg) // panics if Validate was skipped on a bad combo
```

Fix it the way a correct config would look — either name your origins
explicitly with credentials, or use `*` without credentials:

```go
cf_http.CORS(cf_http.CORSConfig{
    AllowCredentials: true,
    AllowedOrigins:   []string{"https://app.example.com"},
})
```

### CSRF and compression

- **`CSRF(cfg)`** — double-submit cookie pattern with Origin/Referer checks.
  Safe methods mint and set a cookie; unsafe methods are rejected (403) when
  Origin and Referer are both missing, the token is missing, or the token does
  not match. `Secure` defaults to true (HTTPS-only cookie). See
  `docs/errors.md` to route rejections through an `ErrorWriter`.
- **`Compression(cfg)`** — gzip responses above `MinSize` for clients that
  accept gzip. Never compresses `text/event-stream` (SSE stays live) and
  forwards WebSocket hijacks untouched.

## Telemetry

The standard middleware records request count, status class, duration, and
in-flight requests. The component also reports lifecycle metrics. Applications
own business metrics by implementing `cf_observability.MetricsProvider`.

GraphQL operation metrics are available from the optional `graphql` package.
REST applications may use the optional `problem` package for RFC 9457 responses;
GraphQL and OAuth handlers retain their native error envelopes.

## GraphQL operation metrics

The `cf_http/graphql` package wraps any GraphQL-over-HTTP `http.Handler`
(gqlgen, graph-gophers, Echo/Gin/chi frontends) and records operation-level
metrics on top of the ordinary `/graphql` HTTP metrics.

- **Default = no operation-name metrics.** Clients can invent endless
  `operationName` values; the series stay off until you opt in, and the
  middleware does **not** read or parse the request body in that mode.
- **`OnlyOperations("GetUser", "ListUsers")`** turns named series on for that
  allowlist only. Generate the list from checked-in `.graphql`/operation files
  or a persisted-query map — never auto-learn it from live traffic.
- **`WithOtherBucket()`** (optional) collapses everything outside the
  allowlist into one bounded `other` label.
- **`AllOperations()`** measures every detected name. **DANGEROUS**: operation
  names are client-controlled, so this is a public cardinality-abuse vector —
  documented as an escape hatch only, not for public endpoints.
- **`WithPeekWindow(n)`** (optional) — how many leading POST bytes may be
  inspected for `operationName` when tracking is on: **omit → 8 KiB**,
  **`n > 0` → peek n**, **`0` → full body read** (costly; opt-in).
  **Tradeoff:** named series require inspecting the request; default peek
  bounds that cost — details in
  [docs/graphql-metrics.md](docs/graphql-metrics.md).

Emitted series (only while operation metrics are enabled):
`http_graphql_operations_total{operation,status_class,graphql_instrumentation}`,
`http_graphql_operation_duration_seconds_sum/count`, plus resolver series from
`RecordResolver`. `graphql_instrumentation` is `http_peek` (auto body-peek
middleware) or `app` (engine/explicit hooks) — filter on `http_peek` in
dev to find leftover auto-instrumentation. Ordinary `http_requests_total`
for the `/graphql` route remains in every mode.

See [docs/examples.md](docs/examples.md) and
[docs/graphql-metrics.md](docs/graphql-metrics.md).

## License

Apache License 2.0. See `LICENSE` and `NOTICE`.
