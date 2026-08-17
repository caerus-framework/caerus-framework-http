package cf_http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	cf "github.com/caerus-framework/caerus-framework"
	cf_configuration "github.com/caerus-framework/caerus-framework-configuration"
	cf_logs "github.com/caerus-framework/caerus-framework-logs"
)

func TestComponentContract(t *testing.T) {
	s := New()
	if s.Name() != ComponentName {
		t.Fatalf("Name() = %q, want %q", s.Name(), ComponentName)
	}
	if s.GetInitOrderStage() != ComponentStage {
		t.Fatalf("GetInitOrderStage() = %q, want %q", s.GetInitOrderStage(), ComponentStage)
	}
	var _ cf.CaerusComponent = s
	var _ cf.Dependencies = s
	var _ cf.Runnable = s
	var _ cf.HealthProvider = s
}

func TestHealthBeforeInit(t *testing.T) {
	s := New()
	if err := s.Health(context.Background()); err == nil {
		t.Fatal("Health before Init should fail")
	}
}

func TestMetricsBeforeInit(t *testing.T) {
	s := New()
	if ms := s.Metrics(); ms != nil {
		t.Fatalf("Metrics before Init = %+v, want nil", ms)
	}
}

func TestNewDefaults(t *testing.T) {
	s := New()
	if s.readTimeout != 10*time.Second {
		t.Fatalf("readTimeout = %v, want 10s", s.readTimeout)
	}
	if s.writeTimeout != 10*time.Second {
		t.Fatalf("writeTimeout = %v, want 10s", s.writeTimeout)
	}
	if s.idleTimeout != 60*time.Second {
		t.Fatalf("idleTimeout = %v, want 60s", s.idleTimeout)
	}
	if s.readHeaderTimeout != 5*time.Second {
		t.Fatalf("readHeaderTimeout = %v, want 5s", s.readHeaderTimeout)
	}
	if s.shutdownTimeout != 10*time.Second {
		t.Fatalf("shutdownTimeout = %v, want 10s", s.shutdownTimeout)
	}
	if s.maxHeaderBytes != 1<<20 {
		t.Fatalf("maxHeaderBytes = %d, want %d", s.maxHeaderBytes, 1<<20)
	}
	if !s.metricsEnabled {
		t.Fatal("metricsEnabled should be true by default")
	}
}

func TestNewWithName(t *testing.T) {
	s := New(WithName("custom"))
	if s.Name() != "custom" {
		t.Fatalf("Name() = %q, want custom", s.Name())
	}
}

func TestInitRequiresAddress(t *testing.T) {
	s := New()
	if err := s.Init(context.Background(), cf.New()); err == nil {
		t.Fatal("Init without address should fail")
	}
}

func TestInitWithBind(t *testing.T) {
	s := New(WithBind(":0"))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	if err := s.Health(context.Background()); err == nil {
		t.Fatal("Health should fail before SetHandler")
	}
}

func TestInitTwiceIsIdempotent(t *testing.T) {
	s := New(WithBind(":0"))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("second Init: %v", err)
	}
}

func TestShutdownBeforeInit(t *testing.T) {
	s := New()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Init: %v", err)
	}
}

func TestRunBeforeInit(t *testing.T) {
	s := New(WithBind(":0"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); err == nil {
		t.Fatal("Run before Init should fail")
	}
}

func TestRunBeforeSetHandler(t *testing.T) {
	s := New(WithBind(":0"))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); err == nil {
		t.Fatal("Run before SetHandler should fail")
	}
}

