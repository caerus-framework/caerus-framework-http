package cf_http

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// ErrCORSCredentialsWildcard is returned by CORSConfig.Validate when
// AllowCredentials is true and AllowedOrigins contains "*".
var ErrCORSCredentialsWildcard = errors.New(
	"cf_http: CORS AllowCredentials cannot be true when AllowedOrigins contains '*'",
)

// CORSConfig configures CORS middleware.
type CORSConfig struct {
	// AllowedOrigins is a list of origins a cross-domain request can be executed from.
	// If the special "*" value is present in the list, all origins will be allowed.
	// Default value is [] (empty), which means no origins are allowed.
	AllowedOrigins []string

	// AllowedMethods is a list of methods the client is allowed to use with
	// cross-domain requests. Default value is simple methods (GET, POST, HEAD).
	AllowedMethods []string

	// AllowedHeaders is a list of non-simple headers the client is allowed to use with
	// cross-domain requests. Default value is [] (empty).
	AllowedHeaders []string

	// ExposedHeaders indicates which headers are safe to expose to the API of a CORS
	// API specification.
	ExposedHeaders []string

	// AllowCredentials indicates whether the request can include user credentials like
	// cookies, HTTP authentication or client side SSL certificates.
	AllowCredentials bool

	// MaxAge indicates how long (in seconds) the results of a preflight request
	// can be cached. Default value is 0, which means no caching.
	MaxAge int
}

// Validate reports whether the CORS configuration is usable.
//
// Returns ErrCORSCredentialsWildcard when AllowCredentials is true and any
// AllowedOrigin is "*". Prefer calling Validate from Init (or wiring) and
// returning the error; CORS itself still panics on the same rule so a missed
// Validate cannot serve a browser-rejected policy.
func (cfg CORSConfig) Validate() error {
	if !cfg.AllowCredentials {
		return nil
	}
	for _, origin := range cfg.AllowedOrigins {
		if origin == "*" {
			return ErrCORSCredentialsWildcard
		}
	}
	return nil
}

