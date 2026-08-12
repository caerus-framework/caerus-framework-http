package cf_http

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cf "github.com/caerus-framework/caerus-framework"
	cf_configuration "github.com/caerus-framework/caerus-framework-configuration"
	cf_logs "github.com/caerus-framework/caerus-framework-logs"

	cf_observability "github.com/caerus-framework/caerus-framework-observability"
)

const (
	// ComponentName is the default framework name for the HTTP server.
	ComponentName = "http"
	// ComponentStage is the serving plane, above the data plane.
	ComponentStage = cf.Stage("app")
)

// RestartPolicy controls behavior when server settings change during config reload.
type RestartPolicy string

const (
	// RestartPolicyHandled logs a warning and continues with current settings.
	// This is the default and safest option for production.
	RestartPolicyHandled RestartPolicy = "handled"

	// RestartPolicyImmediate gracefully stops the server when settings change.
	// The server must be restarted externally with new settings.
	RestartPolicyImmediate RestartPolicy = "immediate"
)

// ServerConfig is the file/env-drivable HTTP server configuration.
// Pointer fields distinguish omitted values from explicit zero values.
type ServerConfig struct {
	Address              string        `json:"address,omitempty" yaml:"address,omitempty" env:"ADDRESS" flag:"http-address"`
	ReadTimeoutSec       *float64      `json:"read_timeout_sec,omitempty" yaml:"read_timeout_sec,omitempty" env:"READ_TIMEOUT_SEC" flag:"http-read-timeout-sec"`
	WriteTimeoutSec      *float64      `json:"write_timeout_sec,omitempty" yaml:"write_timeout_sec,omitempty" env:"WRITE_TIMEOUT_SEC" flag:"http-write-timeout-sec"`
	IdleTimeoutSec       *float64      `json:"idle_timeout_sec,omitempty" yaml:"idle_timeout_sec,omitempty" env:"IDLE_TIMEOUT_SEC" flag:"http-idle-timeout-sec"`
	ReadHeaderTimeoutSec *float64      `json:"read_header_timeout_sec,omitempty" yaml:"read_header_timeout_sec,omitempty" env:"READ_HEADER_TIMEOUT_SEC" flag:"http-read-header-timeout-sec"`
	MaxHeaderBytes       *int          `json:"max_header_bytes,omitempty" yaml:"max_header_bytes,omitempty" env:"MAX_HEADER_BYTES" flag:"http-max-header-bytes"`
	ShutdownTimeoutSec   *float64      `json:"shutdown_timeout_sec,omitempty" yaml:"shutdown_timeout_sec,omitempty" env:"SHUTDOWN_TIMEOUT_SEC" flag:"http-shutdown-timeout-sec"`
	MetricsEnabled       *bool         `json:"metrics_enabled,omitempty" yaml:"metrics_enabled,omitempty" env:"METRICS_ENABLED" flag:"http-metrics-enabled"`
	RestartPolicy        RestartPolicy `json:"restart_policy,omitempty" yaml:"restart_policy,omitempty" env:"RESTART_POLICY" flag:"http-restart-policy"`
}

// Option configures a Server at construction time.
type Option func(*options)

type options struct {
	loaded            *ServerConfig
	configSource      string
	configPath        string
	srcEnvPrefix      string
	srcFormat         cf_configuration.Format
	srcFormatSet      bool
	address           string
	readTimeout       time.Duration
	writeTimeout      time.Duration
	idleTimeout       time.Duration
	readHeaderTimeout time.Duration
	maxHeaderBytes    int
	shutdownTimeout   time.Duration
	metricsEnabled    bool
	restartPolicy     RestartPolicy
	logger            *slog.Logger
	loggerSet         bool
	name              string
}

// WithConfig applies a static configuration snapshot.
func WithConfig(cfg ServerConfig) Option {
	return func(o *options) { o.loaded = &cfg }
}

// WithAddress sets the listen address.
func WithAddress(address string) Option {
	return func(o *options) { o.address = address }
}

// WithReadTimeout sets the maximum read duration.
func WithReadTimeout(d time.Duration) Option {
	return func(o *options) { o.readTimeout = d }
}

// WithWriteTimeout sets the maximum write duration. Zero disables the deadline.
func WithWriteTimeout(d time.Duration) Option {
	return func(o *options) { o.writeTimeout = d }
}

// WithIdleTimeout sets the keep-alive idle timeout.
func WithIdleTimeout(d time.Duration) Option {
	return func(o *options) { o.idleTimeout = d }
}

// WithReadHeaderTimeout sets the maximum header-read duration.
func WithReadHeaderTimeout(d time.Duration) Option {
	return func(o *options) { o.readHeaderTimeout = d }
}

// WithMaxHeaderBytes sets the maximum request header size.
func WithMaxHeaderBytes(n int) Option {
	return func(o *options) { o.maxHeaderBytes = n }
}

