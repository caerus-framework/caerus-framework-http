package cf_http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORS(t *testing.T) {
	cfg := CORSConfig{
		AllowedOrigins: []string{"https://example.com"},
		AllowedMethods: []string{"GET", "POST"},
		AllowedHeaders: []string{"Content-Type"},
		MaxAge:         3600,
	}

	handler := CORS(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Test preflight request
	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if origin := rec.Header().Get("Access-Control-Allow-Origin"); origin != "https://example.com" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want https://example.com", origin)
	}
	if methods := rec.Header().Get("Access-Control-Allow-Methods"); methods != "GET, POST" {
		t.Fatalf("Access-Control-Allow-Methods = %q, want GET, POST", methods)
	}
	if maxAge := rec.Header().Get("Access-Control-Max-Age"); maxAge != "3600" {
		t.Fatalf("Access-Control-Max-Age = %q, want 3600", maxAge)
	}

	// Test disallowed origin
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://evil.com")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if origin := rec.Header().Get("Access-Control-Allow-Origin"); origin != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty", origin)
	}
}

func TestCORSWildcard(t *testing.T) {
	cfg := CORSConfig{
		AllowedOrigins: []string{"*"},
	}

	handler := CORS(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Wildcard without credentials should return "*"
	if origin := rec.Header().Get("Access-Control-Allow-Origin"); origin != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want *", origin)
	}
}

func TestCORSConfigValidate(t *testing.T) {
	bad := CORSConfig{
		AllowedOrigins:   []string{"*"},
		AllowCredentials: true,
	}
	if err := bad.Validate(); !errors.Is(err, ErrCORSCredentialsWildcard) {
		t.Fatalf("Validate() = %v, want ErrCORSCredentialsWildcard", err)
	}

	ok := CORSConfig{
		AllowedOrigins:   []string{"https://example.com"},
		AllowCredentials: true,
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}

	starNoCreds := CORSConfig{AllowedOrigins: []string{"*"}}
	if err := starNoCreds.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestCORSWildcardWithCredentialsPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("CORS with wildcard and credentials should panic")
		}
		err, ok := r.(error)
		if !ok || !errors.Is(err, ErrCORSCredentialsWildcard) {
			t.Fatalf("panic value = %v (%T), want ErrCORSCredentialsWildcard", r, r)
		}
	}()

	cfg := CORSConfig{
		AllowedOrigins:   []string{"*"},
		AllowCredentials: true,
	}

	CORS(cfg)
}

func TestCORSCredentialsWithSpecificOrigin(t *testing.T) {
	cfg := CORSConfig{
		AllowedOrigins:   []string{"https://example.com"},
		AllowCredentials: true,
	}

	handler := CORS(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Specific origin with credentials should return the specific origin
	if origin := rec.Header().Get("Access-Control-Allow-Origin"); origin != "https://example.com" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want https://example.com", origin)
	}
	if creds := rec.Header().Get("Access-Control-Allow-Credentials"); creds != "true" {
		t.Fatalf("Access-Control-Allow-Credentials = %q, want true", creds)
	}
}

