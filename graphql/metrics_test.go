package graphql

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cf_http "github.com/caerus-framework/caerus-framework-http"
	cf_observability "github.com/caerus-framework/caerus-framework-observability"
)

func newTestServer(t *testing.T) *cf_http.Server {
	t.Helper()
	s := cf_http.New(cf_http.WithBind(":0"))
	if err := s.Init(context.Background(), nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}

func findMetric(ms []cf_observability.Metric, name string, labels map[string]string) bool {
	for _, m := range ms {
		if m.Name != name {
			continue
		}
		match := true
		for k, v := range labels {
			if m.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func hasMetricName(ms []cf_observability.Metric, name string) bool {
	for _, m := range ms {
		if m.Name == name {
			return true
		}
	}
	return false
}

func graphQLRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

type countingBody struct {
	io.ReadCloser
	n *int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	*c.n += int64(n)
	return n, err
}

func TestMetricsDefaultOff(t *testing.T) {
	s := newTestServer(t)

	// By default, no operations should be tracked: HTTP /graphql metrics stay
	// but no operation-name series are emitted.
	handler := Metrics(s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	body := `{"operationName":"GetUser","query":"query GetUser { user { id } }"}`
	req := graphQLRequest(http.MethodPost, "/graphql", body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	ms := s.Metrics()
	if hasMetricName(ms, "http_graphql_operations_total") {
		t.Fatal("graphql operation metrics should not be recorded by default")
	}
	if hasMetricName(ms, "http_graphql_resolvers_total") {
		t.Fatal("graphql resolver metrics should not be recorded by default")
	}
}

func TestMetricsDefaultOffDoesNotReadBody(t *testing.T) {
	s := newTestServer(t)
	handler := Metrics(s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	payload := []byte(`{"operationName":"GetUser","query":"query GetUser { user { id } }"}`)
	var read int64
	req := httptest.NewRequest(http.MethodPost, "/graphql", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Body = &countingBody{ReadCloser: io.NopCloser(bytes.NewReader(payload)), n: &read}
	req.ContentLength = int64(len(payload))

	handler.ServeHTTP(httptest.NewRecorder(), req)
	if read != 0 {
		t.Fatalf("default-off Metrics read %d body bytes; want 0 (no extraction)", read)
	}
}

func TestOnlyOperations(t *testing.T) {
	s := newTestServer(t)

	handler := Metrics(s, OnlyOperations("GetUser"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Request with allowed operation
	body := `{"operationName":"GetUser","query":"query GetUser { user { id } }"}`
	req := graphQLRequest(http.MethodPost, "/graphql", body)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// Request with disallowed operation
	body = `{"operationName":"ListUsers","query":"query ListUsers { users { id } }"}`
	req = graphQLRequest(http.MethodPost, "/graphql", body)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// Only GetUser should be recorded as a named operation series (auto peek).
	ms := s.Metrics()
	if !findMetric(ms, "http_graphql_operations_total", map[string]string{
		"operation": "GetUser", "graphql_instrumentation": "http_peek",
	}) {
		t.Fatal("auto Metrics should emit graphql_instrumentation=http_peek")
	}
	if findMetric(ms, "http_graphql_operations_total", map[string]string{"operation": "ListUsers"}) {
		t.Fatal("ListUsers should not be recorded as a named operation series")
	}
}

func TestAllOperations(t *testing.T) {
	s := newTestServer(t)

	handler := Metrics(s, AllOperations())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Every detected name is tracked individually, no allowlist required.
	for _, op := range []string{"GetUser", "ListUsers", "CreateUser"} {
		body := `{"operationName":"` + op + `","query":"query ` + op + ` { x }"}`
		req := graphQLRequest(http.MethodPost, "/graphql", body)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	ms := s.Metrics()
	for _, op := range []string{"GetUser", "ListUsers", "CreateUser"} {
		if !findMetric(ms, "http_graphql_operations_total", map[string]string{"operation": op}) {
			t.Fatalf("http_graphql_operations_total{operation=%s} not found", op)
		}
	}
	if findMetric(ms, "http_graphql_operations_total", map[string]string{"operation": "other"}) {
		t.Fatal("AllOperations should not collapse names into the other bucket")
	}
}

func TestWithOtherBucket(t *testing.T) {
	s := newTestServer(t)

	handler := Metrics(s, OnlyOperations("GetUser"), WithOtherBucket())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Request with allowed operation
	body := `{"operationName":"GetUser","query":"query GetUser { user { id } }"}`
	req := graphQLRequest(http.MethodPost, "/graphql", body)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// Request with disallowed operation
	body = `{"operationName":"ListUsers","query":"query ListUsers { users { id } }"}`
	req = graphQLRequest(http.MethodPost, "/graphql", body)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// GetUser recorded, ListUsers collapsed into the "other" bucket.
	ms := s.Metrics()
	if !findMetric(ms, "http_graphql_operations_total", map[string]string{"operation": "GetUser"}) {
		t.Fatal("http_graphql_operations_total{operation=GetUser} not found")
	}
	if !findMetric(ms, "http_graphql_operations_total", map[string]string{"operation": "other"}) {
		t.Fatal("http_graphql_operations_total{operation=other} not found")
	}
	if findMetric(ms, "http_graphql_operations_total", map[string]string{"operation": "ListUsers"}) {
		t.Fatal("ListUsers should be collapsed into the other bucket")
	}
}

func TestExtractOperationNameGET(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/graphql?operationName=GetUser&query=query", nil)
	opName := extractOperationName(req, DefaultPeekWindow, false)
	if opName != "GetUser" {
		t.Fatalf("operationName = %q, want GetUser", opName)
	}
}

func TestExtractOperationNameGETNotGraphQL(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/graphql?foo=1", nil)
	if op := extractOperationName(req, DefaultPeekWindow, false); op != "" {
		t.Fatalf("operationName = %q, want empty", op)
	}
}

func TestExtractOperationNamePOST(t *testing.T) {
	body := `{"operationName":"GetUser","query":"query GetUser { user { id } }"}`
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	opName := extractOperationName(req, DefaultPeekWindow, false)
	if opName != "GetUser" {
		t.Fatalf("operationName = %q, want GetUser", opName)
	}
}

func TestExtractOperationNameNoBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/graphql", nil)
	req.Header.Set("Content-Type", "application/json")
	opName := extractOperationName(req, DefaultPeekWindow, false)
	if opName != "" {
		t.Fatalf("operationName = %q, want empty", opName)
	}
}

func TestExtractOperationNameSkipsNonGraphQLContentType(t *testing.T) {
	body := `{"operationName":"GetUser","query":"query GetUser { user { id } }"}`
	var read int64
	req := httptest.NewRequest(http.MethodPost, "/graphql", nil)
	req.Header.Set("Content-Type", "text/plain")
	req.Body = &countingBody{ReadCloser: io.NopCloser(strings.NewReader(body)), n: &read}
	req.ContentLength = int64(len(body))

	if op := extractOperationName(req, DefaultPeekWindow, false); op != "" {
		t.Fatalf("operationName = %q, want empty", op)
	}
	if read != 0 {
		t.Fatalf("read %d bytes with non-GraphQL Content-Type; want 0", read)
	}
}

func TestExtractOperationNameSkipsNonGraphQLJSON(t *testing.T) {
	body := `{"foo":"bar","n":1}`
	var read int64
	req := httptest.NewRequest(http.MethodPost, "/graphql", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Body = &countingBody{ReadCloser: io.NopCloser(strings.NewReader(body)), n: &read}
	req.ContentLength = int64(len(body))

	if op := extractOperationName(req, DefaultPeekWindow, false); op != "" {
		t.Fatalf("operationName = %q, want empty", op)
	}
	if read == 0 || read > DefaultPeekWindow {
		t.Fatalf("peek read = %d, want (0, %d]", read, DefaultPeekWindow)
	}
}

func TestExtractOperationNamePeekOnlyLargeBody(t *testing.T) {
	// Large body: only the peek window is read; operationName in the prefix
	// is still found; the handler can read the full body afterward.
	padding := strings.Repeat("x", 100_000)
	body := `{"operationName":"GetUser","query":"` + padding + `"}`
	var read int64
	req := httptest.NewRequest(http.MethodPost, "/graphql", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Body = &countingBody{ReadCloser: io.NopCloser(strings.NewReader(body)), n: &read}
	req.ContentLength = int64(len(body))

	opName := extractOperationName(req, DefaultPeekWindow, false)
	if opName != "GetUser" {
		t.Fatalf("operationName = %q, want GetUser", opName)
	}
	if read > DefaultPeekWindow {
		t.Fatalf("read %d bytes, want <= peek window %d", read, DefaultPeekWindow)
	}

	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, []byte(body)) {
		t.Fatalf("body not fully restored: got %d want %d", len(got), len(body))
	}
}

func TestExtractOperationNameTinyPeekMissesName(t *testing.T) {
	body := `{"operationName":"GetUser","query":"query GetUser { user { id } }"}`
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	// Window too small to contain the operationName key.
	opName := extractOperationName(req, 10, false)
	if opName != "" {
		t.Fatalf("operationName = %q, want empty (peek too small)", opName)
	}
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, []byte(body)) {
		t.Fatalf("body not preserved after tiny peek")
	}
}

func TestExtractOperationNameFullRead(t *testing.T) {
	// operationName only appears after the default peek window — needs full read.
	pad := strings.Repeat("x", int(DefaultPeekWindow)+100)
	body := `{"query":"` + pad + `","operationName":"LateOp"}`
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	if op := extractOperationName(req, DefaultPeekWindow, false); op != "" {
		t.Fatalf("default peek found %q, want empty (name beyond window)", op)
	}
	// Body was peeked+restored; rebuild request for the full-read path.
	req = httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	opName := extractOperationName(req, 0, true)
	if opName != "LateOp" {
		t.Fatalf("operationName = %q, want LateOp", opName)
	}
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, []byte(body)) {
		t.Fatalf("body not restored after full read")
	}
}

func TestWithPeekWindowZeroFullRead(t *testing.T) {
	s := newTestServer(t)
	pad := strings.Repeat("x", int(DefaultPeekWindow)+100)
	body := `{"query":"` + pad + `","operationName":"LateOp"}`

	handler := Metrics(s, OnlyOperations("LateOp"), WithPeekWindow(0))(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !findMetric(s.Metrics(), "http_graphql_operations_total", map[string]string{
		"operation": "LateOp", "graphql_instrumentation": "http_peek",
	}) {
		t.Fatal("WithPeekWindow(0) should find LateOp with graphql_instrumentation=http_peek")
	}
}

func TestResolvePeekWindow(t *testing.T) {
	cfg := &config{}
	n, full := cfg.resolvePeekWindow()
	if n != DefaultPeekWindow || full {
		t.Fatalf("unset: n=%d full=%v, want DefaultPeekWindow/false", n, full)
	}
	cfg = &config{}
	WithPeekWindow(0)(cfg)
	n, full = cfg.resolvePeekWindow()
	if n != 0 || !full {
		t.Fatalf("zero: n=%d full=%v, want 0/true", n, full)
	}
	cfg = &config{}
	WithPeekWindow(4096)(cfg)
	n, full = cfg.resolvePeekWindow()
	if n != 4096 || full {
		t.Fatalf("custom: n=%d full=%v, want 4096/false", n, full)
	}
}

func TestExtractOperationNameChunkedPeek(t *testing.T) {
	body := `{"operationName":"GetUser","query":"query GetUser { user { id } }"}`
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1

	opName := extractOperationName(req, DefaultPeekWindow, false)
	if opName != "GetUser" {
		t.Fatalf("operationName = %q, want GetUser (chunked still peeks)", opName)
	}

	readBody, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(readBody, []byte(body)) {
		t.Fatalf("body not preserved: got %q, want %q", readBody, body)
	}
}

func TestRecordResolver(t *testing.T) {
	s := newTestServer(t)

	RecordResolver(s, "GetUser", "user", 200, 50)

	ms := s.Metrics()
	if !findMetric(ms, "http_graphql_resolvers_total", map[string]string{"operation": "GetUser", "resolver": "user"}) {
		t.Fatal("http_graphql_resolvers_total{operation=GetUser,resolver=user} not found")
	}
}

func TestGraphQLStatusClassLabels(t *testing.T) {
	s := newTestServer(t)

	s.RecordGraphQLMetric("GetUser", 200, 10)
	s.RecordGraphQLMetric("GetUser", 500, 20)

	ms := s.Metrics()
	if !findMetric(ms, "http_graphql_operations_total", map[string]string{
		"operation": "GetUser", "status_class": "2xx", "graphql_instrumentation": "app",
	}) {
		t.Fatal("explicit RecordGraphQLMetric should use graphql_instrumentation=app")
	}
	if !findMetric(ms, "http_graphql_operations_total", map[string]string{"operation": "GetUser", "status_class": "5xx"}) {
		t.Fatal("http_graphql_operations_total{operation=GetUser,status_class=5xx} not found")
	}

	s.RecordGraphQLResolverMetric("GetUser", "user", 400, 30)
	ms = s.Metrics()
	if !findMetric(ms, "http_graphql_resolvers_total", map[string]string{"operation": "GetUser", "resolver": "user", "status_class": "4xx"}) {
		t.Fatal("http_graphql_resolvers_total{operation=GetUser,resolver=user,status_class=4xx} not found")
	}
}

func TestGraphQLTotalSeriesAlwaysClassed(t *testing.T) {
	s := newTestServer(t)

	s.RecordGraphQLMetric("GetUser", 200, 10)
	s.RecordGraphQLResolverMetric("GetUser", "user", 200, 10)

	for _, m := range s.Metrics() {
		switch m.Name {
		case "http_graphql_operations_total", "http_graphql_resolvers_total":
			if m.Labels["status_class"] == "" {
				t.Fatalf("%s missing status_class label: %v", m.Name, m.Labels)
			}
			if m.Labels["graphql_instrumentation"] == "" {
				t.Fatalf("%s missing graphql_instrumentation label: %v", m.Name, m.Labels)
			}
		}
	}
}

func TestContextOperationName(t *testing.T) {
	ctx := context.Background()
	ctx = WithOperationName(ctx, "GetUser")
	opName := OperationNameFrom(ctx)
	if opName != "GetUser" {
		t.Fatalf("operationName = %q, want GetUser", opName)
	}
}

func TestContextOperationNameMissing(t *testing.T) {
	ctx := context.Background()
	opName := OperationNameFrom(ctx)
	if opName != "" {
		t.Fatalf("operationName = %q, want empty", opName)
	}
}

func TestMiddleware(t *testing.T) {
	handler := Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		opName := OperationNameFrom(r.Context())
		if opName != "GetUser" {
			t.Fatalf("operationName = %q, want GetUser", opName)
		}
		w.WriteHeader(http.StatusOK)
	}))

	body := `{"operationName":"GetUser","query":"query GetUser { user { id } }"}`
	req := graphQLRequest(http.MethodPost, "/graphql", body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
}

func TestOperationMetrics(t *testing.T) {
	s := newTestServer(t)

	metrics := StartOperation(s, "GetUser")
	metrics.End(200)

	ms := s.Metrics()
	if !findMetric(ms, "http_graphql_operations_total", map[string]string{"operation": "GetUser"}) {
		t.Fatal("http_graphql_operations_total{operation=GetUser} not found")
	}
}

func TestExtractOperationNamePreservesBody(t *testing.T) {
	body := `{"operationName":"GetUser","query":"query GetUser { user { id } }"}`
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	// Extract operation name
	opName := extractOperationName(req, DefaultPeekWindow, false)
	if opName != "GetUser" {
		t.Fatalf("operationName = %q, want GetUser", opName)
	}

	// Read body again to ensure it was restored
	readBody, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(readBody, []byte(body)) {
		t.Fatalf("body = %q, want %q", readBody, body)
	}
}

func TestNormalizeEmptyOperation(t *testing.T) {
	s := newTestServer(t)

	// Record with empty operation name
	s.RecordGraphQLMetric("", 200, 100)

	ms := s.Metrics()
	if !findMetric(ms, "http_graphql_operations_total", map[string]string{"operation": "unknown"}) {
		t.Fatal("http_graphql_operations_total{operation=unknown} not found")
	}
}

func TestNormalizeEmptyResolver(t *testing.T) {
	s := newTestServer(t)

	// Record with empty resolver name
	s.RecordGraphQLResolverMetric("GetUser", "", 200, 100)

	ms := s.Metrics()
	if !findMetric(ms, "http_graphql_resolvers_total", map[string]string{"operation": "GetUser", "resolver": "unknown"}) {
		t.Fatal("http_graphql_resolvers_total{operation=GetUser,resolver=unknown} not found")
	}
}

// capabilityWriter records whether Hijack/Flush/Push were forwarded.
type capabilityWriter struct {
	http.ResponseWriter
	hijacked bool
	flushed  bool
	pushed   string
}

func (w *capabilityWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return nil, nil, errors.New("test: hijack not connected")
}

func (w *capabilityWriter) Flush() { w.flushed = true }

func (w *capabilityWriter) Push(target string, _ *http.PushOptions) error {
	w.pushed = target
	return nil
}

func TestStatusRecorderCapabilities(t *testing.T) {
	inner := &capabilityWriter{ResponseWriter: httptest.NewRecorder()}
	rec := &statusRecorder{ResponseWriter: inner, status: http.StatusOK}

	if rec.Unwrap() != inner {
		t.Fatal("Unwrap should return the underlying ResponseWriter")
	}

	rec.Flush()
	if !inner.flushed {
		t.Fatal("Flush should forward to the underlying Flusher")
	}

	if err := rec.Push("/push", nil); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if inner.pushed != "/push" {
		t.Fatalf("Push target = %q, want /push", inner.pushed)
	}

	_, _, err := rec.Hijack()
	if err == nil || !inner.hijacked {
		t.Fatalf("Hijack should forward (hijacked=%v err=%v)", inner.hijacked, err)
	}

	plain := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	if err := plain.Push("/x", nil); !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("Push without Pusher = %v, want ErrNotSupported", err)
	}
	if _, _, err := plain.Hijack(); err == nil {
		t.Fatal("Hijack without Hijacker should fail")
	}
}

func TestMetricsMiddlewarePreservesCapabilities(t *testing.T) {
	s := newTestServer(t)
	inner := &capabilityWriter{ResponseWriter: httptest.NewRecorder()}

	var sawUnwrap, sawFlush, sawPush bool
	handler := Metrics(s, OnlyOperations("Ping"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := w.(interface{ Unwrap() http.ResponseWriter }); ok && u.Unwrap() == inner {
			sawUnwrap = true
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
			sawFlush = true
		}
		if p, ok := w.(http.Pusher); ok {
			_ = p.Push("/asset", nil)
			sawPush = true
		}
		w.WriteHeader(http.StatusOK)
	}))

	body := `{"operationName":"Ping"}`
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	handler.ServeHTTP(inner, req)

	if !sawUnwrap || !sawFlush || !sawPush {
		t.Fatalf("middleware writer missing capabilities: unwrap=%v flush=%v push=%v",
			sawUnwrap, sawFlush, sawPush)
	}
	if !inner.flushed || inner.pushed != "/asset" {
		t.Fatalf("inner not reached: flushed=%v pushed=%q", inner.flushed, inner.pushed)
	}
}