// WithShutdownTimeout sets the graceful drain deadline.
func WithShutdownTimeout(d time.Duration) Option {
	return func(o *options) { o.shutdownTimeout = d }
}

// WithMetricsEnabled enables or disables the component MetricsProvider.
func WithMetricsEnabled(enabled bool) Option {
	return func(o *options) { o.metricsEnabled = enabled }
}

// WithRestartPolicy sets what happens when a live reload changes settings
// that cannot rebind in place ("handled" default, or "immediate").
func WithRestartPolicy(policy string) Option {
	return func(o *options) { o.restartPolicy = RestartPolicy(policy) }
}

// WithLogger supplies an explicit logger for tests or embedded use.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) {
		o.logger = logger
		o.loggerSet = true
	}
}

// WithName sets a custom component name for multiple HTTP instances.
func WithName(name string) Option {
	return func(o *options) { o.name = name }
}

// Server owns the net/http serving lifecycle for an app-owned handler.
type Server struct {
	mu sync.Mutex

	configSource string
	configPath   string
	srcEnvPrefix string
	srcFormat    cf_configuration.Format
	srcFormatSet bool

	address           string
	readTimeout       time.Duration
	writeTimeout      time.Duration
	idleTimeout       time.Duration
	readHeaderTimeout time.Duration
	maxHeaderBytes    int
	shutdownTimeout   time.Duration
	metricsEnabled    bool
	restartPolicy     RestartPolicy
	restartRequired   atomic.Bool
	restartRequested  atomic.Bool
	runCancel         context.CancelFunc
	initialized       bool
	running           bool
	boundAddress      string
	loggerSet         bool
	logger            atomic.Pointer[slog.Logger]
	logsSub           *cf_logs.Subscription
	handler           http.Handler
	fw                *cf.CaerusFramework
	name              string
	meter             *requestMeter
	graphqlOps        *requestMeter
	graphqlResolvers  *requestMeter
	reloads           atomic.Uint64
	starts            atomic.Uint64
	drainTimeouts     atomic.Uint64
	hijacks           atomic.Uint64
	connections       atomic.Int64
}

// New creates an inert HTTP server component. It does not bind a port.
func New(opts ...Option) *Server {
	o := options{
		readTimeout:       10 * time.Second,
		writeTimeout:      10 * time.Second,
		idleTimeout:       60 * time.Second,
		readHeaderTimeout: 5 * time.Second,
		maxHeaderBytes:    1 << 20,
		shutdownTimeout:   10 * time.Second,
		metricsEnabled:    true,
		restartPolicy:     RestartPolicyHandled,
		logger:            slog.Default(),
	}
	for _, opt := range opts {
		opt(&o)
	}
	c := &Server{
		configSource:      o.configSource,
		configPath:        o.configPath,
		srcEnvPrefix:      o.srcEnvPrefix,
		srcFormat:         o.srcFormat,
		srcFormatSet:      o.srcFormatSet,
		address:           o.address,
		readTimeout:       o.readTimeout,
		writeTimeout:      o.writeTimeout,
		idleTimeout:       o.idleTimeout,
		readHeaderTimeout: o.readHeaderTimeout,
		maxHeaderBytes:    o.maxHeaderBytes,
		shutdownTimeout:   o.shutdownTimeout,
		metricsEnabled:    o.metricsEnabled,
		restartPolicy:     o.restartPolicy,
		loggerSet:         o.loggerSet,
		name:              o.name,
		meter:             newRequestMeter(),
		graphqlOps:        newRequestMeter(),
		graphqlResolvers:  newRequestMeter(),
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	c.logger.Store(o.logger)
	if o.loaded != nil {
		c.applyConfig(*o.loaded)
	}
	return c
}

// Name implements cf.CaerusComponent.
func (c *Server) Name() string {
	if c.name != "" {
		return c.name
	}
	return ComponentName
}

// Logger returns the current logger for the server.
// This is useful for middleware that needs to log with the same logger.
func (c *Server) Logger() *slog.Logger {
	return c.loggerValue()
}

// GetInitOrderStage implements cf.CaerusComponent.
func (c *Server) GetInitOrderStage() cf.Stage { return ComponentStage }

// GetDependencies implements cf.Dependencies.
func (c *Server) GetDependencies() []string {
	deps := []string{cf_logs.ComponentName}
	if c.configSource != "" {
		deps = append(deps, cf_configuration.ComponentName)
	}
	return deps
}

// Init implements cf.CaerusComponent.
func (c *Server) Init(ctx context.Context, fw *cf.CaerusFramework) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initialized {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.fw = fw
	if !c.loggerSet {
		if logs, ok := cf.Get[*cf_logs.Logs](fw); ok {
			c.logsSub = logs.OnReconfigureFor(c.Name(), func(l *slog.Logger) {
				c.logger.Store(l)
			})
		}
	}
	if c.configSource != "" {
		if err := c.applyConfigFromSource(); err != nil {
			c.unsubscribeLogs()
			return err
		}
	}
	if err := c.validateActiveSettings(); err != nil {
		c.unsubscribeLogs()
		return err
	}
	c.initialized = true
	c.loggerValue().Info("cf_http: initialized",
		"address", c.address,
		"read_timeout", c.readTimeout,
		"write_timeout", c.writeTimeout,
		"idle_timeout", c.idleTimeout,
	)
	return nil
}

// SetHandler registers the app-owned HTTP handler. It must be called before Run.
func (c *Server) SetHandler(handler http.Handler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handler = handler
}

// Handler returns the currently registered handler.
func (c *Server) Handler() http.Handler {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handler
}

// Addr returns the active listener address, or the configured address before
// serving. It is useful when binding to port zero in tests.
func (c *Server) Addr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.boundAddress != "" {
		return c.boundAddress
	}
	return c.address
}

