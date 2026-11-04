package caddyconsul

import (
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// MetricsCollector handles all metrics collection for the caddy-consul plugin.
// All recorder methods are nil-safe: when metrics are disabled the collector is
// nil and every recorder returns immediately, so call sites need no guards.
type MetricsCollector struct {
	logger   *zap.Logger
	registry *prometheus.Registry

	ServicesTotal    prometheus.Gauge
	RoutesTotal      *prometheus.GaugeVec
	UpstreamsHealthy *prometheus.GaugeVec
	UpstreamsTotal   *prometheus.GaugeVec

	ReconcileDuration prometheus.Histogram
	ReconcileErrors   prometheus.Counter
	WatcherErrors     prometheus.Counter
	ConflictsTotal    *prometheus.CounterVec
	DebounceEvents    prometheus.Counter

	// TCP/L4 self-managed listeners (native path — no caddy-l4)
	TCPListenersActive    prometheus.Gauge
	TCPListenerOpens      prometheus.Counter
	TCPListenerCloses     prometheus.Counter
	TCPListenErrors       prometheus.Counter
	TCPConnectionsTotal   prometheus.Counter
	TCPConnectionsActive  prometheus.Gauge
	TCPNoRoute            prometheus.Counter
	TCPNoUpstream         prometheus.Counter
	TCPUpstreamDialErrors prometheus.Counter
	TCPForceClosed        prometheus.Counter

	// Per-route request counter (consul_route_info `metrics`). Created on first use, because its
	// extra label names come from handler config; see RouteRequestsCounter. It lives in its own
	// registry: a Prometheus registry never forgets a metric's label names (Unregister keeps
	// them), so a label change needs a fresh registry. ServeHTTP gathers both.
	routeRequestsMu       sync.Mutex
	routeRequests         *prometheus.CounterVec
	routeRequestsLabels   []string
	routeRequestsRegistry *prometheus.Registry
}

// RouteRequestsMetric is the per-route request counter's name.
const RouteRequestsMetric = "caddy_consul_route_requests_total"

// RouteRequestsCounter returns the per-route request counter, labelled
// service, <extraLabels...>, class, error (in that order), creating and registering it on first
// use. It lives in the global collector, so it survives config reloads with the same labels.
//
// A metric can have only one label set. When a (re)loaded config asks for a different one, the
// old counter is unregistered and replaced (its counts restart from zero), so changing
// metric_labels works with a plain config reload. Within one config, every consul_route_info
// with metrics enabled must use the same labels: the last one provisioned wins. Returns nil, nil
// when metrics are disabled.
func (m *MetricsCollector) RouteRequestsCounter(extraLabels []string) (*prometheus.CounterVec, error) {
	if m == nil {
		return nil, nil
	}
	labels := append(append([]string{"service"}, extraLabels...), "class", "error")

	m.routeRequestsMu.Lock()
	defer m.routeRequestsMu.Unlock()
	if m.routeRequests != nil {
		if slices.Equal(m.routeRequestsLabels, labels) {
			return m.routeRequests, nil
		}
		if m.logger != nil {
			m.logger.Warn("replacing route request counter: label set changed (counts restart)",
				zap.String("metric", RouteRequestsMetric),
				zap.Strings("old_labels", m.routeRequestsLabels),
				zap.Strings("new_labels", labels))
		}
		m.routeRequests, m.routeRequestsLabels, m.routeRequestsRegistry = nil, nil, nil
	}

	cv := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: RouteRequestsMetric,
		Help: "Requests matched to a Consul route, by service, configured tag labels, status class, and whether a handler (e.g. a WAF) ended the request with an error",
	}, labels)
	registry := prometheus.NewRegistry()
	if err := registry.Register(cv); err != nil {
		return nil, err
	}
	m.routeRequests, m.routeRequestsLabels, m.routeRequestsRegistry = cv, labels, registry
	return cv, nil
}