func TestCORSVaryHeader(t *testing.T) {
	cfg := CORSConfig{
		AllowedOrigins: []string{"https://example.com"},
	}

	handler := CORS(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Should have Vary: Origin header
	if vary := rec.Header().Get("Vary"); vary != "Origin" {
		t.Fatalf("Vary = %q, want Origin", vary)
	}
}

func TestCORSNoOrigin(t *testing.T) {
	cfg := CORSConfig{
		AllowedOrigins: []string{"https://example.com"},
	}

	handler := CORS(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if origin := rec.Header().Get("Access-Control-Allow-Origin"); origin != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty", origin)
	}
}

func TestCSRF(t *testing.T) {
	cfg := CSRFConfig{
		CookieName: "_csrf",
		HeaderName: "X-CSRF-Token",
	}

	handler := CSRF(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Test safe method (should pass and set cookie)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Check that a CSRF cookie was set
	cookies := rec.Result().Cookies()
	var csrfCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "_csrf" {
			csrfCookie = c
			break
		}
	}
	if csrfCookie == nil {
		t.Fatal("CSRF cookie should be set on GET request")
	}
	if csrfCookie.Value == "" {
		t.Fatal("CSRF cookie value should not be empty")
	}

	// Test unsafe method without token (should fail)
	req = httptest.NewRequest(http.MethodPost, "/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}

	// Test unsafe method with matching tokens (should pass)
	req = httptest.NewRequest(http.MethodPost, "/", nil)
	req.AddCookie(&http.Cookie{Name: "_csrf", Value: "token123"})
	req.Header.Set("X-CSRF-Token", "token123")
	req.Header.Set("Origin", "https://"+req.Host)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Test unsafe method with mismatched tokens (should fail)
	req = httptest.NewRequest(http.MethodPost, "/", nil)
	req.AddCookie(&http.Cookie{Name: "_csrf", Value: "token123"})
	req.Header.Set("X-CSRF-Token", "token456")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestCSRFNoOriginHeaderFailsClosed(t *testing.T) {
	cfg := CSRFConfig{
		CookieName: "_csrf",
		HeaderName: "X-CSRF-Token",
	}

	handler := CSRF(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Unsafe method with matching tokens but neither Origin nor Referer:
	// fail closed (403), never allow.
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.AddCookie(&http.Cookie{Name: "_csrf", Value: "token123"})
	req.Header.Set("X-CSRF-Token", "token123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (fail closed on missing Origin/Referer)", rec.Code)
	}
}

func TestCSRFTokenGeneration(t *testing.T) {
	cfg := CSRFConfig{
		CookieName:  "_csrf",
		HeaderName:  "X-CSRF-Token",
		TokenLength: 32,
	}

	handler := CSRF(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Generate token on first GET request
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	var token1 string
	for _, c := range rec1.Result().Cookies() {
		if c.Name == "_csrf" {
			token1 = c.Value
			break
		}
	}

	// Generate token on second GET request (should be different)
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	var token2 string
	for _, c := range rec2.Result().Cookies() {
		if c.Name == "_csrf" {
			token2 = c.Value
			break
		}
	}

	if token1 == "" || token2 == "" {
		t.Fatal("Tokens should be generated")
	}
	if token1 == token2 {
		t.Fatal("Tokens should be different")
	}
	// Token should be hex encoded, so length should be 2 * TokenLength
	if len(token1) != 64 {
		t.Fatalf("Token length = %d, want 64 (32 bytes hex encoded)", len(token1))
	}
}

func boolPtr(b bool) *bool { return &b }

func TestCSRFCookieAttributes(t *testing.T) {
	cfg := CSRFConfig{
		CookieName: "_csrf",
		HeaderName: "X-CSRF-Token",
		Secure:     boolPtr(true),
		SameSite:   http.SameSiteStrictMode,
	}

	handler := CSRF(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var csrfCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "_csrf" {
			csrfCookie = c
			break
		}
	}

	if csrfCookie == nil {
		t.Fatal("CSRF cookie should be set")
	}
	if !csrfCookie.Secure {
		t.Fatal("CSRF cookie should be Secure")
	}
	if csrfCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("CSRF cookie SameSite = %v, want Strict", csrfCookie.SameSite)
	}
	if !csrfCookie.HttpOnly {
		t.Fatal("CSRF cookie should be HttpOnly")
	}
}

func TestCSRFDoubleSubmitCookieNotHttpOnly(t *testing.T) {
	handler := CSRF(CSRFConfig{Mode: CSRFDoubleSubmit})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var csrfCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "_csrf" {
			csrfCookie = c
			break
		}
	}
	if csrfCookie == nil {
		t.Fatal("CSRF cookie should be set")
	}
	if csrfCookie.HttpOnly {
		t.Fatal("double_submit cookie must be readable by JavaScript")
	}
}

func TestCSRFOriginOnlySkipsToken(t *testing.T) {
	handler := CSRF(CSRFConfig{Mode: CSRFOriginOnly})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "_csrf" {
			t.Fatal("origin_only must not mint a CSRF cookie")
		}
	}

	req = httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Origin", "https://"+req.Host)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (origin only, no token)", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 without Origin", rec.Code)
	}
}

