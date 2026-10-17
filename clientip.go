package cf_http

import (
	"net"
	"net/http"
	"strings"
)

// TrustedIngressClientIP is the client address for a process behind Caerus
// ingress: browser → HAProxy (PROXY v2) → Istio gateway (numTrustedProxies) →
// pod. The return value is a host with no port.
//
// The gateway writes the address it decided on into X-Envoy-External-Address.
// It also appends hops to X-Forwarded-For. The leftmost X-Forwarded-For value
// is chosen by the original caller, so it is not safe to trust on its own.
//
// The immediate peer (RemoteAddr) must be a trusted proxy before either
// header is read. A trusted proxy is loopback, private, or link-local: the
// gateway, a local dev proxy, or another pod. A direct public connection
// ignores both headers and returns that peer.
//
// When the peer is trusted, X-Envoy-External-Address wins if it is a
// parseable IP. Otherwise X-Forwarded-For is walked from the right, skipping
// trusted proxies. The first address that is not a trusted proxy is the
// client. When every hop is a trusted proxy, the leftmost address is the
// client. When neither header yields an address, the peer itself is the
// client.
func TrustedIngressClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	remote := clientHost(r.RemoteAddr)
	if clientTrustedProxy(remote) {
		if ip := clientEnvoyExternal(r); ip != "" {
			return ip
		}
		if ip := clientFromForwarded(r.Header.Values("X-Forwarded-For")); ip != "" {
			return ip
		}
	}
	return remote
}

// ForwardedClientIP is the address an in-cluster caller should copy onto its
// own outbound request. It is empty when TrustedIngressClientIP found nothing
// better than the peer that connected to this process — copying that peer
// would only hide which pod made the call.
func ForwardedClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	ip := TrustedIngressClientIP(r)
	if ip == "" || ip == clientHost(r.RemoteAddr) {
		return ""
	}
	return ip
}

// SetForwardedClientIP replaces X-Envoy-External-Address and X-Forwarded-For
// with one already-resolved client address. It does not append to a chain the
// caller may have forged. An empty or unparseable ip leaves the request
// unchanged.
func SetForwardedClientIP(req *http.Request, ip string) {
	if req == nil {
		return
	}
	ip = clientHost(ip)
	if net.ParseIP(ip) == nil {
		return
	}
	req.Header.Set("X-Envoy-External-Address", ip)
	req.Header.Set("X-Forwarded-For", ip)
}

func clientEnvoyExternal(r *http.Request) string {
	ip := clientHost(r.Header.Get("X-Envoy-External-Address"))
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}

// clientFromForwarded walks X-Forwarded-For from the right. Trusted proxies
// are skipped. The first address that is not a trusted proxy is the client.
// When every hop is a trusted proxy, the leftmost address is the client.
func clientFromForwarded(values []string) string {
	var hops []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			ip := clientHost(part)
			if net.ParseIP(ip) == nil {
				continue
			}
			hops = append(hops, ip)
		}
	}
	if len(hops) == 0 {
		return ""
	}
	for i := len(hops) - 1; i >= 0; i-- {
		if !clientTrustedProxy(hops[i]) {
			return hops[i]
		}
	}
	return hops[0]
}

func clientHost(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

func clientTrustedProxy(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}
