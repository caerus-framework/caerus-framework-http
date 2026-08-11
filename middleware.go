package cf_http

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"
)

// Middleware is a function that wraps an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware in order. The first middleware is outermost.
func Chain(mw ...Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		for i := len(mw) - 1; i >= 0; i-- {
			next = mw[i](next)
		}
		return next
	}
}

type requestIDKey struct{}

// validRequestIDRegex matches valid request ID characters (alphanumeric, hyphens, underscores)
var validRequestIDRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// RequestID generates a request ID if not present and stores it in context.
// Validates incoming X-Request-ID header: must be <= 256 bytes and contain only
// alphanumeric characters, hyphens, or underscores. Invalid values are replaced.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")

			// Validate the incoming request ID
			if id == "" || len(id) > 256 || !validRequestIDRegex.MatchString(id) {
				id = generateRequestID()
			}

			w.Header().Set("X-Request-ID", id)
			ctx := context.WithValue(r.Context(), requestIDKey{}, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequestIDFrom extracts the request ID from the request context.
func RequestIDFrom(r *http.Request) string {
	if id, ok := r.Context().Value(requestIDKey{}).(string); ok {
		return id
	}
	return ""
}

func generateRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RequestLog logs each request with method, route pattern, status, duration, and request ID.
// Uses route pattern when available (Go 1.22+ ServeMux), otherwise "unknown".
func RequestLog(logger func() *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			duration := time.Since(start)
			l := logger()
			if l == nil {
				l = slog.Default()
			}

			// Use route pattern for logging (bounded cardinality)
			route := getRoutePattern(r)

			attrs := []any{
				"method", r.Method,
				"route", route,
				"status", rec.status,
				"duration_ms", duration.Milliseconds(),
				"request_id", RequestIDFrom(r),
			}
			switch {
			case rec.status >= 500:
				l.Error("http request", attrs...)
			case rec.status >= 400:
				l.Warn("http request", attrs...)
			default:
				l.Info("http request", attrs...)
			}
		})
	}
}

// Recover recovers from panics and logs them with a stack trace.
// Only writes an error response if the response has not been committed yet.
// Uses the provided ErrorWriter, or DefaultErrorWriter when write is nil.
func Recover(get func() *slog.Logger, write ErrorWriter) Middleware {
	ew := DefaultErrorWriter
	if write != nil {
		ew = write
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			defer func() {
				if p := recover(); p != nil {
					l := get
					if l == nil {
						l = func() *slog.Logger { return slog.Default() }
					}
					logger := l()

					// Log the panic value plus the goroutine stack. The stack is
					// for operators; it is never sent to the client.
					logger.Error("panic recovered",
						"panic", p,
						"stack", string(debug.Stack()),
						"request_id", RequestIDFrom(r),
						"route", getRoutePattern(r),
					)

					// Only write an error if the response has not been committed.
					if !rec.committed {
						ew(w, r, Failure{
							Status:    http.StatusInternalServerError,
							Code:      "internal_error",
							Message:   "Internal Server Error",
							RequestID: RequestIDFrom(r),
						})
					}
				}
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// Metrics records request metrics.
// Uses route pattern (Go 1.22+ ServeMux) for bounded cardinality.
// Falls back to "unknown" when pattern is not available.
func Metrics(server *Server) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			server.meter.inFlight.Add(1)
			defer server.meter.inFlight.Add(-1)
			next.ServeHTTP(rec, r)
			duration := time.Since(start)

			// Use route pattern for metrics (bounded cardinality)
			route := getRoutePattern(r)
			recordHTTPFromMiddleware(server, route, rec.status, duration)
		})
	}
}

// statusRecorder captures the response status and whether response was committed.
// Preserves http.Hijacker, http.Flusher, http.Pusher, and Unwrap interfaces.
type statusRecorder struct {
	http.ResponseWriter
	status    int
	committed bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.committed {
		r.status = status
		r.committed = true
		r.ResponseWriter.WriteHeader(status)
	}
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.committed {
		r.committed = true
		if r.status == 0 {
			r.status = http.StatusOK
		}
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap returns the underlying ResponseWriter for http.NewResponseController.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Hijack implements http.Hijacker by delegating to the underlying ResponseWriter.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := r.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Flush implements http.Flusher by delegating to the underlying ResponseWriter.
func (r *statusRecorder) Flush() {
	if fl, ok := r.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// Push implements http.Pusher by delegating to the underlying ResponseWriter.
func (r *statusRecorder) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := r.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

// getRoutePattern extracts the route pattern from the request.
// For Go 1.22+ ServeMux, r.Pattern is set after dispatch.
// Returns "unknown" when pattern is not available (bounded cardinality).
func getRoutePattern(r *http.Request) string {
	// Go 1.22+ sets r.Pattern for ServeMux routes
	if r.Pattern != "" {
		return r.Pattern
	}
	// No pattern available - return "unknown" to avoid unbounded cardinality
	return "unknown"
}
