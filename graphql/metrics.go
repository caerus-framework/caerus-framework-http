// Package graphql provides GraphQL-over-HTTP telemetry helpers for cf_http.
//
// This package works with any GraphQL server that implements http.Handler
// (gqlgen, graph-gophers, etc.). It does not depend on any specific GraphQL
// library.
package graphql

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	cf_http "github.com/caerus-framework/caerus-framework-http"
)

const (
	// DefaultPeekWindow is how many leading POST body bytes Metrics inspects
	// for operationName when WithPeekWindow is not set (8 KiB).
	DefaultPeekWindow int64 = 8 << 10
)

// operationNamePartialRE finds "operationName":"..." in a truncated JSON window.
var operationNamePartialRE = regexp.MustCompile(
	`"operationName"\s*:\s*"((?:\\.|[^"\\])*)"`,
)

// Metrics records GraphQL operation metrics. By default, it does NOT track
// any operations. Use OnlyOperations to specify which operations should be
// tracked. Operations not in the allowlist are not recorded unless
// WithOtherBucket (bounded "other" label) or AllOperations (every name —
// DANGEROUS, see its doc) is used.
//
// When tracking is enabled, POST operationName extraction uses a bounded
// peek window (see WithPeekWindow) — see package docs / README.
func Metrics(server *cf_http.Server, opts ...Option) func(http.Handler) http.Handler {
	cfg := &config{
		allowedOps: make(map[string]struct{}), // empty by default - no tracking
		trackOther: false,
		// peekWindow nil → DefaultPeekWindow
	}
	for _, opt := range opts {
		opt(cfg)
	}
	return func(next http.Handler) http.Handler {
		return &metricsHandler{
			next:   next,
			server: server,
			cfg:    cfg,
		}
	}
}

type config struct {
	allowedOps map[string]struct{}
	// peekWindow: nil = DefaultPeekWindow; 0 = full body read; >0 = peek N bytes.
	peekWindow *int64
	trackOther bool
	trackAll   bool
}

func (c *config) resolvePeekWindow() (n int64, full bool) {
	if c == nil || c.peekWindow == nil {
		return DefaultPeekWindow, false
	}
	if *c.peekWindow == 0 {
		return 0, true
	}
	return *c.peekWindow, false
}

// Option configures GraphQL metrics.
type Option func(*config)

// OnlyOperations restricts metrics to the specified operation names.
// Only these operations will be tracked. This is the primary way to enable
// GraphQL metrics.
func OnlyOperations(ops ...string) Option {
	return func(cfg *config) {
		cfg.allowedOps = make(map[string]struct{}, len(ops))
		for _, op := range ops {
			if op != "" {
				cfg.allowedOps[op] = struct{}{}
			}
		}
	}
}

// AllOperations tracks every detected operation name. DANGEROUS: operation
// names are client-controlled, so this is a public cardinality-abuse vector
// and a self-inflicted DoS. Prefer OnlyOperations, optionally with
// WithOtherBucket. Documented as an escape hatch only — do not enable on
// public endpoints.
func AllOperations() Option {
	return func(cfg *config) {
		cfg.trackAll = true
	}
}

// WithOtherBucket enables tracking of operations not in the allowlist
// under the "other" label. Use with caution as this can still lead to
// unbounded cardinality if there are many unique operation names.
func WithOtherBucket() Option {
	return func(cfg *config) {
		cfg.trackOther = true
	}
}

// WithPeekWindow sets how many leading POST body bytes Metrics may inspect
// when extracting operationName after the Content-Type preflight.
//
//   - omit (default): DefaultPeekWindow (8 KiB) — safe for public endpoints
//   - n > 0: peek at most n bytes, then stop
//   - 0: read the entire body for extraction (explicit opt-in; costly on
//     large uploads — prefer keeping operationName near the start of the JSON)
//
// The peeked/read prefix is always restored for the downstream handler.
func WithPeekWindow(size int64) Option {
	return func(cfg *config) {
		cfg.peekWindow = &size
	}
}

// WithMaxBodySize is an alias for WithPeekWindow (historical name).
func WithMaxBodySize(size int64) Option {
	return WithPeekWindow(size)
}

