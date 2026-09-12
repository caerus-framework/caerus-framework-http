package cf_http

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedIngressClientIP(t *testing.T) {
	t.Parallel()

	t.Run("envoy header wins", func(t *testing.T) {
		t.Parallel()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.0.0.5:12345"
		r.Header.Set("X-Envoy-External-Address", "203.0.113.9")
		if got := TrustedIngressClientIP(r); got != "203.0.113.9" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("invalid header falls back", func(t *testing.T) {
		t.Parallel()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.0.0.5:12345"
		r.Header.Set("X-Envoy-External-Address", "not-an-ip")
		if got := TrustedIngressClientIP(r); got != "10.0.0.5:12345" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("missing header uses RemoteAddr", func(t *testing.T) {
		t.Parallel()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.2.3:9"
		if got := TrustedIngressClientIP(r); got != "10.1.2.3:9" {
			t.Fatalf("got %q", got)
		}
	})
}
