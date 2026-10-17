package cf_http

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	cf "github.com/caerus-framework/caerus-framework"
	cf_configuration "github.com/caerus-framework/caerus-framework-configuration"
)

// SourceOption configures the self-registered HTTP source.
type SourceOption func(*sourceOptions)

type sourceOptions struct {
	envPrefix string
	format    cf_configuration.Format
	formatSet bool
}

// WithSourceEnvPrefix overrides the environment prefix for a source.
func WithSourceEnvPrefix(prefix string) SourceOption {
	return func(o *sourceOptions) { o.envPrefix = prefix }
}

// WithSourceFormat forces the source file format.
func WithSourceFormat(format cf_configuration.Format) SourceOption {
	return func(o *sourceOptions) {
		o.format = format
		o.formatSet = true
	}
}

func defaultSourceEnvPrefix(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_"
}

// WithConfigSource binds the server to a self-registered configuration source.
func WithConfigSource(name, path string, opts ...SourceOption) Option {
	return func(o *options) {
		so := sourceOptions{envPrefix: defaultSourceEnvPrefix(name)}
		for _, opt := range opts {
			opt(&so)
		}
		o.configSource = name
		o.configPath = path
		o.srcEnvPrefix = so.envPrefix
		o.srcFormat = so.format
		o.srcFormatSet = so.formatSet
	}
}

// RegisterConfigSources implements cf.ConfigSourceRegistrar.
func (c *Server) RegisterConfigSources(conf any) error {
	configuration, ok := conf.(*cf_configuration.Configuration)
	if !ok {
		return fmt.Errorf("cf_http: RegisterConfigSources: expected configuration component, got %T", conf)
	}
	if c.configSource == "" {
		return nil
	}
	format := c.srcFormat
	if !c.srcFormatSet {
		path := strings.ToLower(c.configPath)
		if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
			format = cf_configuration.FormatYAML
		} else {
			format = cf_configuration.FormatJSON
		}
	}
	return cf_configuration.AddSource(configuration, cf_configuration.Source[ServerConfig]{
		Name:      c.configSource,
		Path:      c.configPath,
		Format:    format,
		Owner:     c.Name(),
		EnvPrefix: c.srcEnvPrefix,
		Validate:  validateServerConfig,
	})
}

func (c *Server) applyConfigFromSource() error {
	configuration, ok := cf.Get[*cf_configuration.Configuration](c.fw)
	if !ok {
		return errors.New("cf_http: configuration component not registered")
	}
	cfg, ok := cf_configuration.Get[ServerConfig](configuration, c.configSource)
	if !ok {
		return fmt.Errorf("cf_http: configuration source %q not found", c.configSource)
	}
	c.applyConfig(cfg)
	return nil
}

func (c *Server) applyConfig(cfg ServerConfig) {
	if len(cfg.Bind) > 0 {
		c.binds = append([]string{}, cfg.Bind...)
	}
	if cfg.ReadTimeoutSec != nil {
		c.readTimeout = time.Duration(*cfg.ReadTimeoutSec * float64(time.Second))
	}
	if cfg.WriteTimeoutSec != nil {
		c.writeTimeout = time.Duration(*cfg.WriteTimeoutSec * float64(time.Second))
	}
	if cfg.IdleTimeoutSec != nil {
		c.idleTimeout = time.Duration(*cfg.IdleTimeoutSec * float64(time.Second))
	}
	if cfg.ReadHeaderTimeoutSec != nil {
		c.readHeaderTimeout = time.Duration(*cfg.ReadHeaderTimeoutSec * float64(time.Second))
	}
	if cfg.MaxHeaderBytes != nil {
		c.maxHeaderBytes = *cfg.MaxHeaderBytes
	}
	if cfg.ShutdownTimeoutSec != nil {
		c.shutdownTimeout = time.Duration(*cfg.ShutdownTimeoutSec * float64(time.Second))
	}
	if cfg.MetricsEnabled != nil {
		c.metricsEnabled = *cfg.MetricsEnabled
	}
	if cfg.RestartPolicy != "" {
		c.restartPolicy = cfg.RestartPolicy
	}
}

