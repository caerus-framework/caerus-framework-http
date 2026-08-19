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
Bind and server timeouts are restart-required; metrics enablement reloads
live. `restart_policy` (`handled` default, or `immediate`) selects what happens
when a restart-required setting changes on reload — see
[`docs/reload.md`](docs/reload.md). TLS, PROXY protocol, and forwarded-header
normalization belong to the Ingress, mesh, reverse proxy, or load balancer in
front of this component.

`WithWaitForHealth(timeout)` (optional) delays `ListenAndServe` until all
framework `cf.HealthProvider` components are healthy, or the timeout elapses.
Use it to reduce the \"port is open but data deps are not\" window in
non-Kubernetes environments.

## Security middleware

Optional `CORS`, `CSRF`, `SecurityHeaders`, and `Compression` middleware live in this module.
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

### CSRF: pick one mode

`CSRF(cfg)` is **opt-in**. Cookie-session apps need it; `Authorization: Bearer`
APIs usually do not (the browser will not attach that header by itself).

**Mode owns the whole product.** Empty `Mode` is `synchronizer`. Do not set
HttpOnly yourself — there is no HttpOnly field.

| `Mode` | Who it is for | Cookie | Unsafe POST must also send |
|---|---|---|---|
| **`synchronizer`** (default) | HTML forms, or an SPA that copies a token the **server** put in HTML/JSON/a GET header | HttpOnly (JS cannot read it) | Origin/Referer **and** header or form field matching the cookie |
| **`double_submit`** | SPA that reads `document.cookie` into `X-CSRF-Token` | **not** HttpOnly | Origin/Referer **and** matching header/form |
| **`origin_only`** | You only want the Origin belt (strict Ingress, no token UX) | none | Origin/Referer **only** |

All three **fail closed** when an unsafe method has neither `Origin` nor
`Referer`. Origin is compared to `r.Host` — sit behind an edge that owns
Host.

```text
Wrong: “HttpOnly cookie, SPA copies it into the header.”
Right: synchronizer + echo the token from CSRFTokenFrom / ExposeTokenHeader,
       or double_submit (readable cookie). Those are different modes.
```

Call `CSRFConfig.Validate()` in `Init` and return the error (unknown Mode,
`ExposeTokenHeader` with `origin_only`, or a TrustedHosts entry that looks
like a URL). `CSRF(cfg)` panics on the same errors because it returns only
`Middleware`:

```go
cfg := cf_http.CSRFConfig{
    Mode:         cf_http.CSRFSynchronizer,
    TrustedHosts: []string{"api.example.com"},
}
if err := cfg.Validate(); err != nil {
    return err // framework Init path
}
csrf := cf_http.CSRF(cfg) // panics if Validate was skipped on a bad combo
```

**Origin vs Host.** Empty `TrustedHosts` compares Origin/Referer to `r.Host`.
That is safe only behind an edge that **sets** Host (Ingress). A client can
otherwise send `Host: evil.example` and `Origin: https://evil.example` and
they match. Non-empty `TrustedHosts` is the allowlist: Origin host must be
in that list (`host` or `host:port`, no scheme); `r.Host` is ignored.

```text
Wrong: CSRF on a raw socket with empty TrustedHosts, trusting r.Host.
Right: TrustedHosts: []string{"api.example.com"} when nothing rewrites Host.
```

**Synchronizer — two ways to give the page the token** (construct options; default
is app echo only):

**App echo (`CSRFTokenFrom`)** — always available in `synchronizer` /
`double_submit`. On the GET that mints the cookie, the handler can read the
token (context, not `document.cookie`) and put it in HTML or JSON:

```go
mux.HandleFunc("/form", func(w http.ResponseWriter, r *http.Request) {
    token := cf_http.CSRFTokenFrom(r)
    fmt.Fprintf(w, `<input type="hidden" name="csrf_token" value="%s">`, token)
})
```