type metricsHandler struct {
	next   http.Handler
	server *cf_http.Server
	cfg    *config
}

func (c *config) trackingEnabled() bool {
	return c.trackAll || c.trackOther || len(c.allowedOps) > 0
}

func (h *metricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Default Metrics(server) emits no operation series — do not touch the
	// body (avoids up-to-maxBodySize read+JSON parse DoS amplification).
	if !h.cfg.trackingEnabled() {
		h.next.ServeHTTP(w, r)
		return
	}

	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	n, full := h.cfg.resolvePeekWindow()
	opName := extractOperationName(r, n, full)

	shouldTrack := false
	metricOpName := ""

	if opName == "" {
		opName = "anonymous"
	}

	if _, ok := h.cfg.allowedOps[opName]; ok {
		shouldTrack = true
		metricOpName = opName
	} else if h.cfg.trackAll {
		shouldTrack = true
		metricOpName = opName
	} else if h.cfg.trackOther {
		shouldTrack = true
		metricOpName = "other"
	}

	h.next.ServeHTTP(rec, r)

	if shouldTrack {
		duration := time.Since(start)
		recordGraphQLOperationHTTPPeek(h.server, metricOpName, rec.status, duration)
	}
}

// recordGraphQLOperation records via the app/engine path (graphql_instrumentation=app).
func recordGraphQLOperation(server *cf_http.Server, operation string, status int, duration time.Duration) {
	if server == nil {
		return
	}
	if operation == "" {
		operation = "unknown"
	}
	server.RecordGraphQLMetric(operation, status, duration)
}

// recordGraphQLOperationHTTPPeek records auto body-peek samples
// (graphql_instrumentation=http_peek).
func recordGraphQLOperationHTTPPeek(server *cf_http.Server, operation string, status int, duration time.Duration) {
	if server == nil {
		return
	}
	if operation == "" {
		operation = "unknown"
	}
	server.RecordGraphQLMetricFromHTTPPeek(operation, status, duration)
}

// RecordResolver records a GraphQL resolver metric. This is a hook for
// GraphQL libraries to report resolver-level metrics.
func RecordResolver(server *cf_http.Server, operation, resolver string, status int, duration time.Duration) {
	if server == nil {
		return
	}

	// Normalize empty values
	if operation == "" {
		operation = "unknown"
	}
	if resolver == "" {
		resolver = "unknown"
	}

	// Use the server's meter to record GraphQL resolver metrics
	server.RecordGraphQLResolverMetric(operation, resolver, status, duration)
}

// extractOperationName attempts to extract the operation name from a GraphQL
// request (GET query string or POST body).
//
// POST path: Content-Type must look GraphQL-ish. Then either a bounded peek
// (full=false, n bytes) or a full body read (full=true) is used. If the bytes
// inspected do not look like a GraphQL JSON object, extraction stops. Whatever
// was read is restored for the downstream handler.
func extractOperationName(r *http.Request, n int64, full bool) string {
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		if q.Get("query") == "" && q.Get("operationName") == "" {
			return ""
		}
		return q.Get("operationName")
	}

	if r.Method != http.MethodPost {
		return ""
	}
	if !looksLikeGraphQLContentType(r.Header.Get("Content-Type")) {
		return ""
	}
	if r.ContentLength == 0 {
		return ""
	}

	var (
		buf []byte
		err error
	)
	if full {
		buf, err = readFullRequestBody(r)
	} else {
		if n <= 0 {
			n = DefaultPeekWindow
		}
		buf, err = peekRequestBody(r, n)
	}
	if err != nil || len(buf) == 0 {
		return ""
	}
	return operationNameFromPeek(buf)
}

// readFullRequestBody buffers the entire body and restores it on r.Body.
// Used only when WithPeekWindow(0) is set.
func readFullRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(nil))
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func looksLikeGraphQLContentType(ct string) bool {
	if ct == "" {
		return false
	}
	media, _, err := mime.ParseMediaType(ct)
	if err != nil {
		media = strings.TrimSpace(strings.Split(ct, ";")[0])
	}
	switch strings.ToLower(media) {
	case "application/json", "application/graphql", "application/graphql+json":
		return true
	default:
		return false
	}
}

