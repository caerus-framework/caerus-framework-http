# Error Handling Strategy

This document explains the error handling strategy for `cf_http`.

## Overview

`cf_http` separates concerns:
- **Internal logging**: Uses the framework's `cf_logs` component for internal errors
- **Response serialization**: Uses `ErrorWriter` for public error responses
- **Protocol-specific errors**: GraphQL and OAuth retain their native error formats

## Failure

The unit of error output is `Failure` — a safe, client-facing value that must
never contain internal causes, stack traces, or user data:

```go
type Failure struct {
    Status    int    // HTTP status code
    Code      string // stable, machine-readable code, e.g. "NOT_FOUND" (may be empty)
    Message   string // safe, human-readable message
    RequestID string // correlation ID, populated by middleware when present
}
```

Construct one with `NewFailure(status, code, message)`; attach a correlation
ID with `.WithRequestID(id)` (usually unnecessary — middleware fills it in).

## ErrorWriter

Applications customize error responses with an `ErrorWriter`:

```go
type ErrorWriter func(w http.ResponseWriter, r *http.Request, failure Failure)
```

### Default behavior

When a middleware's `Write` option is nil, `DefaultErrorWriter` is used. It
writes `Message` when non-empty, else `http.StatusText(Status)`, via
`http.Error`:

```go
func DefaultErrorWriter(w http.ResponseWriter, r *http.Request, failure Failure) {
    message := failure.Message
    if message == "" {
        message = http.StatusText(failure.Status)
    }
    http.Error(w, message, failure.Status)
}
```

### Custom ErrorWriter

```go
func myErrorWriter(w http.ResponseWriter, r *http.Request, failure cf_http.Failure) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(failure.Status)
    json.NewEncoder(w).Encode(map[string]any{
        "code":       failure.Code,
        "message":    failure.Message,
        "request_id": failure.RequestID,
    })
}

handler := cf_http.Recover(logger, myErrorWriter)
```

REST APIs can pass `problem.ErrorWriter` instead of copying JSON:

```go
import "github.com/caerus-framework/caerus-framework-http/problem"

cf_http.Recover(getLogger, problem.ErrorWriter)
cf_http.MaxBodyBytes(1<<20, problem.ErrorWriter)
```

The same `Write` option is accepted by the other middlewares that can reject a
request, e.g. `CSRF(cf_http.CSRFConfig{Write: myErrorWriter})` and
`MaxBodyBytes(1<<20, myErrorWriter)`. CSRF `Mode` still decides cookie flags
and which checks run; `Write` only changes the 403 body.

`MaxBodyBytes` is **off** unless you wrap a handler with a positive `n`.
Content-Length larger than `n` is 413 before the inner handler runs. A
body that exceeds `n` while being read (chunked, or a lying Content-Length)
is wrapped with `http.MaxBytesReader`; if the handler does not write a
response, the middleware writes 413. Status **413**, code
`PAYLOAD_TOO_LARGE`, message `Request body too large`, plus `Connection:
close`. Do not put this on a mux that also serves file uploads.

## Problem Details (RFC 9457)

The `problem` subpackage provides RFC 9457 Problem Details support. It is
optional and REST-oriented; it is never applied automatically to GraphQL or
OAuth responses.

```go
import "github.com/caerus-framework/caerus-framework-http/problem"

p := problem.New(http.StatusBadRequest, "Invalid request").
    WithDetail("The 'email' field is required").
    WithExtension("field", "email")

// Write the response; the request must be passed so the response's
// request_id can be populated from the request's correlation ID.
if err := problem.Write(w, r, p); err != nil { /* encoding failed */ }
```

`Write(w, r, p)` uses `cf_http.RequestIDFrom(r)` for the JSON `request_id`
member when the problem does not already carry one. The `Type` defaults to
`about:blank` and the `Status` defaults to `500`.

### When to Use Problem Details

✅ **Use for**:
- REST API error responses
- Validation errors
- Business logic errors
- Standardized error format across services

