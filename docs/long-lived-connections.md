# Long-Lived Connections

This document explains how to handle long-lived connections (streaming, WebSockets, Server-Sent Events) with `cf_http`.

## Overview

`cf_http` is designed for request-response HTTP traffic. Long-lived connections require special configuration to avoid timeouts and ensure graceful shutdown.

## WriteTimeout

The `WriteTimeout` setting controls how long the server will wait for a response to be written. For long-lived connections, this must be set to 0 (disabled) or a very large value.

### Configuration

```json
{
  "bind": ":8080",
  "read_timeout_sec": 10,
  "write_timeout_sec": 0,
  "idle_timeout_sec": 60
}
```

Or via code:

```go
httpServer := cf_http.New(
    cf_http.WithWriteTimeout(0), // Disable write timeout
)
```

### Why Zero?

- **Streaming responses**: The response is written incrementally over time
- **WebSockets**: The connection remains open indefinitely
- **Server-Sent Events**: Events are sent over a long-lived connection

If `WriteTimeout` is set to a non-zero value, the connection will be closed when the timeout is reached.

## Streaming Responses

For streaming responses (e.g., large file downloads, real-time data):

```go
func streamHandler(w http.ResponseWriter, r *http.Request) {
    // Disable buffering
    w.Header().Set("Content-Type", "application/octet-stream")
    w.Header().Set("Cache-Control", "no-cache")
    
    flusher, ok := w.(http.Flusher)
    if !ok {
        http.Error(w, "Streaming not supported", http.StatusInternalServerError)
        return
    }
    
    for i := 0; i < 10; i++ {
        fmt.Fprintf(w, "data: %d\n\n", i)
        flusher.Flush()
        time.Sleep(1 * time.Second)
    }
}
```

### Configuration

```json
{
  "write_timeout_sec": 0
}
```

## Server-Sent Events (SSE)

For Server-Sent Events:

```go
func sseHandler(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("Connection", "keep-alive")
    
    flusher, ok := w.(http.Flusher)
    if !ok {
        http.Error(w, "Streaming not supported", http.StatusInternalServerError)
        return
    }
    
    for {
        select {
        case <-r.Context().Done():
            return
        default:
            fmt.Fprintf(w, "data: %s\n\n", time.Now().Format(time.RFC3339))
            flusher.Flush()
            time.Sleep(1 * time.Second)
        }
    }
}
```

### Configuration

```json
{
  "write_timeout_sec": 0
}
```

## WebSockets

For WebSockets, you need to use a WebSocket library (e.g., `gorilla/websocket`). The connection is hijacked from the HTTP server.

```go
import "github.com/gorilla/websocket"

var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool {
        return true // Allow all origins (customize as needed)
    },
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        return
    }
    defer conn.Close()
    
    for {
        messageType, p, err := conn.ReadMessage()
        if err != nil {
            return
        }
        
        if err := conn.WriteMessage(messageType, p); err != nil {
            return
        }
    }
}
```

### Configuration

```json
{
  "write_timeout_sec": 0
}
```

### Connection Tracking

When a WebSocket connection is established, the HTTP connection is hijacked. `cf_http` tracks this via the `http_hijacks_total` metric.

To track active WebSocket connections, implement your own tracking:

```go
var activeConnections sync.Map

func wsHandler(w http.ResponseWriter, r *http.Request) {
    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        return
    }
    
    // Track connection
    activeConnections.Store(conn, struct{}{})
    defer activeConnections.Delete(conn)
    
    // Handle connection...
}
```

## Graceful Shutdown

Graceful shutdown waits for in-flight requests to complete. For long-lived connections:

1. **Streaming responses**: The response will complete naturally
2. **WebSockets**: The connection will be closed when the client disconnects or the server shuts down
3. **SSE**: The connection will be closed when the client disconnects or the server shuts down

### Shutdown Timeout

The `ShutdownTimeoutSec` setting controls how long the server will wait for connections to close:

```json
{
  "shutdown_timeout_sec": 30
}
```

If connections don't close within the timeout, they are forcibly closed.

### Client Disconnect Detection

To detect when a client disconnects:

```go
func handler(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    
    for {
        select {
        case <-ctx.Done():
            // Client disconnected
            return
        default:
            // Send data
            time.Sleep(1 * time.Second)
        }
    }
}
```

## Metrics

Long-lived connections affect metrics:

- `http_connections_active`: Number of active connections (includes long-lived)
- `http_hijacks_total`: Number of hijacked connections (WebSockets)
- `http_requests_total`: Number of completed requests (not including long-lived)
- `http_request_duration_seconds`: Duration of completed requests (not including long-lived)

### Monitoring

Monitor these metrics to detect issues:

- **High `http_connections_active`**: May indicate connection leaks
- **Increasing `http_hijacks_total`**: WebSocket connections are being established
- **Long `http_request_duration_seconds`**: May indicate slow requests (not long-lived connections)

## Best Practices

1. **Disable WriteTimeout**: Set `write_timeout_sec: 0` for long-lived connections
2. **Use Context**: Check `r.Context().Done()` to detect client disconnects
3. **Track Connections**: Implement your own connection tracking for WebSockets
4. **Set Shutdown Timeout**: Set an appropriate `shutdown_timeout_sec` for graceful shutdown
5. **Monitor Metrics**: Monitor `http_connections_active` and `http_hijacks_total`
6. **Handle Errors**: Handle connection errors gracefully
7. **Use Flusher**: Use `http.Flusher` for streaming responses
8. **Don't Block**: Don't block the event loop with long-lived connections

## Example: Complete Long-Lived Connection Handler

```go
func longLivedHandler(w http.ResponseWriter, r *http.Request) {
    // Set headers
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("Connection", "keep-alive")
    
    // Get flusher
    flusher, ok := w.(http.Flusher)
    if !ok {
        http.Error(w, "Streaming not supported", http.StatusInternalServerError)
        return
    }
    
    // Track connection
    connID := uuid.New().String()
    getLogger().Info("Connection established", "conn_id", connID)
    defer getLogger().Info("Connection closed", "conn_id", connID)
    
    // Send events
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()
    
    for {
        select {
        case <-r.Context().Done():
            // Client disconnected
            return
        case t := <-ticker.C:
            fmt.Fprintf(w, "data: %s\n\n", t.Format(time.RFC3339))
            flusher.Flush()
        }
    }
}
```

## Summary

- **WriteTimeout**: Set to 0 for long-lived connections
- **Graceful Shutdown**: Configure `shutdown_timeout_sec` appropriately
- **Client Disconnect**: Use `r.Context().Done()` to detect disconnects
- **WebSockets**: Use a WebSocket library and track connections manually
- **Metrics**: Monitor `http_connections_active` and `http_hijacks_total`
- **Best Practices**: Disable WriteTimeout, use context, track connections, handle errors