// CORS returns a CORS middleware with the given configuration.
//
// Panics if cfg.Validate() fails (credentials + "*"). Prefer
// CORSConfig.Validate in Init and return the error; the panic remains as a
// construction-time guard because CORS returns only Middleware (no error
// slot). See the README "Security middleware" section.
func CORS(cfg CORSConfig) Middleware {
	if err := cfg.Validate(); err != nil {
		panic(err)
	}

	// Default methods
	if len(cfg.AllowedMethods) == 0 {
		cfg.AllowedMethods = []string{"GET", "POST", "HEAD"}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Check if origin is allowed
			allowed := false
			hasWildcard := false
			for _, o := range cfg.AllowedOrigins {
				if o == "*" {
					hasWildcard = true
					allowed = true
					break
				}
				if o == origin {
					allowed = true
					break
				}
			}

			if !allowed {
				next.ServeHTTP(w, r)
				return
			}

			// Add Vary header for proper caching
			w.Header().Add("Vary", "Origin")

			// Set CORS headers
			if hasWildcard && !cfg.AllowCredentials {
				// Wildcard origin without credentials
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else {
				// Specific origin (required when credentials are used)
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}

			if cfg.AllowCredentials {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}

			if len(cfg.ExposedHeaders) > 0 {
				w.Header().Set("Access-Control-Expose-Headers", strings.Join(cfg.ExposedHeaders, ", "))
			}

			// Handle preflight request
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", strings.Join(cfg.AllowedMethods, ", "))
				if len(cfg.AllowedHeaders) > 0 {
					w.Header().Set("Access-Control-Allow-Headers", strings.Join(cfg.AllowedHeaders, ", "))
				}
				if cfg.MaxAge > 0 {
					w.Header().Set("Access-Control-Max-Age", strconv.Itoa(cfg.MaxAge))
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// CSRFMode selects one exclusive CSRF product. Empty Mode means
// CSRFSynchronizer (Path B). Do not combine these with a separate HttpOnly
// switch — Mode owns the cookie flag and which checks run.
type CSRFMode string

const (
	// CSRFSynchronizer is Path B: HttpOnly cookie plus Origin/Referer plus a
	// header or form field that matches the cookie. JavaScript cannot read the
	// cookie; the app echoes the token (CSRFTokenFrom) or sets ExposeTokenHeader.
	CSRFSynchronizer CSRFMode = "synchronizer"
	// CSRFDoubleSubmit is Path A: readable cookie (HttpOnly false). The SPA
	// copies document.cookie into the header. Origin/Referer still run.
	CSRFDoubleSubmit CSRFMode = "double_submit"
	// CSRFOriginOnly is Path C: Origin/Referer only. No CSRF cookie, no
	// header match. Do not call this double-submit.
	CSRFOriginOnly CSRFMode = "origin_only"
)

// ErrCSRFUnknownMode is returned by CSRFConfig.Validate when Mode is not
// empty and not one of the three products.
var ErrCSRFUnknownMode = errors.New("cf_http: unknown CSRF Mode")

// ErrCSRFExposeTokenOriginOnly is returned when ExposeTokenHeader is set
// with origin_only (that mode has no token to expose).
var ErrCSRFExposeTokenOriginOnly = errors.New("cf_http: ExposeTokenHeader cannot be used with origin_only")

// ErrCSRFInvalidTrustedHost is returned when a TrustedHosts entry is empty
// or looks like a URL (scheme or path) instead of host or host:port.
var ErrCSRFInvalidTrustedHost = errors.New("cf_http: TrustedHosts entries must be host or host:port, not a URL")

// CSRFConfig configures CSRF middleware.
type CSRFConfig struct {
	// Mode is the CSRF product. Empty means CSRFSynchronizer. Unknown values
	// fail Validate and panic in CSRF (same construction guard as CORS).
	Mode CSRFMode

	// CookieName is the name of the CSRF cookie. Default is "_csrf".
	// Ignored in origin_only (no cookie).
	CookieName string

	// HeaderName is the name of the CSRF request header and, when
	// ExposeTokenHeader is true, the GET/HEAD response header.
	// Default is "X-CSRF-Token".
	HeaderName string

	// FormField is the HTML form field accepted on unsafe methods when the
	// header is empty. Default is "csrf_token". Only read when Content-Type
	// is application/x-www-form-urlencoded or multipart/form-data, so JSON
	// bodies are never consumed. Ignored in origin_only.
	FormField string

	// TokenLength is the length of the generated token in bytes. Default is 32.
	TokenLength int

	// Secure controls the cookie's Secure attribute. Nil (unset) means secure:
	// the cookie is only sent over HTTPS. Set a non-nil value to override,
	// e.g. false for local development over plain HTTP.
	Secure *bool

	// SameSite is the SameSite attribute of the cookie. Default is Lax.
	SameSite http.SameSite

	// TrustedHosts is an allowlist of Origin/Referer hosts (host or
	// host:port, as in url.URL.Host — no scheme). Empty means compare to
	// r.Host, which is only safe behind an edge that owns Host (Ingress).
	// Non-empty: the Origin (or Referer) host must be in this list; r.Host
	// is ignored so a client cannot spoof Host to match a fake Origin.
	TrustedHosts []string

	// ExposeTokenHeader, when true, copies the token into HeaderName on
	// GET and HEAD responses (not OPTIONS). Default false. Use it so a
	// same-origin SPA can read response.headers instead of echoing via
	// CSRFTokenFrom. Wrap the API mux only — never a CDN-cached public GET.
	// Invalid with origin_only.
	ExposeTokenHeader bool

	// Write is the ErrorWriter used for rejected requests. Nil uses
	// DefaultErrorWriter.
	Write ErrorWriter
}

type csrfTokenContextKey struct{}

// Validate reports whether the CSRF configuration is usable.
func (cfg CSRFConfig) Validate() error {
	switch cfg.Mode {
	case "", CSRFSynchronizer, CSRFDoubleSubmit, CSRFOriginOnly:
	default:
		return fmt.Errorf("%w: %q", ErrCSRFUnknownMode, cfg.Mode)
	}
	if cfg.ExposeTokenHeader && cfg.Mode == CSRFOriginOnly {
		return ErrCSRFExposeTokenOriginOnly
	}
	for _, h := range cfg.TrustedHosts {
		if err := validateTrustedHost(h); err != nil {
			return err
		}
	}
	return nil
}

func validateTrustedHost(h string) error {
	h = strings.TrimSpace(h)
	if h == "" || strings.Contains(h, "://") || strings.Contains(h, "/") {
		return fmt.Errorf("%w: %q", ErrCSRFInvalidTrustedHost, h)
	}
	return nil
}

// CSRFTokenFrom returns the CSRF token for this request after CSRF
// middleware has run. On the GET that mints the cookie, the token is in
// context (the browser has not echoed the cookie yet). Afterwards it is
// also in the cookie. Empty when origin_only or CSRF did not run.
func CSRFTokenFrom(r *http.Request) string {
	if r == nil {
		return ""
	}
	if v, ok := r.Context().Value(csrfTokenContextKey{}).(string); ok {
		return v
	}
	c, err := r.Cookie("_csrf")
	if err != nil {
		return ""
	}
	return c.Value
}

// CSRF returns middleware for cfg.Mode. Prefer CSRFConfig.Validate in Init;
// CSRF panics if Validate fails because it returns only Middleware.
//
// All modes fail closed on unsafe methods with neither Origin nor Referer.
// synchronizer (default) and double_submit also require a matching token.
func CSRF(cfg CSRFConfig) Middleware {
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	if cfg.Mode == "" {
		cfg.Mode = CSRFSynchronizer
	}
	if cfg.CookieName == "" {
		cfg.CookieName = "_csrf"
	}
	if cfg.HeaderName == "" {
		cfg.HeaderName = "X-CSRF-Token"
	}
	if cfg.FormField == "" {
		cfg.FormField = "csrf_token"
	}
	if cfg.TokenLength == 0 {
		cfg.TokenLength = 32
	}
	if cfg.SameSite == 0 {
		cfg.SameSite = http.SameSiteLaxMode
	}
	secure := cfg.Secure == nil || *cfg.Secure
	httpOnly := cfg.Mode != CSRFDoubleSubmit

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
				if cfg.Mode == CSRFOriginOnly {
					next.ServeHTTP(w, r)
					return
				}
				token, err := csrfEnsureToken(w, r, cfg, secure, httpOnly)
				if err != nil {
					csrfWrite(w, r, cfg.Write, ErrorCodeInternal, http.StatusInternalServerError, "CSRF token generation failed")
					return
				}
				if cfg.ExposeTokenHeader && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
					w.Header().Set(cfg.HeaderName, token)
				}
				ctx := context.WithValue(r.Context(), csrfTokenContextKey{}, token)
				r = r.WithContext(ctx)
				if _, err := r.Cookie(cfg.CookieName); err != nil {
					r.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: token})
				}
				next.ServeHTTP(w, r)
				return
			}

			if !validateOrigin(r, secure, cfg.TrustedHosts) {
				csrfWrite(w, r, cfg.Write, "CSRF_ORIGIN_VALIDATION_FAILED", http.StatusForbidden, "CSRF origin validation failed")
				return
			}
			if cfg.Mode == CSRFOriginOnly {
				next.ServeHTTP(w, r)
				return
			}

			cookie, err := r.Cookie(cfg.CookieName)
			if err != nil {
				csrfWrite(w, r, cfg.Write, "CSRF_TOKEN_MISSING", http.StatusForbidden, "CSRF token missing")
				return
			}
			submitted := submittedCSRFToken(r, cfg.HeaderName, cfg.FormField)
			if submitted == "" {
				csrfWrite(w, r, cfg.Write, "CSRF_TOKEN_MISSING", http.StatusForbidden, "CSRF token missing")
				return
			}
			if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(submitted)) != 1 {
				csrfWrite(w, r, cfg.Write, "CSRF_TOKEN_MISMATCH", http.StatusForbidden, "CSRF token mismatch")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func csrfEnsureToken(w http.ResponseWriter, r *http.Request, cfg CSRFConfig, secure, httpOnly bool) (string, error) {
	if c, err := r.Cookie(cfg.CookieName); err == nil && c.Value != "" {
		return c.Value, nil
	}
	token, err := generateCSRFToken(cfg.TokenLength)
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cfg.CookieName,
		Value:    token,
		Path:     "/",
		Secure:   secure,
		HttpOnly: httpOnly,
		SameSite: cfg.SameSite,
	})
	return token, nil
}

