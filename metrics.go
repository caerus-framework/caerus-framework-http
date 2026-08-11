package cf_http

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cf_observability "github.com/caerus-framework/caerus-framework-observability"
)

type requestMeter struct {
	mu       sync.Mutex
	total    map[string]map[string]uint64
	sumNs    map[string]uint64
	count    map[string]uint64
	inFlight atomic.Int64
}

func newRequestMeter() *requestMeter {
	return &requestMeter{
		total: make(map[string]map[string]uint64),
		sumNs: make(map[string]uint64),
		count: make(map[string]uint64),
	}
}

// Values for the http_instrumentation metric label on http_requests_* series.
const (
	// HTTPInstrumentationMiddleware marks samples from cf_http.Metrics middleware
	// (route from r.Pattern / "unknown"). This is the usual REST path.
	HTTPInstrumentationMiddleware = "middleware"
	// HTTPInstrumentationApp marks samples from an explicit Record call
	// (Echo/chi shims, custom middleware). Also normal — use when the router
	// does not set r.Pattern.
	HTTPInstrumentationApp = "app"
)

// Record adds one completed request to the server meter with
// http_instrumentation="app". Empty routes are normalized to unknown so
// arbitrary URL paths never become metric labels.
func Record(server *Server, route string, status int, duration time.Duration) {
	recordHTTP(server, route, status, duration, HTTPInstrumentationApp)
}

// recordHTTPFromMiddleware is used by Metrics middleware
// (http_instrumentation="middleware").
func recordHTTPFromMiddleware(server *Server, route string, status int, duration time.Duration) {
	recordHTTP(server, route, status, duration, HTTPInstrumentationMiddleware)
}

func recordHTTP(server *Server, route string, status int, duration time.Duration, instrumentation string) {
	if server == nil || server.meter == nil {
		return
	}
	if route == "" {
		route = "unknown"
	}
	if instrumentation == "" {
		instrumentation = HTTPInstrumentationApp
	}
	if duration < 0 {
		duration = 0
	}
	key := route + graphqlKeySep + instrumentation
	class := statusClass(status)
	m := server.meter
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

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "unknown"
	}
	return fmt.Sprintf("%dxx", status/100)
}

// Metrics implements cf_observability.MetricsProvider.
func (c *Server) Metrics() []cf_observability.Metric {
	c.mu.Lock()
	initialized := c.initialized
	enabled := c.metricsEnabled
	name := c.Name()
	address := c.address
	reloads := c.reloads.Load()
	starts := c.starts.Load()
	drainTimeouts := c.drainTimeouts.Load()
	hijacks := c.hijacks.Load()
	connections := c.connections.Load()
	restartRequired := c.restartRequired.Load()
	c.mu.Unlock()
	if !initialized || !enabled {
		return nil
	}

	ms := []cf_observability.Metric{
		{
			Name:   "http_info",
			Help:   "HTTP server descriptor; 1 while initialized.",
			Value:  1,
			Labels: map[string]string{"component": name, "address": address},
		},
		{
			Name:   "http_requests_in_flight",
			Help:   "Requests currently executing through HTTP metrics middleware.",
			Value:  float64(c.meter.inFlight.Load()),
			Labels: map[string]string{"component": name},
		},
		{
			Name:   "http_connections_active",
			Help:   "Active connections owned by net/http.",
			Value:  float64(connections),
			Labels: map[string]string{"component": name},
		},
		{
			Name:   "http_config_reloads_total",
			Help:   "Successful HTTP configuration reload notifications.",
			Value:  float64(reloads),
			Labels: map[string]string{"component": name},
			Type:   cf_observability.MetricTypeCounter,
		},
		{
			Name:   "http_server_starts_total",
			Help:   "Successful HTTP listener starts.",
			Value:  float64(starts),
			Labels: map[string]string{"component": name},
			Type:   cf_observability.MetricTypeCounter,
		},
		{
			Name:   "http_server_drain_timeouts_total",
			Help:   "Graceful HTTP drains that reached their deadline.",
			Value:  float64(drainTimeouts),
			Labels: map[string]string{"component": name},
			Type:   cf_observability.MetricTypeCounter,
		},
		{
			Name:   "http_hijacks_total",
			Help:   "Connections transferred out of net/http ownership.",
			Value:  float64(hijacks),
			Labels: map[string]string{"component": name},
			Type:   cf_observability.MetricTypeCounter,
		},
		{
			Name:   "http_server_restart_required",
			Help:   "Server requires restart due to configuration changes (1 = restart needed, 0 = no restart needed).",
			Value:  boolToFloat(restartRequired),
			Labels: map[string]string{"component": name},
		},
	}

	m := c.meter
	m.mu.Lock()
	keys := make([]string, 0, len(m.total))
	for key := range m.total {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		route, instrumentation, ok := strings.Cut(key, graphqlKeySep)
		if !ok {
			route = key
			instrumentation = HTTPInstrumentationApp
		}
		classes := m.total[key]
		classNames := make([]string, 0, len(classes))
		for class := range classes {
			classNames = append(classNames, class)
		}
		sort.Strings(classNames)
		for _, class := range classNames {
			ms = append(ms, cf_observability.Metric{
				Name: "http_requests_total",
				Help: "Completed HTTP requests by route, status class, and " +
					"instrumentation (middleware|app).",
				Value: float64(classes[class]),
				Labels: map[string]string{
					"component": name, "route": route, "status_class": class,
					"http_instrumentation": instrumentation,
				},
				Type: cf_observability.MetricTypeCounter,
			})
		}
		labels := map[string]string{
			"component": name, "route": route, "http_instrumentation": instrumentation,
		}
		ms = append(ms,
			cf_observability.Metric{
				Name:   "http_request_duration_seconds_sum",
				Help:   "Total HTTP request duration in seconds.",
				Value:  float64(m.sumNs[key]) / float64(time.Second),
				Labels: copyStringMap(labels),
				Type:   cf_observability.MetricTypeCounter,
			},
			cf_observability.Metric{
				Name:   "http_request_duration_seconds_count",
				Help:   "Number of HTTP request duration observations.",
				Value:  float64(m.count[key]),
				Labels: copyStringMap(labels),
				Type:   cf_observability.MetricTypeCounter,
			},
		)
	}
	m.mu.Unlock()

	if c.graphqlOps != nil {
		ms = append(ms, renderGraphQLMeter(c.graphqlOps, name, "operation")...)
	}
	if c.graphqlResolvers != nil {
		ms = append(ms, renderGraphQLMeter(c.graphqlResolvers, name, "resolver")...)
	}
	return ms
}