// Run implements cf.Runnable.
func (c *Server) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if !c.initialized {
		c.mu.Unlock()
		return errors.New("cf_http: Run before Init")
	}
	if c.running {
		c.mu.Unlock()
		return errors.New("cf_http: Run already active")
	}
	if c.handler == nil {
		c.mu.Unlock()
		return errors.New("cf_http: Run before SetHandler")
	}
	handler := c.handler
	address := c.address
	readTimeout := c.readTimeout
	writeTimeout := c.writeTimeout
	idleTimeout := c.idleTimeout
	readHeaderTimeout := c.readHeaderTimeout
	maxHeaderBytes := c.maxHeaderBytes
	shutdownTimeout := c.shutdownTimeout

	runCtx, runCancel := context.WithCancel(ctx)
	c.running = true
	c.runCancel = runCancel
	c.restartRequested.Store(false)
	c.mu.Unlock()
	defer func() {
		runCancel()
		c.mu.Lock()
		c.running = false
		c.runCancel = nil
		c.boundAddress = ""
		c.mu.Unlock()
	}()

	if err := ctx.Err(); err != nil {
		return err
	}
	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadTimeout:       readTimeout,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          log.New(&frameworkLogWriter{server: c}, "", 0),
		ConnState:         c.connState,
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		c.loggerValue().Error("cf_http: listen failed", "address", address, "err", err)
		return err
	}
	if err := runCtx.Err(); err != nil {
		_ = listener.Close()
		return err
	}
	c.starts.Add(1)
	c.mu.Lock()
	c.boundAddress = listener.Addr().String()
	c.mu.Unlock()
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(listener) }()

	select {
	case err := <-errCh:
		return normalizeServerError(err)
	case <-runCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		shutdownErr := server.Shutdown(shutdownCtx)
		serveErr := normalizeServerError(<-errCh)
		cancel()
		if shutdownErr != nil {
			if errors.Is(shutdownErr, context.DeadlineExceeded) || errors.Is(shutdownCtx.Err(), context.DeadlineExceeded) {
				c.drainTimeouts.Add(1)
			}
			c.loggerValue().Error("cf_http: graceful shutdown failed", "err", shutdownErr)
			return shutdownErr
		}
		if c.restartRequested.Load() {
			return ErrServerRestartRequired
		}
		return serveErr
	}
}

// ErrServerRestartRequired is returned by Run when a live configuration reload
// changed settings that cannot rebind in place and the active restart policy
// was "immediate". Run has already drained and returned; the process should
// exit so the orchestrator starts a fresh instance with the new settings.
var ErrServerRestartRequired = errors.New("cf_http: server settings changed; immediate restart requested")

// Health implements cf.HealthProvider.
func (c *Server) Health(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.initialized {
		return errors.New("cf_http: server is not initialized")
	}
	if c.handler == nil {
		return errors.New("cf_http: handler is not registered")
	}
	return nil
}

// Shutdown implements cf.CaerusComponent. Run performs the listener drain.
func (c *Server) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unsubscribeLogs()
	c.initialized = false
	c.fw = nil
	return nil
}

func (c *Server) unsubscribeLogs() {
	if c.logsSub != nil {
		c.logsSub.Unsubscribe()
		c.logsSub = nil
	}
}

func (c *Server) loggerValue() *slog.Logger {
	if logger := c.logger.Load(); logger != nil {
		return logger
	}
	return slog.Default()
}

func normalizeServerError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// frameworkLogWriter writes through the current framework logger so that
// http.Server errors respect live logger reconfiguration.
type frameworkLogWriter struct {
	server *Server
}

func (w *frameworkLogWriter) Write(p []byte) (n int, err error) {
	logger := w.server.loggerValue()
	msg := strings.TrimSuffix(string(p), "\n")
	logger.Error("http.Server", "msg", msg)
	return len(p), nil
}