func submittedCSRFToken(r *http.Request, headerName, formField string) string {
	if t := r.Header.Get(headerName); t != "" {
		return t
	}
	if formField == "" {
		return ""
	}
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	switch strings.ToLower(mediaType) {
	case "application/x-www-form-urlencoded", "multipart/form-data":
	default:
		return ""
	}
	_ = r.ParseForm()
	return r.PostFormValue(formField)
}

// csrfWrite writes a CSRF rejection through the configured ErrorWriter (or
// DefaultErrorWriter), correlating the request ID when present.
func csrfWrite(w http.ResponseWriter, r *http.Request, ew ErrorWriter, code string, status int, message string) {
	if ew == nil {
		ew = DefaultErrorWriter
	}
	ew(w, r, Failure{
		Status:    status,
		Code:      code,
		Message:   message,
		RequestID: RequestIDFrom(r),
	})
}

// generateCSRFToken generates a cryptographically secure random token.
func generateCSRFToken(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// validateOrigin validates the Origin or Referer header to prevent cross-origin requests.
func validateOrigin(r *http.Request, secure bool, trusted []string) bool {
	origin := r.Header.Get("Origin")
	if origin != "" {
		return validateOriginHeader(origin, r.Host, secure, trusted)
	}

	// Fall back to Referer if Origin is not present
	referer := r.Header.Get("Referer")
	if referer != "" {
		return validateRefererHeader(referer, r.Host, secure, trusted)
	}

	// Fail closed when neither Origin nor Referer is present. Unsafe methods
	// are exactly what CSRF protects, and modern browsers always send Origin
	// on cross-origin requests (and on same-origin POST/PUT/DELETE).
	return false
}

func originHostAllowed(gotHost, requestHost string, trusted []string) bool {
	if len(trusted) == 0 {
		return strings.EqualFold(gotHost, requestHost)
	}
	for _, h := range trusted {
		if strings.EqualFold(gotHost, h) {
			return true
		}
	}
	return false
}

// validateOriginHeader validates the Origin header against TrustedHosts, or
// against the request Host when the allowlist is empty.
func validateOriginHeader(origin, host string, secure bool, trusted []string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}

	if secure && u.Scheme != "https" {
		return false
	}

	return originHostAllowed(u.Host, host, trusted)
}

