package cf_http

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChain(t *testing.T) {
	var order []string
	mw1 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "mw1-before")
			next.ServeHTTP(w, r)
			order = append(order, "mw1-after")
		})
	}
	mw2 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "mw2-before")
			next.ServeHTTP(w, r)
			order = append(order, "mw2-after")
		})
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
		w.WriteHeader(http.StatusOK)
	})

	chained := Chain(mw1, mw2)(handler)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	chained.ServeHTTP(rec, req)

	expected := []string{"mw1-before", "mw2-before", "handler", "mw2-after", "mw1-after"}
	if len(order) != len(expected) {
		t.Fatalf("order = %v, want %v", order, expected)
	}
	for i, v := range expected {
		if order[i] != v {
			t.Fatalf("order[%d] = %q, want %q", i, order[i], v)
		}
	}
}

func TestChainEmpty(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	chained := Chain()(handler)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	chained.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRequestID(t *testing.T) {
	handler := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestIDFrom(r)
		if id == "" {
			t.Fatal("request ID should be set")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("X-Request-ID header should be set")
	}
}

func TestRequestIDPreservesExisting(t *testing.T) {
	handler := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestIDFrom(r)
		if id != "existing-id" {
			t.Fatalf("request ID = %q, want existing-id", id)
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "existing-id")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get("X-Request-ID") != "existing-id" {
		t.Fatalf("X-Request-ID = %q, want existing-id", rec.Header().Get("X-Request-ID"))
	}
}

func TestRequestIDValidatesInput(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string // empty means should be replaced
	}{
		{"valid", "abc-123_XYZ", "abc-123_XYZ"},
		{"too long", strings.Repeat("a", 257), ""},
		{"invalid chars", "abc@123", ""},
		{"spaces", "abc 123", ""},
		{"empty", "", ""},
		{"exactly 256", strings.Repeat("a", 256), strings.Repeat("a", 256)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id := RequestIDFrom(r)
				if tt.expected != "" {
					if id != tt.expected {
						t.Fatalf("request ID = %q, want %q", id, tt.expected)
					}
				} else {
					// Should be replaced with generated ID
					if id == tt.input || id == "" {
						t.Fatalf("request ID should be replaced, got %q", id)
					}
					if len(id) != 32 { // generated ID is 32 hex chars
						t.Fatalf("generated ID length = %d, want 32", len(id))
					}
				}
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.input != "" {
				req.Header.Set("X-Request-ID", tt.input)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
		})
	}
}

func TestRequestLog(t *testing.T) {
	var logged bool
	logger := func() *slog.Logger {
		return slog.New(slog.NewTextHandler(&testWriter{fn: func(s string) {
			if strings.Contains(s, "http request") {
				logged = true
			}
		}}, nil))
	}

	handler := RequestLog(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !logged {
		t.Fatal("request should be logged")
	}
}

func TestRequestLogLevels(t *testing.T) {
	tests := []struct {
		status int
		level  string
	}{
		{200, "INFO"},
		{404, "WARN"},
		{500, "ERROR"},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			var logged bool
			logger := func() *slog.Logger {
				return slog.New(slog.NewTextHandler(&testWriter{fn: func(s string) {
					if strings.Contains(s, tt.level) {
						logged = true
					}
				}}, nil))
			}

			handler := RequestLog(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if !logged {
				t.Fatalf("status %d should log at %s level", tt.status, tt.level)
			}
		})
	}
}

func TestRecover(t *testing.T) {
	var logged bool
	logger := func() *slog.Logger {
		return slog.New(slog.NewTextHandler(&testWriter{fn: func(s string) {
			if strings.Contains(s, "panic recovered") {
				logged = true
			}
		}}, nil))
	}

	handler := Recover(logger, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !logged {
		t.Fatal("panic should be logged")
	}
}

func TestRecoverNoPanic(t *testing.T) {
	handler := Recover(nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRecoverCommittedResponse(t *testing.T) {
	// A panic after the response was committed must not overwrite it: the
	// status and body already sent to the client stay as they are, and the
	// panic is still logged.
	var logged bool
	logger := func() *slog.Logger {
		return slog.New(slog.NewTextHandler(&testWriter{fn: func(s string) {
			if strings.Contains(s, "panic recovered") {
				logged = true
			}
		}}, nil))
	}

	handler := Recover(logger, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("partial"))
		panic("panic after write")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (committed response preserved)", rec.Code)
	}
	if rec.Body.String() != "partial" {
		t.Fatalf("body = %q, want %q (committed body preserved)", rec.Body.String(), "partial")
	}
	if !logged {
		t.Fatal("panic should be logged even after a committed response")
	}
}

func TestMetrics(t *testing.T) {
	s := New(WithAddress(":0"))
	if err := s.Init(context.Background(), nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Use ServeMux with a pattern to test route pattern extraction
	mux := http.NewServeMux()
	mux.HandleFunc("GET /test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := Metrics(s)(mux)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	key := "GET /test" + graphqlKeySep + HTTPInstrumentationMiddleware
	if s.meter.count[key] != 1 {
		t.Fatalf("count = %d, want 1 (keys=%v)", s.meter.count[key], s.meter.count)
	}
	if s.meter.total[key]["2xx"] != 1 {
		t.Fatalf("total = %v, want 1", s.meter.total[key])
	}
}

func TestMetricsUnknownRoute(t *testing.T) {
	s := New(WithAddress(":0"))
	if err := s.Init(context.Background(), nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Handler without ServeMux - should use "unknown"
	handler := Metrics(s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Should use "unknown" since no route pattern
	key := "unknown" + graphqlKeySep + HTTPInstrumentationMiddleware
	if s.meter.count[key] != 1 {
		t.Fatalf("count = %d, want 1", s.meter.count[key])
	}
	if s.meter.total[key]["2xx"] != 1 {
		t.Fatalf("total = %v, want 1", s.meter.total[key])
	}
}

func TestMetricsMultiplePathsSamePattern(t *testing.T) {
	s := New(WithAddress(":0"))
	if err := s.Init(context.Background(), nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Use ServeMux with a pattern that matches multiple paths
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := Metrics(s)(mux)

	// Request to /users/123
	req1 := httptest.NewRequest(http.MethodGet, "/users/123", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	// Request to /users/456
	req2 := httptest.NewRequest(http.MethodGet, "/users/456", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	// Both should be recorded under the same pattern "GET /users/{id}"
	key := "GET /users/{id}" + graphqlKeySep + HTTPInstrumentationMiddleware
	if s.meter.count[key] != 2 {
		t.Fatalf("count = %d, want 2", s.meter.count[key])
	}
	if s.meter.total[key]["2xx"] != 2 {
		t.Fatalf("total = %v, want 2", s.meter.total[key])
	}
}

func TestMetricsInFlight(t *testing.T) {
	s := New(WithAddress(":0"))
	if err := s.Init(context.Background(), nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	started := make(chan struct{})
	done := make(chan struct{})
	handler := Metrics(s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-done
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	go handler.ServeHTTP(rec, req)

	<-started
	if s.meter.inFlight.Load() != 1 {
		t.Fatalf("inFlight = %d, want 1", s.meter.inFlight.Load())
	}

	close(done)
	time.Sleep(10 * time.Millisecond)
	if s.meter.inFlight.Load() != 0 {
		t.Fatalf("inFlight = %d, want 0", s.meter.inFlight.Load())
	}
}

type testWriter struct {
	fn func(string)
}

func (w *testWriter) Write(p []byte) (n int, err error) {
	w.fn(string(p))
	return len(p), nil
}
