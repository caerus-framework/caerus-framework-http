package cf_http

import (
	"bufio"
	"compress/gzip"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
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

// CSRFConfig configures CSRF middleware.
type CSRFConfig struct {
	// CookieName is the name of the CSRF cookie. Default is "_csrf".
	CookieName string

	// HeaderName is the name of the CSRF header. Default is "X-CSRF-Token".
	HeaderName string

	// TokenLength is the length of the generated token in bytes. Default is 32.
	TokenLength int

	// Secure controls the cookie's Secure attribute. Nil (unset) means secure:
	// the cookie is only sent over HTTPS. Set a non-nil value to override,
	// e.g. false for local development over plain HTTP.
	Secure *bool

	// SameSite is the SameSite attribute of the cookie. Default is Lax.
	SameSite http.SameSite

	// Write is the ErrorWriter used for rejected requests. Nil uses
	// DefaultErrorWriter.
	Write ErrorWriter
}

// CSRF returns a CSRF middleware implementing the double-submit cookie pattern
// with Origin/Referer validation. Safe methods (GET/HEAD/OPTIONS) mint and set
// a CSRF cookie when absent; unsafe methods must present a matching header.
// For unsafe methods, a request with neither an Origin nor a Referer header is
// rejected (fail closed) because those methods are exactly what CSRF protects.
func CSRF(cfg CSRFConfig) Middleware {
	if cfg.CookieName == "" {
		cfg.CookieName = "_csrf"
	}
	if cfg.HeaderName == "" {
		cfg.HeaderName = "X-CSRF-Token"
	}
	if cfg.TokenLength == 0 {
		cfg.TokenLength = 32
	}
	if cfg.SameSite == 0 {
		cfg.SameSite = http.SameSiteLaxMode
	}
	secure := cfg.Secure == nil || *cfg.Secure

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Safe methods: mint token if needed and continue
			if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
				// Check if we already have a CSRF cookie
				_, err := r.Cookie(cfg.CookieName)
				if err != nil {
					// No cookie, generate and set a new token
					token, err := generateCSRFToken(cfg.TokenLength)
					if err != nil {
						csrfWrite(w, r, cfg.Write, ErrorCodeInternal, http.StatusInternalServerError, "CSRF token generation failed")
						return
					}

					cookie := &http.Cookie{
						Name:     cfg.CookieName,
						Value:    token,
						Path:     "/",
						Secure:   secure,
						HttpOnly: true,
						SameSite: cfg.SameSite,
					}
					http.SetCookie(w, cookie)
				}

				next.ServeHTTP(w, r)
				return
			}

			// Unsafe methods: validate CSRF token.

			// 1. Origin/Referer validation (prevent cross-origin requests).
			// Fail closed when neither header is present.
			if !validateOrigin(r, secure) {
				csrfWrite(w, r, cfg.Write, "CSRF_ORIGIN_VALIDATION_FAILED", http.StatusForbidden, "CSRF origin validation failed")
				return
			}

			// 2. Get token from cookie.
			cookie, err := r.Cookie(cfg.CookieName)
			if err != nil {
				csrfWrite(w, r, cfg.Write, "CSRF_TOKEN_MISSING", http.StatusForbidden, "CSRF token missing")
				return
			}

			// 3. Get token from header.
			headerToken := r.Header.Get(cfg.HeaderName)
			if headerToken == "" {
				csrfWrite(w, r, cfg.Write, "CSRF_TOKEN_MISSING", http.StatusForbidden, "CSRF token missing")
				return
			}

			// 4. Constant-time comparison.
			if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(headerToken)) != 1 {
				csrfWrite(w, r, cfg.Write, "CSRF_TOKEN_MISMATCH", http.StatusForbidden, "CSRF token mismatch")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
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
func validateOrigin(r *http.Request, secure bool) bool {
	origin := r.Header.Get("Origin")
	if origin != "" {
		return validateOriginHeader(origin, r.Host, secure)
	}

	// Fall back to Referer if Origin is not present
	referer := r.Header.Get("Referer")
	if referer != "" {
		return validateRefererHeader(referer, r.Host, secure)
	}

	// Fail closed when neither Origin nor Referer is present. Unsafe methods
	// are exactly what CSRF protects, and modern browsers always send Origin
	// on cross-origin requests (and on same-origin POST/PUT/DELETE).
	return false
}

// validateOriginHeader validates the Origin header against the request host.
func validateOriginHeader(origin, host string, secure bool) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}

	// Check scheme
	if secure && u.Scheme != "https" {
		return false
	}

	// Check host
	return u.Host == host
}

// validateRefererHeader validates the Referer header against the request host.
func validateRefererHeader(referer, host string, secure bool) bool {
	u, err := url.Parse(referer)
	if err != nil {
		return false
	}

	// Check scheme
	if secure && u.Scheme != "https" {
		return false
	}

	// Check host
	return u.Host == host
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
	mu          sync.Mutex
	headersSent bool
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Never compress Server-Sent Events; their streaming must stay live.
	if !w.isCompressible() {
		return w.ResponseWriter.Write(p)
	}

	// Buffer data until we have enough to compress
	w.buf = append(w.buf, p...)
	if len(w.buf) < w.minSize {
		// Return the original length to satisfy io.Writer contract
		return len(p), nil
	}

	// Set headers only when we actually start compressing
	if !w.headersSent {
		w.ResponseWriter.Header().Del("Content-Length")
		w.ResponseWriter.Header().Set("Content-Encoding", "gzip")
		// Use Add instead of Set to preserve existing Vary headers
		w.ResponseWriter.Header().Add("Vary", "Accept-Encoding")
		w.headersSent = true
	}

	// Initialize gzip writer if needed
	if w.gzWriter == nil {
		gz, err := gzip.NewWriterLevel(w.ResponseWriter, w.level)
		if err != nil {
			return 0, err
		}
		w.gzWriter = gz
	}

	// Write buffered data
	_, err := w.gzWriter.Write(w.buf)
	w.buf = w.buf[:0]

	// Return the original length to satisfy io.Writer contract
	// Even if gzip write failed, we've consumed the input
	return len(p), err
}

// isCompressible reports whether the response's declared content type should
// be gzip-compressed. Streaming types (SSE) are excluded, including when the
// Content-Type carries parameters (e.g. charset).
func (w *gzipResponseWriter) isCompressible() bool {
	ct := w.Header().Get("Content-Type")
	if ct == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "text/event-stream")
	}
	return !strings.EqualFold(mediaType, "text/event-stream")
}

func (w *gzipResponseWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.gzWriter != nil {
		return w.gzWriter.Close()
	}

	// If we have buffered data but haven't initialized gzip, write it uncompressed
	if len(w.buf) > 0 {
		_, err := w.ResponseWriter.Write(w.buf)
		return err
	}

	return nil
}

func (w *gzipResponseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.gzWriter != nil {
		w.gzWriter.Flush()
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
	}
	if len(w.buf) > 0 {
		_, _ = w.ResponseWriter.Write(w.buf)
		w.buf = nil
	}
	return hj.Hijack()
}

// Ensure gzipResponseWriter implements the necessary interfaces
var _ http.ResponseWriter = (*gzipResponseWriter)(nil)
var _ http.Flusher = (*gzipResponseWriter)(nil)
var _ http.Hijacker = (*gzipResponseWriter)(nil)
var _ io.Closer = (*gzipResponseWriter)(nil)