// gatherer returns everything ServeHTTP serves: the collector's registry plus the route request
// counter's (when one exists).
func (m *MetricsCollector) gatherer() prometheus.Gatherer {
	m.routeRequestsMu.Lock()
	defer m.routeRequestsMu.Unlock()
	if m.routeRequestsRegistry == nil {
		return m.registry
	}
	return prometheus.Gatherers{m.registry, m.routeRequestsRegistry}
}

var (
	globalMetrics   *MetricsCollector
	globalMetricsMu sync.RWMutex
)

// GetMetrics returns the global MetricsCollector, or nil if not initialized.
func GetMetrics() *MetricsCollector {
	globalMetricsMu.RLock()
	defer globalMetricsMu.RUnlock()
	return globalMetrics
}

// GetOrCreateGlobalMetrics returns the global MetricsCollector, creating it if
// needed. It is created once and reused across config reloads so counters keep
// accumulating and the Prometheus registry isn't re-registered.
func GetOrCreateGlobalMetrics(logger *zap.Logger) *MetricsCollector {
	globalMetricsMu.Lock()
	defer globalMetricsMu.Unlock()

	if globalMetrics != nil {
		return globalMetrics
	}
	globalMetrics = newMetricsCollector(logger)
	return globalMetrics
}

// newMetricsCollector builds a fresh collector with its own registry. Used by
// GetOrCreateGlobalMetrics and, for isolation, by tests.
func newMetricsCollector(logger *zap.Logger) *MetricsCollector {
	registry := prometheus.NewRegistry()

	m := &MetricsCollector{
		logger:   logger,
		registry: registry,

		ServicesTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "caddy_consul_services_total",
			Help: "Total number of Consul services being watched",
		}),
		RoutesTotal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "caddy_consul_routes_total",
			Help: "Total number of active routes by protocol",
		}, []string{"protocol"}),
		UpstreamsHealthy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "caddy_consul_upstreams_healthy",
			Help: "Number of healthy upstreams per service",
		}, []string{"service"}),
		UpstreamsTotal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "caddy_consul_upstreams_total",
			Help: "Total number of upstreams per service",
		}, []string{"service"}),
		ReconcileDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "caddy_consul_reconcile_duration_seconds",
			Help:    "Time taken to reconcile routes",
			Buckets: prometheus.DefBuckets,
		}),
		ReconcileErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "caddy_consul_reconcile_errors_total",
			Help: "Total number of reconciliation errors",
		}),
		WatcherErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "caddy_consul_watcher_errors_total",
			Help: "Total number of watcher errors",
		}),
		ConflictsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "caddy_consul_conflicts_total",
			Help: "Total number of route conflicts detected",
		}, []string{"type"}),
		DebounceEvents: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "caddy_consul_debounce_events_total",
			Help: "Total number of debounce flush events",
		}),

		TCPListenersActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "caddy_consul_tcp_listeners_active",
			Help: "Number of dynamic TCP listeners currently open",
		}),
		TCPListenerOpens: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "caddy_consul_tcp_listener_opens_total",
			Help: "Total dynamic TCP listeners opened",
		}),
		TCPListenerCloses: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "caddy_consul_tcp_listener_closes_total",
			Help: "Total dynamic TCP listeners closed",
		}),
		TCPListenErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "caddy_consul_tcp_listen_errors_total",
			Help: "Total failures to open a dynamic TCP listener (e.g. address in use)",
		}),
		TCPConnectionsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "caddy_consul_tcp_connections_total",
			Help: "Total accepted TCP connections",
		}),
		TCPConnectionsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "caddy_consul_tcp_connections_active",
			Help: "Currently active proxied TCP connections",
		}),
		TCPNoRoute: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "caddy_consul_tcp_no_route_total",
			Help: "TCP connections dropped because no route matched (incl. SNI mismatch)",
		}),
		TCPNoUpstream: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "caddy_consul_tcp_no_upstream_total",
			Help: "TCP connections dropped because the matched route had no healthy upstream",
		}),
		TCPUpstreamDialErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "caddy_consul_tcp_upstream_dial_errors_total",
			Help: "Failed upstream dials for TCP connections",
		}),
		TCPForceClosed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "caddy_consul_tcp_force_closed_total",
			Help: "TCP connections force-closed after the drain grace period on port removal",
		}),
	}

	registry.MustRegister(
		m.ServicesTotal,
		m.RoutesTotal,
		m.UpstreamsHealthy,
		m.UpstreamsTotal,
		m.ReconcileDuration,
		m.ReconcileErrors,
		m.WatcherErrors,
		m.ConflictsTotal,
		m.DebounceEvents,
		m.TCPListenersActive,
		m.TCPListenerOpens,
		m.TCPListenerCloses,
		m.TCPListenErrors,
		m.TCPConnectionsTotal,
		m.TCPConnectionsActive,
		m.TCPNoRoute,
		m.TCPNoUpstream,
		m.TCPUpstreamDialErrors,
		m.TCPForceClosed,
	)

	return m
}

