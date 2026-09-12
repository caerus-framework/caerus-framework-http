package cf_http

import (
	"net"
	"net/http"
	"strings"
)

// TrustedIngressClientIP is a RequestLogConfig.ClientIP getter for processes
// behind Caerus netcup ingress: Internet → HAProxy (PROXY v2) → Istio gateway
// (numTrustedProxies ≥ 1) → mesh → app.
//
// It prefers Envoy's X-Envoy-External-Address when that value is a parseable
// IP (set by the trusted gateway). Otherwise it falls back to r.RemoteAddr
// (sidecar / mesh peer).
//
// Only enable this when untrusted clients cannot reach the pod without going
// through that ingress — the header is trivial to spoof on a direct connection.
func TrustedIngressClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if ext := strings.TrimSpace(r.Header.Get("X-Envoy-External-Address")); ext != "" {
		host := ext
		if h, _, err := net.SplitHostPort(ext); err == nil {
			host = h
		}
		if net.ParseIP(host) != nil {
			return ext
		}
	}
	return r.RemoteAddr
}