func TestCSRFTokenFromOnMintingGET(t *testing.T) {
	var got string
	handler := CSRF(CSRFConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = CSRFTokenFrom(r)
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var cookieVal string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "_csrf" {
			cookieVal = c.Value
			break
		}
	}
	if cookieVal == "" {
		t.Fatal("cookie should be set")
	}
	if got != cookieVal {
		t.Fatalf("CSRFTokenFrom = %q, want cookie %q", got, cookieVal)
	}
}

func TestCSRFExposeTokenHeader(t *testing.T) {
	handler := CSRF(CSRFConfig{ExposeTokenHeader: true})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	token := rec.Header().Get("X-CSRF-Token")
	if token == "" {
		t.Fatal("GET should expose the token header")
	}
	var cookieVal string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "_csrf" {
			cookieVal = c.Value
			break
		}
	}
	if token != cookieVal {
		t.Fatalf("header %q != cookie %q", token, cookieVal)
	}

	req = httptest.NewRequest(http.MethodOptions, "/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get("X-CSRF-Token") != "" {
		t.Fatal("OPTIONS must not expose the token (CORS preflight)")
	}
}

func TestCSRFFormField(t *testing.T) {
	handler := CSRF(CSRFConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("csrf_token=token123"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "_csrf", Value: "token123"})
	req.Header.Set("Origin", "https://"+req.Host)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestCSRFJSONBodyDoesNotCountAsForm(t *testing.T) {
	handler := CSRF(CSRFConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"csrf_token":"token123"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "_csrf", Value: "token123"})
	req.Header.Set("Origin", "https://"+req.Host)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (JSON is not a form field)", rec.Code)
	}
}

func TestCSRFValidate(t *testing.T) {
	if err := (CSRFConfig{}).Validate(); err != nil {
		t.Fatalf("empty config: %v", err)
	}
	if err := (CSRFConfig{Mode: CSRFOriginOnly, ExposeTokenHeader: true}).Validate(); err != ErrCSRFExposeTokenOriginOnly {
		t.Fatalf("got %v, want ErrCSRFExposeTokenOriginOnly", err)
	}
	if err := (CSRFConfig{Mode: "nope"}).Validate(); !errors.Is(err, ErrCSRFUnknownMode) {
		t.Fatalf("got %v, want ErrCSRFUnknownMode", err)
	}
	if err := (CSRFConfig{TrustedHosts: []string{"https://example.com"}}).Validate(); !errors.Is(err, ErrCSRFInvalidTrustedHost) {
		t.Fatalf("got %v, want ErrCSRFInvalidTrustedHost", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("CSRF should panic on unknown Mode")
		}
	}()
	_ = CSRF(CSRFConfig{Mode: "nope"})
}

func TestCSRFTrustedHostsRejectsSpoofedHost(t *testing.T) {
	handler := CSRF(CSRFConfig{TrustedHosts: []string{"api.example.com"}})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Host = "evil.example"
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(&http.Cookie{Name: "_csrf", Value: "token123"})
	req.Header.Set("X-CSRF-Token", "token123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (Origin host not in allowlist)", rec.Code)
	}
}

func TestCSRFTrustedHostsIgnoresRequestHost(t *testing.T) {
	handler := CSRF(CSRFConfig{TrustedHosts: []string{"api.example.com"}})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Host = "evil.example"
	req.Header.Set("Origin", "https://api.example.com")
	req.AddCookie(&http.Cookie{Name: "_csrf", Value: "token123"})
	req.Header.Set("X-CSRF-Token", "token123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Origin host is on the allowlist)", rec.Code)
	}
}

func TestCSRFDefaultOriginTrustsRequestHost(t *testing.T) {
	handler := CSRF(CSRFConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Host = "evil.example"
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(&http.Cookie{Name: "_csrf", Value: "token123"})
	req.Header.Set("X-CSRF-Token", "token123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (empty TrustedHosts compares Origin to r.Host)", rec.Code)
	}
}

func TestSecurityHeadersDefaultNoSniff(t *testing.T) {
	handler := SecurityHeaders(SecurityHeadersConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("nosniff = %q", rec.Header().Get("X-Content-Type-Options"))
	}
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS must be omitted when HSTSMaxAge is 0")
	}
}

