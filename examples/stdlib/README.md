# stdlib Example

This example demonstrates a simple HTTP server using Go's standard library `net/http` with `cf_http`.

## Features

- Uses `http.ServeMux` for routing
- Applies standard middleware chain:
  - `Metrics` - records request metrics
  - `RequestID` - generates/propagates request IDs
  - `RequestLog` - logs requests with structured logging
  - `Recover` - recovers from panics
- Graceful shutdown on SIGINT/SIGTERM
- Configuration via `config/http.json`

## Running

```bash
# Create config directory
mkdir -p config

# HTTP server config (logs/observability have their own core config files)
cat > config/http.json <<EOF
{
  "address": ":8080",
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
  "address": ":9090"
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

# Platform probes (readiness + metrics)
curl http://localhost:9090/readyz
curl http://localhost:9090/metrics
```

## Key Points

1. **Handler Registration**: The handler is set via `httpServer.SetHandler(handler)` after middleware is applied.

2. **Middleware Chain**: Use `cf_http.Chain()` to compose middleware in order. The first middleware is outermost.

3. **Graceful Shutdown**: The framework handles graceful shutdown automatically when signals are received.

4. **Configuration**: Server configuration is loaded from `config/http.json` and can be reloaded at runtime.

5. **Metrics**: Request metrics are automatically recorded and exposed via the observability component.