**Response header (`ExposeTokenHeader: true`)** — middleware sets
`X-CSRF-Token` on **GET and HEAD** (not OPTIONS, so CORS preflights do not
see it). A same-origin SPA reads `response.headers.get("X-CSRF-Token")`.
Default **false**. Wrap the **API mux only**. Do not put this in front of a
CDN-cached public GET (the header is a secret; a shared cache can leak it).
Cross-origin fetch also needs CORS `Access-Control-Expose-Headers`.

```go
cf_http.CSRF(cf_http.CSRFConfig{}) // synchronizer, app echoes via CSRFTokenFrom
cf_http.CSRF(cf_http.CSRFConfig{ExposeTokenHeader: true}) // same mode, GET header too
cf_http.CSRF(cf_http.CSRFConfig{Mode: cf_http.CSRFDoubleSubmit})
cf_http.CSRF(cf_http.CSRFConfig{Mode: cf_http.CSRFOriginOnly})
```

HTML forms may send `csrf_token` instead of the header (`FormField`, default
`csrf_token`). JSON bodies are not parsed as forms. `Secure` defaults to true
(HTTPS-only cookie). See `docs/errors.md` to route rejections through an
`ErrorWriter`.

XSS on your own origin can still call your API as the user. Keep the
**session** cookie HttpOnly regardless of CSRF mode.

### Security headers

Optional `SecurityHeaders(cfg)` sets `X-Content-Type-Options: nosniff` by
default (set `NoSniff: false` to skip). HSTS is **off** until `HSTSMaxAge`
is a positive number of seconds — do not turn it on for a plain-HTTP laptop
listener. Prefer `SecurityHeadersConfig.Validate()` in `Init` (negative
max-age); `SecurityHeaders(cfg)` panics on the same error.

```go
cf_http.SecurityHeaders(cf_http.SecurityHeadersConfig{
    HSTSMaxAge:            31536000, // one year
    HSTSIncludeSubdomains: true,
})
```

The edge can still send these. This helper is for apps that terminate TLS
in-process or want nosniff without waiting on Ingress.

### Compression

- **`Compression(cfg)`** — gzip responses above `MinSize` for clients that
  accept gzip. Only JSON, HTML, and other `text/*` (not SSE). Images and
  `octet-stream` pass through. `WriteHeader` is delayed until the body is
  large enough so `Content-Encoding` is not applied too late. WebSocket
  hijacks stay untouched.
  **BREACH:** gzip can leak secrets if the same response mixes a secret
  (CSRF token, session fragment) with attacker-controlled text (a search
  query reflected in HTML). Do not compress those pages; keep secrets off
  gzip’d HTML/JSON that includes user input.
- **`RequestLog(get)`** — access line with method, route, status, duration,
  request ID, and **partial** `client_ip` (IPv4 `/24`, IPv6 `/48` via
  `cf_logs.ClientIP`). `RequestLogWith` sets `omit` / `full` or a getter
  for an identity the **app** already trusts. This module never reads
  `X-Forwarded-For`. Query, body, and cookies stay off.
- **`MaxBodyBytes(n, write)`** — opt-in 413 when the request body is larger
  than `n` bytes (`n <= 0` is a no-op). Not on the Server by default: a global
  limit would break uploads and large GraphQL variables. Wrap JSON POST
  routes (auth-api JSON POSTs should); leave multipart upload routes off or
  on a much larger `n`. Rejections go through `ErrorWriter` (nil =
  `DefaultErrorWriter`); use the same writer as Recover / CSRF, or
  `problem.Write`. Handlers that read the body themselves can call
  `IsBodyTooLarge(err)` after `Read` / `Decode`.

```text
Wrong: Chain(..., MaxBodyBytes(1<<20, nil))(mux) when mux also serves
       multipart uploads.
Right: jsonMux := MaxBodyBytes(1<<20, nil)(jsonRoutes)
```

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
