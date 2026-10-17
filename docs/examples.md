# Router Examples and Patterns

This document provides examples and patterns for using `cf_http` with different routers.

## Table of Contents

- [stdlib](#stdlib)
- [Echo](#echo)
- [Gin](#gin)
- [chi](#chi)
- [GraphQL](#graphql)
- [Anti-patterns](#anti-patterns)

## stdlib

The standard library `net/http` is the simplest way to use `cf_http`.

```go
mux := http.NewServeMux()
mux.HandleFunc("GET /", handler)

handler := cf_http.Chain(
    cf_http.Metrics(httpServer),
    cf_http.RequestID(),
    cf_http.RequestLog(getLogger),
    cf_http.Recover(getLogger, nil),
)(mux)

httpServer.SetHandler(handler)
```

**Route Pattern**: stdlib's `ServeMux` automatically sets `r.Pattern` after dispatch, which is used for metrics.

## Echo

Echo is a popular high-performance HTTP framework for Go.

```go
e := echo.New()
e.GET("/", handler)

// Wrap cf_http middleware for Echo
e.Use(echo.WrapMiddleware(cf_http.Metrics(httpServer)))
e.Use(echo.WrapMiddleware(cf_http.RequestID()))
e.Use(echo.WrapMiddleware(cf_http.RequestLog(getLogger)))
e.Use(echo.WrapMiddleware(cf_http.Recover(getLogger, nil)))

httpServer.SetHandler(e)
```

**Route Pattern**: Echo's `c.Path()` returns the route pattern (e.g., `/users/:id`).

**Custom Metrics Middleware**: If you need custom metrics behavior:

```go
func echoMetrics(httpServer *cf_http.Server) echo.MiddlewareFunc {
    return func(next echo.HandlerFunc) echo.HandlerFunc {
        return func(c echo.Context) error {
            start := time.Now()
            err := next(c)
            duration := time.Since(start)
            cf_http.Record(httpServer, c.Path(), c.Response().Status, duration)
            return err
        }
    }
}
```

## Gin

Gin is another popular HTTP framework for Go.

```go
r := gin.New()
r.GET("/", handler)

// Gin middleware is different - apply cf_http middleware to the router
r.Use(gin.WrapH(cf_http.Metrics(httpServer)(r)))
r.Use(gin.WrapH(cf_http.RequestID()(r)))
r.Use(gin.WrapH(cf_http.RequestLog(getLogger)(r)))
r.Use(gin.WrapH(cf_http.Recover(getLogger, nil)(r)))

httpServer.SetHandler(r)
```

**Route Pattern**: Gin's `c.FullPath()` returns the route pattern (e.g., `/users/:id`).

**Custom Metrics Middleware**:

```go
func ginMetrics(httpServer *cf_http.Server) gin.HandlerFunc {
    return func(c *gin.Context) {
        start := time.Now()
        c.Next()
        duration := time.Since(start)
        cf_http.Record(httpServer, c.FullPath(), c.Writer.Status(), duration)
    }
}
```

## chi

chi is a lightweight, idiomatic HTTP router for Go.

```go
r := chi.NewRouter()
r.Get("/", handler)

// chi middleware is compatible with stdlib
r.Use(cf_http.Metrics(httpServer))
r.Use(cf_http.RequestID())
r.Use(cf_http.RequestLog(getLogger))
r.Use(cf_http.Recover(getLogger, nil))

httpServer.SetHandler(r)
```

**Route Pattern**: chi's `chi.RouteContext(r.Context()).RoutePattern()` returns the route pattern.

**Custom Metrics Middleware**:

```go
func chiMetrics(httpServer *cf_http.Server) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            start := time.Now()
            rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
            next.ServeHTTP(rec, r)
            duration := time.Since(start)
            route := chi.RouteContext(r.Context()).RoutePattern()
            cf_http.Record(httpServer, route, rec.status, duration)
        })
    }
}
```

## GraphQL

`cf_http` provides optional GraphQL operation telemetry via the `graphql`
subpackage. It works with any GraphQL-over-HTTP `http.Handler` (gqlgen,
graph-gophers, Echo/Gin/chi frontends) and records operation-level series on
top of the ordinary `/graphql` HTTP metrics.

**Default = no operation-name metrics.** Clients can invent endless
`operationName` values, so the operation series stay off until you opt in with
`OnlyOperations`. Body inspection (GraphQL-ish `Content-Type` + peek window)
is documented in [graphql-metrics.md](graphql-metrics.md): default peek **8 KiB**,
`WithPeekWindow(0)` = full read. Generate the allowlist from checked-in
`.graphql`/operation files or a persisted-query map; never auto-learn it from
live traffic.

```go
import "github.com/caerus-framework/caerus-framework-http/graphql"

// Create GraphQL handler (e.g., with gqlgen)
gqlHandler := handler.NewDefaultServer(generated.NewExecutableSchema(cfg))

// Enable operation metrics for an allowlist. Anything not in the list emits
// no operation-level sample; the HTTP /graphql metrics still count it.
gql := graphql.Metrics(httpServer,
    graphql.OnlyOperations("GetUser", "ListUsers"),
)(gqlHandler)

// Optional: collapse everything outside the allowlist into one bounded bucket.
gql = graphql.Metrics(httpServer,
    graphql.OnlyOperations("GetUser", "ListUsers"),
    graphql.WithOtherBucket(),
)(gqlHandler)

// Apply standard middleware
handler := cf_http.Chain(
    cf_http.Metrics(httpServer),
    cf_http.RequestID(),
    cf_http.RequestLog(getLogger),
    cf_http.Recover(getLogger, nil),
)(gql)

httpServer.SetHandler(handler)
```

**Operation Filtering**: Use `OnlyOperations()` to track specific operations.
Untracked operations are dropped from the operation series unless
`WithOtherBucket()` collapses them into a single `other` label.
`AllOperations()` exists as a **DANGEROUS** escape hatch that measures every
detected name — operation names are client-controlled, so it is a public
cardinality-abuse vector and must not be enabled on public endpoints.
Generate the allowlist from checked-in `.graphql`/operation files or a
persisted-query map.

**Resolver Metrics**: For resolver-level metrics, use the `RecordResolver`
hook:

```go
func (r *resolver) User(ctx context.Context, id string) (*User, error) {
    start := time.Now()
    user, err := r.db.GetUser(ctx, id)
    duration := time.Since(start)
    
    status := 200
    if err != nil {
        status = 500
    }
    
    graphql.RecordResolver(httpServer, "GetUser", "User", status, duration)
    return user, err
}
```

## Anti-patterns

### Double-counting Metrics

❌ **Wrong**: Applying both `cf_http.Metrics` and a custom router metrics middleware.

```go
// Don't do this!
handler := cf_http.Metrics(httpServer)(router)
router.Use(customMetricsMiddleware) // Double-counting!
```

✅ **Right**: Use only one metrics middleware.

```go
handler := cf_http.Metrics(httpServer)(router)
```

### Setting Handler Before Middleware

❌ **Wrong**: Setting the handler before applying middleware.

```go
httpServer.SetHandler(router)
router.Use(cf_http.Metrics(httpServer)) // Too late!
```

✅ **Right**: Apply middleware before setting the handler.

```go
handler := cf_http.Metrics(httpServer)(router)
httpServer.SetHandler(handler)
```

### Using Raw URL Paths for Metrics

❌ **Wrong**: Using `r.URL.Path` for metrics (creates unbounded cardinality).

```go
cf_http.Record(httpServer, r.URL.Path, status, duration) // /users/123, /users/456, ...
```

✅ **Right**: Use router-specific route patterns.

```go
cf_http.Record(httpServer, c.Path(), status, duration) // /users/:id
```

### Blocking the Event Loop

❌ **Wrong**: Long-running operations in middleware.

```go
func slowMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        time.Sleep(10 * time.Second) // Blocks!
        next.ServeHTTP(w, r)
    })
}
```

✅ **Right**: Keep middleware fast and non-blocking.

```go
func fastMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // Fast operation
        next.ServeHTTP(w, r)
    })
}
```

### Ignoring Graceful Shutdown

❌ **Wrong**: Not handling shutdown signals.

```go
httpServer.Run(ctx) // ctx never cancelled
```

✅ **Right**: Handle shutdown signals.

```go
ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer cancel()
httpServer.Run(ctx)
```

## Best Practices

1. **Use Chain for Middleware Composition**: `cf_http.Chain()` provides clear, ordered middleware composition.

2. **Apply Metrics First**: Apply `cf_http.Metrics` as the outermost middleware to capture all requests.

3. **Use Router-Specific Patterns**: Use router-specific route patterns for metrics to avoid cardinality explosion.

4. **Handle Shutdown Gracefully**: Always handle shutdown signals to allow graceful drain.

5. **Configure Timeouts**: Set appropriate timeouts for your use case. Use `WriteTimeoutSec: 0` for streaming endpoints.

6. **Use Problem Details for Errors**: Use the `problem` package for consistent error responses.

7. **Monitor Metrics**: Monitor `http_requests_total`, `http_request_duration_seconds`, and `http_connections_active`.

8. **Test Middleware Order**: Middleware order matters. Test your middleware chain to ensure correct behavior.
