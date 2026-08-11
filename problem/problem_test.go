package problem

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	cf_http "github.com/caerus-framework/caerus-framework-http"
)

func TestWrite(t *testing.T) {
	p := New(http.StatusBadRequest, "Bad Request").
		WithType("https://example.com/problems/bad-request").
		WithDetail("The request was invalid").
		WithRequestID("req-123")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	if err := Write(rec, req, p); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}

	var resp Problem
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.Status)
	}
	if resp.Title != "Bad Request" {
		t.Fatalf("title = %q, want Bad Request", resp.Title)
	}
	if resp.Type != "https://example.com/problems/bad-request" {
		t.Fatalf("type = %q, want https://example.com/problems/bad-request", resp.Type)
	}
	if resp.Detail != "The request was invalid" {
		t.Fatalf("detail = %q, want The request was invalid", resp.Detail)
	}
	if resp.RequestID != "req-123" {
		t.Fatalf("request_id = %q, want req-123", resp.RequestID)
	}
}

func TestWriteDefaultStatus(t *testing.T) {
	p := Problem{Title: "Error"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	if err := Write(rec, req, p); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestWriteDefaultType(t *testing.T) {
	p := New(http.StatusBadRequest, "Bad Request")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	if err := Write(rec, req, p); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var resp Problem
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if resp.Type != "about:blank" {
		t.Fatalf("type = %q, want about:blank", resp.Type)
	}
}

func TestWriteRequestIDFromRequest(t *testing.T) {
	// A problem without an explicit RequestID picks up the request's
	// correlation ID.
	p := New(http.StatusBadRequest, "Bad Request")

	handler := cf_http.RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := Write(w, r, p); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "auto-789")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var resp Problem
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if resp.RequestID != "auto-789" {
		t.Fatalf("request_id = %q, want auto-789", resp.RequestID)
	}
}

func TestExtensions(t *testing.T) {
	p := New(http.StatusBadRequest, "Bad Request").
		WithExtension("field", "email").
		WithExtension("code", "INVALID_EMAIL")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	if err := Write(rec, req, p); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if resp["field"] != "email" {
		t.Fatalf("field = %v, want email", resp["field"])
	}
	if resp["code"] != "INVALID_EMAIL" {
		t.Fatalf("code = %v, want INVALID_EMAIL", resp["code"])
	}
}

func TestBadRequest(t *testing.T) {
	p := BadRequest("Invalid input", "The email field is required")
	if p.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", p.Status)
	}
	if p.Title != "Invalid input" {
		t.Fatalf("title = %q, want Invalid input", p.Title)
	}
	if p.Detail != "The email field is required" {
		t.Fatalf("detail = %q, want The email field is required", p.Detail)
	}
}

func TestUnauthorized(t *testing.T) {
	p := Unauthorized("Unauthorized", "Invalid credentials")
	if p.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", p.Status)
	}
}

func TestForbidden(t *testing.T) {
	p := Forbidden("Forbidden", "Access denied")
	if p.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", p.Status)
	}
}

func TestNotFound(t *testing.T) {
	p := NotFound("Not Found", "Resource not found")
	if p.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", p.Status)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	p := MethodNotAllowed("Method Not Allowed", "POST not allowed")
	if p.Status != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", p.Status)
	}
}

func TestConflict(t *testing.T) {
	p := Conflict("Conflict", "Resource already exists")
	if p.Status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", p.Status)
	}
}

func TestTooManyRequests(t *testing.T) {
	p := TooManyRequests("Too Many Requests", "Rate limit exceeded")
	if p.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", p.Status)
	}
}

func TestInternalServerError(t *testing.T) {
	p := InternalServerError("Internal Server Error", "An error occurred")
	if p.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", p.Status)
	}
}

func TestServiceUnavailable(t *testing.T) {
	p := ServiceUnavailable("Service Unavailable", "Service is down")
	if p.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", p.Status)
	}
}

func TestChaining(t *testing.T) {
	p := New(http.StatusBadRequest, "Bad Request").
		WithType("https://example.com/problems/validation").
		WithDetail("Validation failed").
		WithInstance("/users/123").
		WithRequestID("req-456").
		WithExtension("field", "email")

	if p.Type != "https://example.com/problems/validation" {
		t.Fatalf("type = %q", p.Type)
	}
	if p.Detail != "Validation failed" {
		t.Fatalf("detail = %q", p.Detail)
	}
	if p.Instance != "/users/123" {
		t.Fatalf("instance = %q", p.Instance)
	}
	if p.RequestID != "req-456" {
		t.Fatalf("request_id = %q", p.RequestID)
	}
	if p.Extensions["field"] != "email" {
		t.Fatalf("field = %v", p.Extensions["field"])
	}
}
