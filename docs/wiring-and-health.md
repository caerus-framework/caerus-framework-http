# Wiring and Health

How `cf_http` fits into a Caerus framework application: where the component
lives, how it is wired, and how readiness works.

## Golden path: app-owned consumer

Declare the HTTP server as chassis in `main`, next to postgres/valkey, and let
an app component own the handler. The app component resolves the peer at
`Init`, lists it in `GetDependencies`, and installs the handler via
`SetHandler`.

```go
// main
fw := cf.New(&cf.FrameworkOptions{
    Logs: &cf.LogsSettings{Format: "json", Level: "info", ConfigSource: "logs"},
    Observability: &cf.ObservabilitySettings{ConfigSource: "observability"},
    Components: []cf.CaerusComponent{
        cf_postgres.New(...),
        cf_valkey.New(...),
        httpServer, // cf_http.New(cf_http.WithConfigSource("http", "config/http.json"))
        app,       // product component (see below)
    },
})
fw.RunWithSignals(ctx, cf.WithInitTimeout(30*time.Second), cf.WithShutdownTimeout(10*time.Second))
```

```go
// app component
type MyAPI struct {
    name     string
    httpSrv  *cf_http.Server
}

func (a *MyAPI) Name() string                { return a.name }
func (a *MyAPI) GetInitOrderStage() cf.Stage { return cf.Stage("app") }
func (a *MyAPI) GetDependencies() []string   { return []string{cf_http.ComponentName} }

func (a *MyAPI) Init(ctx context.Context, fw *cf.CaerusFramework) error {
    srv, ok := cf.Get[*cf_http.Server](fw) // GetByName when WithName
    if !ok {
        return errors.New("http server not registered")
    }
    a.httpSrv = srv

    mux := http.NewServeMux()
    mux.HandleFunc("GET /healthz", a.health)
    mux.HandleFunc("GET /api/v1/users", a.listUsers)

    handler := cf_http.Chain(
        cf_http.RequestID(),
        cf_http.Recover(getLogger, nil),
        cf_http.RequestLog(getLogger),
        cf_http.Metrics(srv),
    )(mux)

    srv.SetHandler(handler)
    return nil
}
```

Route patterns on Go 1.22+ `ServeMux` (e.g. `GET /api/v1/users`) are reported
verbatim in `http_requests_total{route=...}`. When `r.Pattern` is not
available (bare `http.HandlerFunc` or non-`ServeMux` routers), the route
falls back to the fixed label `"unknown"` — never the literal request path, so
unbounded paths cannot explode metric cardinality. Prefer a router that sets
`r.Pattern` (ServeMux 1.22+, or the Echo `c.Path()` shim in the echo example)
for meaningful route labels.

### REST instrumentation labels (`http_instrumentation`)

Unlike GraphQL body-peek, **HTTP middleware metrics are the industry-normal
path** for REST. Samples carry `http_instrumentation`:

| Value | Source |
|---|---|
| **`middleware`** | `cf_http.Metrics(server)` — usual ServeMux / Pattern path |
| **`app`** | Explicit `cf_http.Record(...)` — Echo/chi shims, custom middleware |

Dev / staging smells to watch:

```promql
# Router not setting a pattern (fix wiring):
http_requests_total{route="unknown"}

# Same logical traffic counted twice (middleware + shim both installed):
http_requests_total{http_instrumentation="middleware"}
http_requests_total{http_instrumentation="app"}
```

`app` alone with a real `route` label (Echo `c.Path()` shim, no
`cf_http.Metrics`) is fine — that is still standard REST instrumentation.
The GraphQL `graphql_instrumentation="http_peek"` warning does **not** apply
here; see [graphql-metrics.md](graphql-metrics.md) for that distinction.

## Simple path: one-off binary

```go
fw := cf.New(&cf.FrameworkOptions{Components: []cf.CaerusComponent{httpServer}})
cf.AddComponent(fw, httpServer)
// ...
srv, _ := cf.MustGet[*cf_http.Server](fw)
srv.SetHandler(mux)
```

## Component facts

- `ComponentName = "http"`, `ComponentStage = cf.Stage("app")` — the serving
  plane, drained by the framework's run lifecycle.
- The module is **self-sufficient**: `WithConfigSource(name, path, opts…)`
  registers its own `Source[ServerConfig]` (config component required; bare
  frameworks absorb nothing). `EnvPrefix` defaults to `NAME_` and can be
  overridden with `WithSourceEnvPrefix`; `--<name>` overrides the config file
  path; `--http-*` flags overlay individual settings at process start.
- `WithName` gives multiple named instances; resolve peers with `GetByName`.

## Health and readiness

`cf_http.Server` implements `cf.HealthProvider`: `Health(ctx)` is healthy
only while initialized, a handler is set, **and Run has a live listener**.
Init plus `SetHandler` is not enough — that is the job path (no `Run`) and
the window before bind. Observability `/readyz` includes this check, so
Kubernetes stops sending traffic until the port is claimed, and again as
soon as drain starts (readiness fails before in-flight requests finish).

## See also

- `reload.md` — how live config reload interacts with listening settings
- `long-lived-connections.md` — WebSocket/SSE and `WriteTimeout`
- `examples.md` — full copy-paste service examples
- `graphql-metrics.md` — operationName body peek / `WithPeekWindow`
