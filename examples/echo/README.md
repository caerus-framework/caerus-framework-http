# Echo Example

This example demonstrates an Echo HTTP server using `cf_http`.

## Features

- Uses Echo framework for routing and middleware
- Wraps `cf_http` middleware using Echo's `WrapMiddleware`
- Demonstrates how to integrate `cf_http` with any `http.Handler`-compatible framework
- Graceful shutdown on SIGINT/SIGTERM
- Configuration via `config/http.json`

## Running

```bash
# Create config directory
mkdir -p config

# HTTP server config (logs/observability have their own core config files)
cat > config/http.json <<EOF
{
  "bind": ":8080",
  "read_timeout_sec": 10,
  "write_timeout_sec": 10,
  "idle_timeout_sec": 60
}
EOF

cat > config/logs.json <<EOF
{
  "format": "json",
  "level": "info"
}
EOF

cat > config/observability.json <<EOF
{
  "bind": ":9090"
}
EOF

# Run the example (GOWORK=off: this is its own module, separate from go.work)
GOWORK=off go run .
```

## Testing

```bash
# Health check
curl http://localhost:8080/health

# Main endpoint
curl http://localhost:8080/

# Route-pattern metrics (path label /users/:id, not the raw id)
curl http://localhost:8080/users/42
curl http://localhost:9090/metrics | grep http_requests_total
```

## Key Points

1. **Echo as http.Handler**: Echo's `*echo.Echo` implements `http.Handler`, so it can be used directly with `cf_http.SetHandler()`.

2. **Middleware Wrapping**: Use `echo.WrapMiddleware()` to adapt `cf_http` middleware to Echo's middleware signature.

3. **Route Parameters**: Echo's route parameters (e.g., `/users/:id`) are automatically tracked in metrics via `c.Path()`.

4. **Error Handling**: Echo's error handling works seamlessly with `cf_http.Recover()`.

5. **Configuration**: Server configuration is loaded from `config/http.json` and can be reloaded at runtime.

## Differences from stdlib Example

- Uses Echo's routing and middleware system
- Demonstrates framework integration pattern
- Shows how to wrap middleware for non-stdlib routers
- Echo's built-in features (binding, validation, etc.) remain available