// validateRefererHeader validates the Referer header against TrustedHosts, or
// against the request Host when the allowlist is empty.
func validateRefererHeader(referer, host string, secure bool, trusted []string) bool {
	u, err := url.Parse(referer)
	if err != nil {
		return false
	}

	if secure && u.Scheme != "https" {
		return false
	}

	return originHostAllowed(u.Host, host, trusted)
}

// ErrSecurityHeadersHSTSMaxAge is returned when HSTSMaxAge is negative.
var ErrSecurityHeadersHSTSMaxAge = errors.New("cf_http: HSTSMaxAge must be >= 0")

// SecurityHeadersConfig configures SecurityHeaders. Installing the middleware
// sets X-Content-Type-Options: nosniff unless NoSniff is explicitly false.
// HSTS is omitted until HSTSMaxAge > 0 (do not set HSTS on a plain-HTTP
// local listener unless you mean it).
type SecurityHeadersConfig struct {
	// HSTSMaxAge is Strict-Transport-Security max-age in seconds. 0 omits
	// the header. A common production value is 31536000 (one year).
	HSTSMaxAge int

	// HSTSIncludeSubdomains adds includeSubDomains. Ignored when HSTSMaxAge is 0.
	HSTSIncludeSubdomains bool

	// HSTSPreload adds preload. Ignored when HSTSMaxAge is 0. Only set this
	// if the site is ready for the HSTS preload list (HTTPS on all hosts,
	// includeSubDomains, long max-age).
	HSTSPreload bool

	// NoSniff controls X-Content-Type-Options: nosniff. Nil (unset) means
	// on — that is why you installed this middleware. Set false to skip.
	NoSniff *bool
}

// Validate reports whether SecurityHeadersConfig is usable.
func (cfg SecurityHeadersConfig) Validate() error {
	if cfg.HSTSMaxAge < 0 {
		return ErrSecurityHeadersHSTSMaxAge
	}
	return nil
}

// SecurityHeaders sets nosniff and, when HSTSMaxAge > 0, HSTS. Prefer
// Validate in Init; SecurityHeaders panics if Validate fails.
func SecurityHeaders(cfg SecurityHeadersConfig) Middleware {
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	nosniff := cfg.NoSniff == nil || *cfg.NoSniff
	var hsts string
	if cfg.HSTSMaxAge > 0 {
		hsts = "max-age=" + strconv.Itoa(cfg.HSTSMaxAge)
		if cfg.HSTSIncludeSubdomains {
			hsts += "; includeSubDomains"
		}
		if cfg.HSTSPreload {
			hsts += "; preload"
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if nosniff {
				w.Header().Set("X-Content-Type-Options", "nosniff")
			}
			if hsts != "" {
				w.Header().Set("Strict-Transport-Security", hsts)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CompressionConfig configures compression middleware.
type CompressionConfig struct {
	// MinSize is the minimum size in bytes before compression is applied.
	// Default is 1024.
	MinSize int

	// Levels is the gzip compression level. Default is gzip.DefaultCompression.
	Level int
}

// Compression returns a gzip compression middleware.
func Compression(cfg CompressionConfig) Middleware {
	if cfg.MinSize == 0 {
		cfg.MinSize = 1024
	}
	if cfg.Level == 0 {
		cfg.Level = gzip.DefaultCompression
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check if client accepts gzip
			if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				next.ServeHTTP(w, r)
				return
			}

			// Check if response is already encoded
			if w.Header().Get("Content-Encoding") != "" {
				next.ServeHTTP(w, r)
				return
			}

			// Check for WebSocket upgrade
			if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				next.ServeHTTP(w, r)
				return
			}

			// Create response writer that compresses
			gw := &gzipResponseWriter{
				ResponseWriter: w,
				level:          cfg.Level,
				minSize:        cfg.MinSize,
			}
			defer gw.Close()

			// Don't set headers yet - we'll set them when we actually start compressing
			next.ServeHTTP(gw, r)
		})
	}
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gzWriter    *gzip.Writer
	level       int
	minSize     int
	buf         []byte
	status      int
	mu          sync.Mutex
	committed   bool
	gzipping    bool
	passthrough bool
}