// --- nil-safe recorders (no-op when the collector is nil / metrics disabled) ---

// Converge (onServicesChanged) recorders.
func (m *MetricsCollector) IncDebounce() {
	if m == nil {
		return
	}
	m.DebounceEvents.Inc()
}

func (m *MetricsCollector) ObserveReconcile(d time.Duration) {
	if m == nil {
		return
	}
	m.ReconcileDuration.Observe(d.Seconds())
}

func (m *MetricsCollector) IncReconcileError() {
	if m == nil {
		return
	}
	m.ReconcileErrors.Inc()
}

func (m *MetricsCollector) IncConflict(typ string) {
	if m == nil {
		return
	}
	m.ConflictsTotal.WithLabelValues(typ).Inc()
}

func (m *MetricsCollector) SetServicesTotal(n int) {
	if m == nil {
		return
	}
	m.ServicesTotal.Set(float64(n))
}

func (m *MetricsCollector) SetRoutesTotal(protocol string, n int) {
	if m == nil {
		return
	}
	m.RoutesTotal.WithLabelValues(protocol).Set(float64(n))
}

// TCP listener-manager recorders.
func (m *MetricsCollector) SetTCPListenersActive(n int) {
	if m == nil {
		return
	}
	m.TCPListenersActive.Set(float64(n))
}

func (m *MetricsCollector) IncTCPListenerOpen() {
	if m == nil {
		return
	}
	m.TCPListenerOpens.Inc()
}

func (m *MetricsCollector) IncTCPListenerClose() {
	if m == nil {
		return
	}
	m.TCPListenerCloses.Inc()
}

func (m *MetricsCollector) IncTCPListenError() {
	if m == nil {
		return
	}
	m.TCPListenErrors.Inc()
}

func (m *MetricsCollector) IncTCPConnAccepted() {
	if m == nil {
		return
	}
	m.TCPConnectionsTotal.Inc()
	m.TCPConnectionsActive.Inc()
}

func (m *MetricsCollector) DecTCPConnActive() {
	if m == nil {
		return
	}
	m.TCPConnectionsActive.Dec()
}

func (m *MetricsCollector) AddTCPForceClosed(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.TCPForceClosed.Add(float64(n))
}

// TCP proxy recorders.
func (m *MetricsCollector) IncTCPNoRoute() {
	if m == nil {
		return
	}
	m.TCPNoRoute.Inc()
}

func (m *MetricsCollector) IncTCPNoUpstream() {
	if m == nil {
		return
	}
	m.TCPNoUpstream.Inc()
}

func (m *MetricsCollector) IncTCPDialError() {
	if m == nil {
		return
	}
	m.TCPUpstreamDialErrors.Inc()
}

// ServeHTTP handles Prometheus metrics scraping.
func (m *MetricsCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	promhttp.HandlerFor(m.gatherer(), promhttp.HandlerOpts{}).ServeHTTP(w, r)
}