❌ **Don't use for**:
- GraphQL errors (use GraphQL error format)
- OAuth errors (use OAuth error format)
- Internal errors (log them, don't expose details)

### Example: REST API with Problem Details

```go
func createUser(w http.ResponseWriter, r *http.Request) {
    var req CreateUserRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        _ = problem.Write(w, r, problem.New(http.StatusBadRequest, "Invalid request").
            WithDetail("Failed to parse request body"))
        return
    }

    if req.Email == "" {
        _ = problem.Write(w, r, problem.New(http.StatusBadRequest, "Validation failed").
            WithDetail("The 'email' field is required").
            WithExtension("field", "email"))
        return
    }

    // Success
    w.WriteHeader(http.StatusCreated)
}
```

## GraphQL Errors

GraphQL has its own error format. `cf_http` does not interfere with GraphQL
error handling:

```go
// GraphQL handler
gqlHandler := handler.NewDefaultServer(schema)

// Apply cf_http middleware
handler := cf_http.Chain(
    cf_http.RequestID(),
    cf_http.Recover(getLogger, nil),
    cf_http.RequestLog(getLogger),
    cf_http.Metrics(httpServer),
)(gqlHandler)

httpServer.SetHandler(handler)
```

GraphQL errors are returned in the response body:

```json
{
  "data": null,
  "errors": [
    {
      "message": "User not found",
      "path": ["user"],
      "extensions": {
        "code": "NOT_FOUND"
      }
    }
  ]
}
```

## OAuth Errors

OAuth has its own error format. `cf_http` does not interfere with OAuth error
handling:

```json
{
  "error": "invalid_grant",
  "error_description": "The provided authorization grant is invalid"
}
```

## Error Handling in Middleware

### Recovery middleware

`Recover` catches panics, logs them with the goroutine stack trace, and (only
when the response was not already committed) writes a 500 via the provided
`ErrorWriter`:

```go
cf_http.Recover(getLogger, nil)
```

- Panics are logged at ERROR level with the stack trace and request ID
- The 500 body comes from `DefaultErrorWriter` (or your `ErrorWriter`)
- A panic after the response was committed does not overwrite what was sent;
  it is still logged

### Request logging

`RequestLog` logs requests with status codes and a **partial**
`client_ip` (IPv4 `/24`, IPv6 `/48`). Query, body, and cookies are never
logged. The module does not read `X-Forwarded-For`; use
`RequestLogWith` if you must omit the address, log it in full, or pass
a getter for an identity the **app** already trusts.

```go
import cf_logs "github.com/caerus-framework/caerus-framework-logs"

cf_http.RequestLog(getLogger)
cf_http.RequestLogWith(getLogger, cf_http.RequestLogConfig{IP: cf_logs.IPOmit})
```

- 2xx/3xx: INFO level
- 4xx: WARN level
- 5xx: ERROR level

## Best Practices

1. **Don't Expose Internal Details**: Never expose stack traces, database errors, or internal paths to clients. Log them instead.

2. **Use Structured Errors**: Use Problem Details for REST APIs to provide a consistent error format.

3. **Log Internal Errors**: Log all internal errors with context (request ID, user ID, etc.).

4. **Use Appropriate Status Codes**: Use correct HTTP status codes (400 for bad request, 404 for not found, etc.).

5. **Validate Input**: Validate input early and return clear error messages.

6. **Don't Mix Error Formats**: Use one error format per protocol (REST → Problem Details, GraphQL → GraphQL errors, OAuth → OAuth errors).

7. **Test Error Paths**: Test error handling paths, not just happy paths.

## Example: Complete Error Handling

```go
func handleRequest(w http.ResponseWriter, r *http.Request) {
    // Validate input
    var req Request
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        _ = problem.Write(w, r, problem.New(http.StatusBadRequest, "Invalid request").
            WithDetail("Failed to parse request body"))
        return
    }

    // Business logic
    result, err := doSomething(req)
    if err != nil {
        // Log the internal error with correlation context
        getLogger().Error("Business logic failed",
            "error", err,
            "request_id", cf_http.RequestIDFrom(r),
        )

        // Return a user-friendly error
        if errors.Is(err, ErrNotFound) {
            _ = problem.Write(w, r, problem.New(http.StatusNotFound, "Not found").
                WithDetail("The requested resource was not found"))
            return
        }

        _ = problem.Write(w, r, problem.New(http.StatusInternalServerError, "Internal error").
            WithDetail("An unexpected error occurred"))
        return
    }

    // Success
    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode(result)
}
```

## Summary

- **Internal errors**: Logged via framework logger
- **Public errors**: `Failure` written through an `ErrorWriter` (`DefaultErrorWriter` uses `http.Error`), or Problem Details for REST
- **GraphQL/OAuth**: Use native error formats
- **Don't mix formats**: Use one format per protocol
- **Don't expose internals**: Log details, return user-friendly messages
