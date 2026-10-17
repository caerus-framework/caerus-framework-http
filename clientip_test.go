package cf_http

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedIngressClientIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		remote string
		envoy  string
		xff    string
		want   string
	}{
		{
			name:   "gateway envoy header wins over a spoofed left XFF",
			remote: "10.0.0.148:443",
			envoy:  "203.0.113.10",
			xff:    "192.0.2.20, 203.0.113.10",
			want:   "203.0.113.10",
		},
		{
			name:   "trusted peer walks XFF from the right and skips private hops",
			remote: "10.0.0.148:443",
			xff:    "192.0.2.20, 203.0.113.10, 10.0.0.50",
			want:   "203.0.113.10",
		},
		{
			name:   "loopback dev proxy uses XFF",
			remote: "127.0.0.1:54321",
			xff:    "203.0.113.10",
			want:   "203.0.113.10",
		},
		{
			name:   "all-private XFF falls back to the leftmost hop",
			remote: "10.0.0.148:443",
			xff:    "10.1.0.5, 10.0.0.9",
			want:   "10.1.0.5",
		},
		{
			name:   "invalid envoy header falls through to XFF",
			remote: "10.0.0.5:12345",
			envoy:  "not-an-ip",
			xff:    "203.0.113.9",
			want:   "203.0.113.9",
		},
		{
			name:   "trusted peer with no headers is the peer without the port",
			remote: "10.1.2.3:9",
			want:   "10.1.2.3",
		},
		{
			name:   "public peer ignores forged envoy and XFF",
			remote: "203.0.113.10:1234",
			envoy:  "192.0.2.20",
			xff:    "198.51.100.8",
			want:   "203.0.113.10",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remote
			if tt.envoy != "" {
				r.Header.Set("X-Envoy-External-Address", tt.envoy)
			}
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if got := TrustedIngressClientIP(r); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("nil request", func(t *testing.T) {
		t.Parallel()
		if got := TrustedIngressClientIP(nil); got != "" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestForwardedClientIP(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.170:4000"
	r.Header.Set("X-Envoy-External-Address", "203.0.113.10")
	if got := ForwardedClientIP(r); got != "203.0.113.10" {
		t.Fatalf("forward %q", got)
	}

	plain := httptest.NewRequest(http.MethodGet, "/", nil)
	plain.RemoteAddr = "10.0.0.170:4000"
	if got := ForwardedClientIP(plain); got != "" {
		t.Fatalf("peer-only must not be forwarded, got %q", got)
	}
}

func TestSetForwardedClientIP(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "http://auth/api", nil)
	req.Header.Set("X-Forwarded-For", "192.0.2.20, 10.0.0.1")
	SetForwardedClientIP(req, "203.0.113.10")
	if got := req.Header.Get("X-Envoy-External-Address"); got != "203.0.113.10" {
		t.Fatalf("envoy %q", got)
	}
	if got := req.Header.Get("X-Forwarded-For"); got != "203.0.113.10" {
		t.Fatalf("xff was appended or left spoofed: %q", got)
	}

	bad := httptest.NewRequest(http.MethodGet, "http://auth/api", nil)
	bad.Header.Set("X-Forwarded-For", "keep-me")
	SetForwardedClientIP(bad, "not-an-ip")
	if got := bad.Header.Get("X-Forwarded-For"); got != "keep-me" {
		t.Fatalf("invalid ip changed the header: %q", got)
	}
}
