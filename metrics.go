package caddyconsul

import (
	"net/http"
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
	promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
}