func TestRunAndShutdown(t *testing.T) {
	s := New(WithBind("127.0.0.1:0"))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s.SetHandler(handler)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()

	// Wait for server to start
	time.Sleep(50 * time.Millisecond)

	addr := s.Addr()
	if addr == "" {
		t.Fatal("Addr() should be non-empty after Run")
	}

	// Make a request
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Shutdown
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestGracefulDrain(t *testing.T) {
	s := New(WithBind("127.0.0.1:0"), WithShutdownTimeout(2*time.Second))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	handlerStarted := make(chan struct{})
	handlerDone := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerStarted)
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		close(handlerDone)
	})
	s.SetHandler(handler)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()

	// Wait for server to start
	time.Sleep(50 * time.Millisecond)

	addr := s.Addr()

	// Start a request
	go func() {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			t.Errorf("GET: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	}()

	// Wait for handler to start
	<-handlerStarted

	// Cancel context while request is in flight
	cancel()

	// Wait for Run to return
	if err := <-errCh; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Handler should have completed
	select {
	case <-handlerDone:
		// Good
	case <-time.After(1 * time.Second):
		t.Fatal("handler did not complete during drain")
	}
}

func TestExplicitZeroWriteTimeout(t *testing.T) {
	zero := 0.0
	s := New(WithBind(":0"), WithWriteTimeout(time.Duration(zero)))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	if s.writeTimeout != 0 {
		t.Fatalf("writeTimeout = %v, want 0", s.writeTimeout)
	}
}

func TestConfigWithExplicitZero(t *testing.T) {
	zero := 0.0
	cfg := ServerConfig{
		Bind:            Bind{":0"},
		WriteTimeoutSec: &zero,
	}
	s := New(WithConfig(cfg))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	if s.writeTimeout != 0 {
		t.Fatalf("writeTimeout = %v, want 0", s.writeTimeout)
	}
}

func TestMetricsAfterInit(t *testing.T) {
	s := New(WithBind(":0"))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	ms := s.Metrics()
	if ms == nil {
		t.Fatal("Metrics after Init should not be nil")
	}

	// Should have http_info
	found := false
	for _, m := range ms {
		if m.Name == "http_info" {
			found = true
			if m.Value != 1 {
				t.Fatalf("http_info value = %v, want 1", m.Value)
			}
		}
	}
	if !found {
		t.Fatal("http_info metric not found")
	}
}

func TestMetricsDisabled(t *testing.T) {
	s := New(WithBind(":0"), WithMetricsEnabled(false))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if ms := s.Metrics(); ms != nil {
		t.Fatalf("Metrics with metricsEnabled=false = %+v, want nil", ms)
	}
}

func TestRecord(t *testing.T) {
	s := New(WithBind(":0"))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	Record(s, "/test", 200, 100*time.Millisecond)
	Record(s, "/test", 200, 50*time.Millisecond)
	Record(s, "/test", 404, 25*time.Millisecond)
	Record(s, "/other", 500, 10*time.Millisecond)

	ms := s.Metrics()
	if ms == nil {
		t.Fatal("Metrics should not be nil")
	}

	// Check that we have request metrics
	foundRequests := false
	foundDuration := false
	for _, m := range ms {
		if m.Name == "http_requests_total" {
			foundRequests = true
		}
		if m.Name == "http_request_duration_seconds_count" {
			foundDuration = true
		}
	}
	if !foundRequests {
		t.Fatal("http_requests_total not found")
	}
	if !foundDuration {
		t.Fatal("http_request_duration_seconds_count not found")
	}
}

func TestRecordNilServer(t *testing.T) {
	// Should not panic
	Record(nil, "/test", 200, 100*time.Millisecond)
}

func TestRecordEmptyRoute(t *testing.T) {
	s := New(WithBind(":0"))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	Record(s, "", 200, 100*time.Millisecond)

	ms := s.Metrics()
	if ms == nil {
		t.Fatal("Metrics should not be nil")
	}

	// Should have "unknown" route with app instrumentation
	found := false
	for _, m := range ms {
		if m.Name == "http_requests_total" && m.Labels["route"] == "unknown" {
			if m.Labels["http_instrumentation"] != HTTPInstrumentationApp {
				t.Fatalf("http_instrumentation = %q, want %q",
					m.Labels["http_instrumentation"], HTTPInstrumentationApp)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("unknown route not found")
	}
}

func TestStatusClass(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{200, "2xx"},
		{201, "2xx"},
		{301, "3xx"},
		{400, "4xx"},
		{404, "4xx"},
		{500, "5xx"},
		{503, "5xx"},
		{0, "unknown"},
		{99, "unknown"},
		{600, "unknown"},
	}
	for _, tt := range tests {
		got := statusClass(tt.status)
		if got != tt.want {
			t.Errorf("statusClass(%d) = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestHealthAfterSetHandler(t *testing.T) {
	s := New(WithBind("127.0.0.1:0"))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	s.SetHandler(handler)

	if err := s.Health(context.Background()); err == nil {
		t.Fatal("Health after SetHandler should fail until Run is listening")
	}
}

func TestHealthWhileListening(t *testing.T) {
	s := New(WithBind("127.0.0.1:0"))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	s.SetHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()
	waitListening(t, s)
	if err := s.Health(context.Background()); err != nil {
		t.Fatalf("Health while listening: %v", err)
	}
	cancel()
	<-errCh
	if err := s.Health(context.Background()); err == nil {
		t.Fatal("Health after Run returns should fail")
	}
}

func waitListening(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.Health(context.Background()); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("server not listening")
}

func TestHealthAfterShutdown(t *testing.T) {
	s := New(WithBind(":0"))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	s.SetHandler(handler)
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := s.Health(context.Background()); err == nil {
		t.Fatal("Health after Shutdown should fail")
	}
}

func TestAddrBeforeRun(t *testing.T) {
	s := New(WithBind("127.0.0.1:8080"))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	if addr := s.Addr(); addr != "127.0.0.1:8080" {
		t.Fatalf("Addr() = %q, want 127.0.0.1:8080", addr)
	}
}

func TestAddrAfterRun(t *testing.T) {
	s := New(WithBind("127.0.0.1:0"))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	s.SetHandler(handler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	addr := s.Addr()
	if addr == "" || addr == "127.0.0.1:0" {
		t.Fatalf("Addr() = %q, want bound address", addr)
	}
	cancel()
	<-errCh
}

func TestValidateServerConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ServerConfig
		wantErr bool
	}{
		{
			name:    "valid",
			cfg:     ServerConfig{Bind: Bind{":0"}},
			wantErr: false,
		},
		{
			name: "negative timeout",
			cfg: ServerConfig{
				Bind:           Bind{":0"},
				ReadTimeoutSec: ptrFloat(-1),
			},
			wantErr: true,
		},
		{
			name: "zero shutdown timeout",
			cfg: ServerConfig{
				Bind:               Bind{":0"},
				ShutdownTimeoutSec: ptrFloat(0),
			},
			wantErr: true,
		},
		{
			name: "zero max header bytes",
			cfg: ServerConfig{
				Bind:           Bind{":0"},
				MaxHeaderBytes: ptrInt(0),
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateServerConfig(&tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateServerConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func ptrFloat(f float64) *float64 { return &f }
func ptrInt(i int) *int           { return &i }

func TestApplyConfig(t *testing.T) {
	readTimeout := 5.0
	writeTimeout := 0.0
	metricsEnabled := false
	cfg := ServerConfig{
		Bind:            Bind{":8080"},
		ReadTimeoutSec:  &readTimeout,
		WriteTimeoutSec: &writeTimeout,
		MetricsEnabled:  &metricsEnabled,
	}
	s := New()
	s.applyConfig(cfg)
	if len(s.binds) != 1 || s.binds[0] != ":8080" {
		t.Fatalf("bind = %q, want :8080", s.binds)
	}
	if s.readTimeout != 5*time.Second {
		t.Fatalf("readTimeout = %v, want 5s", s.readTimeout)
	}
	if s.writeTimeout != 0 {
		t.Fatalf("writeTimeout = %v, want 0", s.writeTimeout)
	}
	if s.metricsEnabled {
		t.Fatal("metricsEnabled should be false")
	}
}

func TestOnConfigReload(t *testing.T) {
	fw := cf.New()
	addComponent(t, fw, cf_logs.New())
	addComponent(t, fw, cf_configuration.New())
	s := New(WithBind(":0"), WithConfigSource("http", ""))
	addComponent(t, fw, s)
	if err := fw.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	t.Cleanup(func() { _ = fw.Shutdown(context.Background()) })

	metricsEnabled := false
	cfg := &ServerConfig{
		MetricsEnabled: &metricsEnabled,
	}
	s.OnConfigReload("http", cfg)

	if s.metricsEnabled {
		t.Fatal("metricsEnabled should be false after reload")
	}
	if s.reloads.Load() != 1 {
		t.Fatalf("reloads = %d, want 1", s.reloads.Load())
	}
}

func TestOnConfigReloadWrongSource(t *testing.T) {
	s := New(WithBind(":0"), WithConfigSource("http", "config/http.json"))
	// Don't initialize - just test the reload logic
	metricsEnabled := false
	cfg := &ServerConfig{
		MetricsEnabled: &metricsEnabled,
	}
	s.OnConfigReload("other", cfg)

	if s.reloads.Load() != 0 {
		t.Fatalf("reloads = %d, want 0", s.reloads.Load())
	}
}

func TestRunImmediateRestartPolicy(t *testing.T) {
	fw := cf.New()
	addComponent(t, fw, cf_logs.New())
	addComponent(t, fw, cf_configuration.New())
	s := New(WithBind("127.0.0.1:0"), WithConfigSource("http", ""))
	addComponent(t, fw, s)
	if err := fw.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	t.Cleanup(func() { _ = fw.Shutdown(context.Background()) })

	s.SetHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start running")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if addr := s.Addr(); addr == "" {
		t.Fatal("server not listening")
	}

	newAddress := "127.0.0.1:1"
	cfg := &ServerConfig{Bind: Bind{newAddress}, RestartPolicy: RestartPolicyImmediate}
	s.OnConfigReload("http", cfg)

	err := <-errCh
	if !errors.Is(err, ErrServerRestartRequired) {
		t.Fatalf("Run err = %v, want ErrServerRestartRequired", err)
	}
	if !s.restartRequested.Load() {
		t.Fatal("restartRequested should be set after immediate reload")
	}
}

func addComponent(t *testing.T, fw *cf.CaerusFramework, c cf.CaerusComponent) {
	t.Helper()
	if err := fw.AddComponent(c); err != nil {
		t.Fatalf("AddComponent: %v", err)
	}
}

func TestDependencies(t *testing.T) {
	s := New()
	deps := s.GetDependencies()
	if len(deps) != 1 || deps[0] != "logs" {
		t.Fatalf("GetDependencies() = %v, want [logs]", deps)
	}

	s2 := New(WithConfigSource("http", "config/http.json"))
	deps2 := s2.GetDependencies()
	if len(deps2) != 2 {
		t.Fatalf("GetDependencies() = %v, want [logs, configuration]", deps2)
	}
}

func TestConnState(t *testing.T) {
	s := New(WithBind("127.0.0.1:0"))
	if err := s.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s.SetHandler(handler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	addr := s.Addr()
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// Connection should be active or recently closed
	// We can't easily test the exact count, but we can verify no panic
	cancel()
	<-errCh
}

func TestNormalizeServerError(t *testing.T) {
	if err := normalizeServerError(http.ErrServerClosed); err != nil {
		t.Fatalf("normalizeServerError(ErrServerClosed) = %v, want nil", err)
	}
	if err := normalizeServerError(errors.New("other")); err == nil {
		t.Fatal("normalizeServerError(other) = nil, want error")
	}
}