// WriteHeader records the status. For compressible types the real header is
// delayed until we know whether the body reaches MinSize (so Content-Encoding
// is not too late). Images and other non-allowlisted types pass through
// immediately.
func (w *gzipResponseWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		return
	}
	w.status = code
	if !compressibleContentType(w.Header().Get("Content-Type")) {
		w.passthrough = true
		w.commitUncompressedLocked()
	}
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.passthrough || !compressibleContentType(w.Header().Get("Content-Type")) {
		w.passthrough = true
		w.commitUncompressedLocked()
		return w.ResponseWriter.Write(p)
	}
	if w.gzipping {
		return w.gzWriter.Write(p)
	}

	w.buf = append(w.buf, p...)
	if len(w.buf) < w.minSize {
		return len(p), nil
	}
	if err := w.startGzipLocked(); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *gzipResponseWriter) startGzipLocked() error {
	if w.committed {
		return nil
	}
	w.ResponseWriter.Header().Del("Content-Length")
	w.ResponseWriter.Header().Set("Content-Encoding", "gzip")
	w.ResponseWriter.Header().Add("Vary", "Accept-Encoding")
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(w.status)
	w.committed = true
	w.gzipping = true
	gz, err := gzip.NewWriterLevel(w.ResponseWriter, w.level)
	if err != nil {
		return err
	}
	w.gzWriter = gz
	_, err = w.gzWriter.Write(w.buf)
	w.buf = w.buf[:0]
	return err
}

func (w *gzipResponseWriter) commitUncompressedLocked() {
	if w.committed {
		return
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(w.status)
	w.committed = true
	if len(w.buf) > 0 {
		_, _ = w.ResponseWriter.Write(w.buf)
		w.buf = nil
	}
}

// compressibleContentType is the gzip allowlist: JSON, HTML, and other text
// (not SSE). Images, fonts, and opaque binaries stay uncompressed. Empty
// Content-Type is treated as text-like so handlers that never set a type
// still gzip. BREACH: do not gzip a page that mixes secrets with
// attacker-controlled content (see README).
func compressibleContentType(ct string) bool {
	if ct == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	if strings.EqualFold(mediaType, "text/event-stream") {
		return false
	}
	if strings.HasPrefix(strings.ToLower(mediaType), "text/") {
		return true
	}
	switch strings.ToLower(mediaType) {
	case "application/json", "application/ld+json", "application/problem+json",
		"application/javascript", "application/xml", "application/xhtml+xml":
		return true
	default:
		return false
	}
}

func (w *gzipResponseWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.gzWriter != nil {
		return w.gzWriter.Close()
	}
	w.commitUncompressedLocked()
	return nil
}

func (w *gzipResponseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.gzWriter != nil {
		_ = w.gzWriter.Flush()
	} else if !w.committed {
		// Streaming before MinSize: do not start gzip mid-flush.
		w.passthrough = true
		w.commitUncompressedLocked()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards hijacking to the underlying ResponseWriter so long-lived
// connections (WebSocket, streaming) stay usable behind compression. Any
// pending buffered or compressed data is flushed before the hijack.
func (w *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("cf_http: underlying ResponseWriter does not implement http.Hijacker")
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.gzWriter != nil {
		_ = w.gzWriter.Close()
		w.gzWriter = nil
	} else {
		w.commitUncompressedLocked()
	}
	return hj.Hijack()
}

// Ensure gzipResponseWriter implements the necessary interfaces
var _ http.ResponseWriter = (*gzipResponseWriter)(nil)
var _ http.Flusher = (*gzipResponseWriter)(nil)
var _ http.Hijacker = (*gzipResponseWriter)(nil)
var _ io.Closer = (*gzipResponseWriter)(nil)