func (c *Server) connState(_ net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		c.connections.Add(1)
	case http.StateClosed:
		c.connections.Add(-1)
	case http.StateHijacked:
		c.connections.Add(-1)
		c.hijacks.Add(1)
	}
}

func (c *Server) validateActiveSettings() error {
	if c.address == "" {
		return errors.New("cf_http: address is required")
	}
	if c.readTimeout < 0 || c.writeTimeout < 0 || c.idleTimeout < 0 || c.readHeaderTimeout < 0 {
		return errors.New("cf_http: server timeouts must not be negative")
	}
	if c.maxHeaderBytes <= 0 {
		return errors.New("cf_http: max_header_bytes must be positive")
	}
	if c.shutdownTimeout <= 0 {
		return errors.New("cf_http: shutdown timeout must be positive")
	}
	switch c.restartPolicy {
	case "", RestartPolicyHandled, RestartPolicyImmediate:
	default:
		return errors.New("cf_http: unknown restart_policy " + strconv.Quote(string(c.restartPolicy)))
	}
	return nil
}

// graphqlKeySep joins GraphQL meter key parts; the metrics renderer splits
// them back into bounded labels.
const graphqlKeySep = "\x1f"

// Values for the graphql_instrumentation metric label.
const (
	// GraphQLInstrumentationApp marks samples recorded by application /
	// engine hooks (RecordGraphQLMetric, StartOperation, RecordResolver).
	GraphQLInstrumentationApp = "app"
	// GraphQLInstrumentationHTTPPeek marks samples from graphql.Metrics
	// auto body-peek extraction — convenient, not the usual production path.
	GraphQLInstrumentationHTTPPeek = "http_peek"
)

// RecordGraphQLMetric records one GraphQL operation sample in the dedicated
// http_graphql_operations_* series with graphql_instrumentation="app".
// Empty operation names are normalized to "unknown". Callers must pass
// bounded operation names (allowlist / codegen).
func (c *Server) RecordGraphQLMetric(operation string, status int, duration time.Duration) {
	c.recordGraphQLOp(operation, status, duration, GraphQLInstrumentationApp)
}

// RecordGraphQLMetricFromHTTPPeek records a sample produced by graphql.Metrics
// auto body-peek extraction (graphql_instrumentation="http_peek"). Prefer
// RecordGraphQLMetric or engine hooks in production; use the label to find
// leftover auto-instrumentation in scrapes / dashboards.
func (c *Server) RecordGraphQLMetricFromHTTPPeek(operation string, status int, duration time.Duration) {
	c.recordGraphQLOp(operation, status, duration, GraphQLInstrumentationHTTPPeek)
}

func (c *Server) recordGraphQLOp(operation string, status int, duration time.Duration, instrumentation string) {
	if operation == "" {
		operation = "unknown"
	}
	if instrumentation == "" {
		instrumentation = GraphQLInstrumentationApp
	}
	recordMeter(c.graphqlOps, operation+graphqlKeySep+instrumentation, status, duration)
}

// RecordGraphQLResolverMetric records one GraphQL resolver sample in the
// dedicated http_graphql_resolvers_* series with graphql_instrumentation="app".
// Empty operation/resolver values are normalized to "unknown". Cardinality is
// caller-owned: pass bounded labels (codegen / app allowlist) only.
func (c *Server) RecordGraphQLResolverMetric(operation, resolver string, status int, duration time.Duration) {
	if operation == "" {
		operation = "unknown"
	}
	if resolver == "" {
		resolver = "unknown"
	}
	recordMeter(c.graphqlResolvers, operation+graphqlKeySep+resolver+graphqlKeySep+GraphQLInstrumentationApp, status, duration)
}

// recordMeter records one sample into a request-shaped meter, normalizing empty
// keys to "unknown" and negative durations to zero.
func recordMeter(m *requestMeter, key string, status int, duration time.Duration) {
	if m == nil {
		return
	}
	if key == "" {
		key = "unknown"
	}
	if duration < 0 {
		duration = 0
	}
	class := statusClass(status)

	m.mu.Lock()
	classes := m.total[key]
	if classes == nil {
		classes = make(map[string]uint64)
		m.total[key] = classes
	}
	classes[class]++
	m.sumNs[key] += uint64(duration)
	m.count[key]++
	m.mu.Unlock()
}

var _ cf.CaerusComponent = (*Server)(nil)
var _ cf.Dependencies = (*Server)(nil)
var _ cf.Runnable = (*Server)(nil)
var _ cf.HealthProvider = (*Server)(nil)
var _ cf_observability.MetricsProvider = (*Server)(nil)
var _ cf.ConfigReloader = (*Server)(nil)
var _ cf.ConfigSourceRegistrar = (*Server)(nil)