// OnConfigReload implements cf.ConfigReloader. Metrics enablement is live;
// listener settings remain active until restart based on restart policy.
func (c *Server) OnConfigReload(source string, cfg any) {
	if source != c.configSource {
		return
	}
	loaded, ok := cfg.(*ServerConfig)
	if !ok {
		c.loggerValue().Error("cf_http: config reload rejected", "source", source, "type", fmt.Sprintf("%T", cfg))
		return
	}
	c.mu.Lock()
	if !c.initialized {
		c.mu.Unlock()
		return
	}

	settingsChanged := serverSettingsChanged(c, loaded)

	if loaded.MetricsEnabled != nil {
		c.metricsEnabled = *loaded.MetricsEnabled
	}

	// Update restart policy if changed. The policy field itself is not a
	// restart-required setting; it only selects how a settings change is handled.
	if loaded.RestartPolicy != "" {
		c.restartPolicy = loaded.RestartPolicy
	}

	if settingsChanged {
		switch c.restartPolicy {
		case RestartPolicyImmediate:
			// Grab the Run cancel func under the lock, then act outside it so
			// the loud log and the cancel are never ordered with a stale lock.
			// restartRequested makes Run return ErrServerRestartRequired after it
			// drains, so the process exits and a fresh instance rebinds.
			c.restartRequired.Store(true)
			c.restartRequested.Store(true)
			cancel := c.runCancel
			c.mu.Unlock()
			c.loggerValue().Error("cf_http: server settings changed; restart_policy=immediate — gracefully stopping so the process exits and a fresh instance rebinds",
				"source", source,
				"bind", loaded.Bind,
			)
			if cancel != nil {
				cancel()
			}
			c.reloads.Add(1)
			return
		case RestartPolicyHandled:
			fallthrough
		default:
			c.restartRequired.Store(true)
			c.loggerValue().Error("cf_http: server settings changed; restart required — NO LIVE REBIND: the current listener stays on the old settings until a NEW PROCESS rebinds (roll the Deployment / restart the process)",
				"source", source,
				"bind", loaded.Bind,
			)
		}
	}

	c.mu.Unlock()
	c.reloads.Add(1)
	c.loggerValue().Info("cf_http: configuration reloaded", "source", source)
}

func serverSettingsChanged(c *Server, cfg *ServerConfig) bool {
	if len(cfg.Bind) > 0 && !bindsEqual(cfg.Bind, c.binds) {
		return true
	}
	if cfg.ReadTimeoutSec != nil && time.Duration(*cfg.ReadTimeoutSec*float64(time.Second)) != c.readTimeout {
		return true
	}
	if cfg.WriteTimeoutSec != nil && time.Duration(*cfg.WriteTimeoutSec*float64(time.Second)) != c.writeTimeout {
		return true
	}
	if cfg.IdleTimeoutSec != nil && time.Duration(*cfg.IdleTimeoutSec*float64(time.Second)) != c.idleTimeout {
		return true
	}
	if cfg.ReadHeaderTimeoutSec != nil && time.Duration(*cfg.ReadHeaderTimeoutSec*float64(time.Second)) != c.readHeaderTimeout {
		return true
	}
	if cfg.MaxHeaderBytes != nil && *cfg.MaxHeaderBytes != c.maxHeaderBytes {
		return true
	}
	if cfg.ShutdownTimeoutSec != nil && time.Duration(*cfg.ShutdownTimeoutSec*float64(time.Second)) != c.shutdownTimeout {
		return true
	}
	return false
}

func validateServerConfig(cfg *ServerConfig) error {
	for name, value := range map[string]*float64{
		"read_timeout_sec":        cfg.ReadTimeoutSec,
		"write_timeout_sec":       cfg.WriteTimeoutSec,
		"idle_timeout_sec":        cfg.IdleTimeoutSec,
		"read_header_timeout_sec": cfg.ReadHeaderTimeoutSec,
	} {
		if value != nil {
			if err := validateSeconds(name, *value, false); err != nil {
				return err
			}
		}
	}
	if cfg.ShutdownTimeoutSec != nil {
		if err := validateSeconds("shutdown_timeout_sec", *cfg.ShutdownTimeoutSec, true); err != nil {
			return err
		}
	}
	if cfg.MaxHeaderBytes != nil && *cfg.MaxHeaderBytes <= 0 {
		return errors.New("cf_http: max_header_bytes must be positive")
	}
	switch cfg.RestartPolicy {
	case "", RestartPolicyHandled, RestartPolicyImmediate:
	default:
		return fmt.Errorf("cf_http: unknown restart_policy %q (want %q or %q)",
			cfg.RestartPolicy, RestartPolicyHandled, RestartPolicyImmediate)
	}
	return nil
}

func validateSeconds(name string, value float64, positive bool) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || (positive && value == 0) {
		return fmt.Errorf("cf_http: %s must be %s", name, func() string {
			if positive {
				return "positive and finite"
			}
			return "non-negative and finite"
		}())
	}
	if value > float64((1<<63-1))/float64(time.Second) {
		return fmt.Errorf("cf_http: %s is too large", name)
	}
	return nil
}
