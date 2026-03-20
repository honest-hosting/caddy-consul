package caddyconsul

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// MetricsCollector handles all metrics collection for the caddy-consul plugin.
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

// GetOrCreateGlobalMetrics returns the global MetricsCollector, creating it if needed.
func GetOrCreateGlobalMetrics(logger *zap.Logger) *MetricsCollector {
	globalMetricsMu.Lock()
	defer globalMetricsMu.Unlock()

	if globalMetrics != nil {
		return globalMetrics
	}

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
	)

	globalMetrics = m
	return m
}

// ServeHTTP handles Prometheus metrics scraping.
func (m *MetricsCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
}