func looksLikeGraphQLJSON(peek []byte) bool {
	trim := bytes.TrimLeft(peek, " \t\r\n")
	if len(trim) == 0 || trim[0] != '{' {
		return false
	}
	// Cheap key presence in the peek window (not a full schema check).
	return bytes.Contains(peek, []byte(`"query"`)) ||
		bytes.Contains(peek, []byte(`"operationName"`)) ||
		bytes.Contains(peek, []byte(`"extensions"`))
}

func operationNameFromPeek(peek []byte) string {
	if !looksLikeGraphQLJSON(peek) {
		return ""
	}
	var req struct {
		OperationName string `json:"operationName"`
	}
	if err := json.Unmarshal(peek, &req); err == nil {
		return req.OperationName
	}
	// Truncated window: still accept operationName if the string is intact.
	m := operationNamePartialRE.FindSubmatch(peek)
	if len(m) < 2 {
		return ""
	}
	var name string
	if err := json.Unmarshal([]byte(`"`+string(m[1])+`"`), &name); err != nil {
		return ""
	}
	return name
}

type prependCloser struct {
	io.Reader
	c io.Closer
}

func (p *prependCloser) Close() error {
	if p.c != nil {
		return p.c.Close()
	}
	return nil
}

// peekRequestBody reads up to n leading bytes and restores them on r.Body.
func peekRequestBody(r *http.Request, n int64) ([]byte, error) {
	if r.Body == nil || n <= 0 {
		return nil, nil
	}
	orig := r.Body
	peek, err := io.ReadAll(io.LimitReader(orig, n))
	if err != nil {
		r.Body = orig
		return nil, err
	}
	r.Body = &prependCloser{
		Reader: io.MultiReader(bytes.NewReader(peek), orig),
		c:      orig,
	}
	return peek, nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap returns the underlying ResponseWriter for http.NewResponseController.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("graphql: underlying ResponseWriter does not implement http.Hijacker")
	}
	return hj.Hijack()
}

// Push implements http.Pusher by delegating to the underlying ResponseWriter.
func (r *statusRecorder) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := r.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

var _ http.ResponseWriter = (*statusRecorder)(nil)
var _ http.Flusher = (*statusRecorder)(nil)
var _ http.Hijacker = (*statusRecorder)(nil)
var _ http.Pusher = (*statusRecorder)(nil)

// Context key for GraphQL operation name
type contextKey struct{}

// WithOperationName stores the operation name in the request context.
func WithOperationName(ctx context.Context, operationName string) context.Context {
	return context.WithValue(ctx, contextKey{}, operationName)
}

// OperationNameFrom extracts the operation name from the request context.
func OperationNameFrom(ctx context.Context) string {
	if op, ok := ctx.Value(contextKey{}).(string); ok {
		return op
	}
	return ""
}

// Middleware provides a way to extract operation names from GraphQL libraries
// that don't automatically expose them. Use this in your GraphQL handler to
// ensure operation names are available for metrics.
func Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Try to extract operation name if not already in context
			if OperationNameFrom(r.Context()) == "" {
				opName := extractOperationName(r, DefaultPeekWindow, false)
				if opName != "" {
					ctx := WithOperationName(r.Context(), opName)
					r = r.WithContext(ctx)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// OperationMetrics provides a simple way to track operation metrics from
// within a GraphQL resolver or handler.
type OperationMetrics struct {
	server    *cf_http.Server
	operation string
	start     time.Time
	mu        sync.Mutex
}

// StartOperation begins tracking a GraphQL operation.
func StartOperation(server *cf_http.Server, operation string) *OperationMetrics {
	return &OperationMetrics{
		server:    server,
		operation: operation,
		start:     time.Now(),
	}
}

// End records the operation metric with the given status.
func (m *OperationMetrics) End(status int) {
	if m == nil || m.server == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	duration := time.Since(m.start)
	recordGraphQLOperation(m.server, m.operation, status, duration)
}