func TestSecurityHeadersHSTS(t *testing.T) {
	handler := SecurityHeaders(SecurityHeadersConfig{
		HSTSMaxAge:            31536000,
		HSTSIncludeSubdomains: true,
		HSTSPreload:           true,
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	got := rec.Header().Get("Strict-Transport-Security")
	want := "max-age=31536000; includeSubDomains; preload"
	if got != want {
		t.Fatalf("HSTS = %q, want %q", got, want)
	}
}

func TestSecurityHeadersNoSniffOff(t *testing.T) {
	off := false
	handler := SecurityHeaders(SecurityHeadersConfig{NoSniff: &off})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get("X-Content-Type-Options") != "" {
		t.Fatalf("nosniff should be off, got %q", rec.Header().Get("X-Content-Type-Options"))
	}
}

func TestCompression(t *testing.T) {
	cfg := CompressionConfig{
		MinSize: 10,
	}

	handler := Compression(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Hello, World! This is a test response that should be compressed."))
	}))

	// Test with gzip accepted
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if encoding := rec.Header().Get("Content-Encoding"); encoding != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", encoding)
	}

	// Test without gzip accepted
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if encoding := rec.Header().Get("Content-Encoding"); encoding != "" {
		t.Fatalf("Content-Encoding = %q, want empty", encoding)
	}
}

func TestCompressionMinSize(t *testing.T) {
	cfg := CompressionConfig{
		MinSize: 1000,
	}

	handler := Compression(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("small"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Response is smaller than MinSize, so it should not be compressed
	if encoding := rec.Header().Get("Content-Encoding"); encoding != "" {
		t.Fatalf("Content-Encoding = %q, want empty (response too small)", encoding)
	}
}

func TestCompressionSkipsSSE(t *testing.T) {
	cfg := CompressionConfig{
		MinSize: 10,
	}
	payload := []byte("data: hello\n\n")

	handler := Compression(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(payload)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// SSE is streaming: it must never be gzip-compressed, and the body must
	// pass through byte-for-byte.
	if encoding := rec.Header().Get("Content-Encoding"); encoding != "" {
		t.Fatalf("Content-Encoding = %q, want empty (SSE must not be compressed)", encoding)
	}
	if rec.Body.String() != string(payload) {
		t.Fatalf("body = %q, want %q (SSE must pass through raw)", rec.Body.String(), payload)
	}
}

func TestCompressionSkipsSSEWithCharsetAndFlush(t *testing.T) {
	cfg := CompressionConfig{MinSize: 10}
	chunk1 := []byte("data: one\n\n")
	chunk2 := []byte("data: two\n\n")

	handler := Compression(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		if _, err := w.Write(chunk1); err != nil {
			t.Errorf("Write chunk1: %v", err)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if _, err := w.Write(chunk2); err != nil {
			t.Errorf("Write chunk2: %v", err)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if encoding := rec.Header().Get("Content-Encoding"); encoding != "" {
		t.Fatalf("Content-Encoding = %q, want empty (SSE+charset must not be compressed)", encoding)
	}
	want := string(chunk1) + string(chunk2)
	if rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestCompressionWriteHeaderThenBody(t *testing.T) {
	payload := strings.Repeat("n", 64)
	handler := Compression(CompressionConfig{MinSize: 10})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(payload))
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
}

func TestCompressionSkipsOctetStream(t *testing.T) {
	payload := strings.Repeat("b", 64)
	handler := Compression(CompressionConfig{MinSize: 10})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(payload))
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("binary types must not gzip, got %q", rec.Header().Get("Content-Encoding"))
	}
	if rec.Body.String() != payload {
		t.Fatalf("body must pass through uncompressed")
	}
}

func TestCSRFSecureDefaultsTrue(t *testing.T) {
	cfg := CSRFConfig{
		CookieName: "_csrf",
		HeaderName: "X-CSRF-Token",
		// Secure left nil → HTTPS-only cookie by default
	}

	handler := CSRF(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var csrfCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "_csrf" {
			csrfCookie = c
			break
		}
	}
	if csrfCookie == nil {
		t.Fatal("CSRF cookie should be set")
	}
	if !csrfCookie.Secure {
		t.Fatal("CSRF cookie Secure should default to true when cfg.Secure is nil")
	}
}