// renderGraphQLMeter renders a request-shaped GraphQL meter into the
// http_graphql_operations_* or http_graphql_resolvers_* series.
//
// Keys:
//   - operation: "operation\x1finstrumentation"
//   - resolver:  "operation\x1fresolver\x1finstrumentation"
//
// graphql_instrumentation is "app" (engine/explicit) or "http_peek" (auto
// body-peek middleware) so scrapes can flag non-standard instrumentation.
func renderGraphQLMeter(m *requestMeter, component, kind string) []cf_observability.Metric {
	sample := "http_graphql_operations_total"
	durationName := "http_graphql_operation_duration_seconds"
	helpTotal := "Completed GraphQL operation calls by operation, status class, and instrumentation (app|http_peek)."
	helpSum := "Total GraphQL operation duration in seconds."
	helpCount := "Number of GraphQL operation duration observations."
	if kind == "resolver" {
		sample = "http_graphql_resolvers_total"
		durationName = "http_graphql_resolver_duration_seconds"
		helpTotal = "Completed GraphQL resolver calls by operation, resolver, status class, and instrumentation (app|http_peek)."
		helpSum = "Total GraphQL resolver duration in seconds."
		helpCount = "Number of GraphQL resolver duration observations."
	}

	ms := []cf_observability.Metric{}
	m.mu.Lock()
	defer m.mu.Unlock()

	keys := make([]string, 0, len(m.total))
	for key := range m.total {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		labels := map[string]string{"component": component}
		parts := strings.Split(key, graphqlKeySep)
		switch kind {
		case "resolver":
			if len(parts) >= 3 {
				labels["operation"] = parts[0]
				labels["resolver"] = parts[1]
				labels["graphql_instrumentation"] = parts[2]
			} else if len(parts) == 2 {
				// Legacy key without instrumentation.
				labels["operation"] = parts[0]
				labels["resolver"] = parts[1]
				labels["graphql_instrumentation"] = GraphQLInstrumentationApp
			} else {
				labels["operation"] = key
				labels["resolver"] = "unknown"
				labels["graphql_instrumentation"] = GraphQLInstrumentationApp
			}
		default:
			if len(parts) >= 2 {
				labels["operation"] = parts[0]
				labels["graphql_instrumentation"] = parts[1]
			} else {
				labels["operation"] = key
				labels["graphql_instrumentation"] = GraphQLInstrumentationApp
			}
		}

		classes := m.total[key]
		classNames := make([]string, 0, len(classes))
		for class := range classes {
			classNames = append(classNames, class)
		}
		sort.Strings(classNames)
		for _, class := range classNames {
			classLabels := copyStringMap(labels)
			classLabels["status_class"] = class
			ms = append(ms, cf_observability.Metric{
				Name:   sample,
				Help:   helpTotal,
				Value:  float64(classes[class]),
				Labels: classLabels,
				Type:   cf_observability.MetricTypeCounter,
			})
		}
		ms = append(ms,
			cf_observability.Metric{
				Name:   durationName + "_sum",
				Help:   helpSum,
				Value:  float64(m.sumNs[key]) / float64(time.Second),
				Labels: copyStringMap(labels),
				Type:   cf_observability.MetricTypeCounter,
			},
			cf_observability.Metric{
				Name:   durationName + "_count",
				Help:   helpCount,
				Value:  float64(m.count[key]),
				Labels: copyStringMap(labels),
				Type:   cf_observability.MetricTypeCounter,
			},
		)
	}
	return ms
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
