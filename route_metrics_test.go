package caddyconsul

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestRouteRequestsCounter(t *testing.T) {
	var nilCollector *MetricsCollector
	cv, err := nilCollector.RouteRequestsCounter([]string{"tenant"})
	require.NoError(t, err)
	assert.Nil(t, cv, "disabled metrics: no counter")

	m := newMetricsCollector(zap.NewNop())
	first, err := m.RouteRequestsCounter([]string{"site", "tenant"})
	require.NoError(t, err)
	again, err := m.RouteRequestsCounter([]string{"site", "tenant"})
	require.NoError(t, err)
	assert.Same(t, first, again, "same label set reuses the counter (config reloads)")

	_, err = m.RouteRequestsCounter([]string{"tenant"})
	assert.Error(t, err, "a different label set can't share the metric")
}

// metricsHandler returns a consul_route_info handler with metrics on, as Provision would set it up.
func metricsHandler(t *testing.T) (*ConsulRouteInfoHandler, *MetricsCollector) {
	t.Helper()
	rt := NewRouteTable()
	rt.Update([]CompiledHTTPRoute{{
		Host: "blog.example.com", Path: "/", ServiceName: "site-1",
		Upstreams: []Upstream{{Address: "10.0.0.1:8080", Healthy: true}},
		TagValues: map[string]string{"X-Tenant": "acme", "X-Site": "blog"},
	}})
	m := newMetricsCollector(zap.NewNop())
	labels := []string{"site", "tenant"}
	cv, err := m.RouteRequestsCounter(labels)
	require.NoError(t, err)
	return &ConsulRouteInfoHandler{
		Metrics:      true,
		MetricLabels: map[string]string{"tenant": "X-Tenant", "site": "X-Site"},
		routeTable:   rt,
		counter:      cv,
		labelNames:   labels,
		labelTagKeys: []string{"X-Site", "X-Tenant"},
	}, m
}

func TestConsulRouteInfo_Metrics(t *testing.T) {
	h, _ := metricsHandler(t)
	count := func(class, errored string) float64 {
		return testutil.ToFloat64(h.counter.WithLabelValues("site-1", "blog", "acme", class, errored))
	}
	serve := func(host string, next caddyhttp.HandlerFunc) error {
		r := withVars(httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil), map[string]any{})
		return h.ServeHTTP(httptest.NewRecorder(), r, next)
	}

	// Upstream answered 404
	require.NoError(t, serve("blog.example.com", func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(http.StatusNotFound)
		return nil
	}))
	assert.Equal(t, 1.0, count("4xx", "false"))

	// A handler (e.g. the WAF) ended the request with a 403 error: counted as a block, error returned
	blocked := caddyhttp.Error(http.StatusForbidden, errors.New("interruption"))
	err := serve("blog.example.com", func(http.ResponseWriter, *http.Request) error { return blocked })
	assert.ErrorIs(t, err, blocked)
	assert.Equal(t, 1.0, count("4xx", "true"))

	// Non-HandlerError errors count as 5xx; a body without WriteHeader, or nothing at all, as 2xx
	_ = serve("blog.example.com", func(http.ResponseWriter, *http.Request) error { return errors.New("boom") })
	assert.Equal(t, 1.0, count("5xx", "true"))
	require.NoError(t, serve("blog.example.com", func(w http.ResponseWriter, _ *http.Request) error {
		_, err := w.Write([]byte("ok"))
		return err
	}))
	require.NoError(t, serve("blog.example.com", func(http.ResponseWriter, *http.Request) error { return nil }))
	assert.Equal(t, 2.0, count("2xx", "false"))

	// 1xx informational doesn't count as the final status
	require.NoError(t, serve("blog.example.com", func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusMovedPermanently)
		return nil
	}))
	assert.Equal(t, 1.0, count("3xx", "false"))

	// No route matched: not counted at all
	require.NoError(t, serve("other.example.com", func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(http.StatusOK)
		return nil
	}))
	// 4xx/false, 4xx/true, 5xx/true, 2xx/false (two requests), 3xx/false; the unmatched host adds none
	assert.Equal(t, 5, testutil.CollectAndCount(h.counter), "one series per distinct label set above")
}

// The recorder must keep http.ResponseController features working (SSE flushing via Unwrap).
func TestStatusRecorder_FlushPassthrough(t *testing.T) {
	under := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriterWrapper: &caddyhttp.ResponseWriterWrapper{ResponseWriter: under}}
	rec.WriteHeader(http.StatusOK)
	require.NoError(t, http.NewResponseController(rec).Flush())
	assert.True(t, under.Flushed)
	assert.Equal(t, http.StatusOK, rec.status)
}

func TestConsulRouteInfo_UnmarshalCaddyfile_Metrics(t *testing.T) {
	var h ConsulRouteInfoHandler
	require.NoError(t, h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`consul_route_info {
		var app.tenant X-Tenant
		metrics
		metric_label tenant X-Tenant
		metric_label site X-Site
	}`)))
	assert.True(t, h.Metrics)
	assert.Equal(t, map[string]string{"tenant": "X-Tenant", "site": "X-Site"}, h.MetricLabels)

	for _, bad := range []string{
		"consul_route_info {\n\tmetrics extra\n}",
		"consul_route_info {\n\tmetric_label onlyname\n}",
	} {
		var h ConsulRouteInfoHandler
		assert.Error(t, h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(bad)), bad)
	}
}

func TestConsulMetricsHandler(t *testing.T) {
	globalMetricsMu.Lock()
	prev := globalMetrics
	globalMetrics = nil
	globalMetricsMu.Unlock()
	t.Cleanup(func() {
		globalMetricsMu.Lock()
		globalMetrics = prev
		globalMetricsMu.Unlock()
	})
	next := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		t.Fatal("consul_metrics must not call next")
		return nil
	})

	w := httptest.NewRecorder()
	require.NoError(t, ConsulMetricsHandler{}.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics/consul", nil), next))
	assert.Equal(t, http.StatusNotFound, w.Code, "metrics disabled")

	m := GetOrCreateGlobalMetrics(zap.NewNop())
	cv, err := m.RouteRequestsCounter(nil)
	require.NoError(t, err)
	cv.WithLabelValues("site-1", "2xx", "false").Inc()

	w = httptest.NewRecorder()
	require.NoError(t, ConsulMetricsHandler{}.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics/consul", nil), next))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, strings.Contains(w.Body.String(), "caddy_consul_services_total"))
	assert.True(t, strings.Contains(w.Body.String(), RouteRequestsMetric+`{class="2xx",error="false",service="site-1"} 1`))

	err = ConsulMetricsHandler{}.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/metrics/consul", nil), next)
	var he caddyhttp.HandlerError
	require.ErrorAs(t, err, &he)
	assert.Equal(t, http.StatusMethodNotAllowed, he.StatusCode)
}
