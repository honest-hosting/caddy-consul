package caddyconsul

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestConcurrent_CompilerParallel verifies the compiler is safe under concurrent use.
// The compiler should be stateless — multiple goroutines compiling different route sets
// must not interfere with each other.
func TestConcurrent_CompilerParallel(t *testing.T) {
	rc := NewRouteCompiler(testLogger())

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			routes := []RouteDefinition{
				{
					ServiceName: fmt.Sprintf("svc-%d", n),
					Protocol:    ProtocolHTTP,
					Host:        fmt.Sprintf("svc-%d.example.com", n),
					Path:        "/",
					Upstreams:   []Upstream{{Address: fmt.Sprintf("10.0.0.%d:8080", n%256), Healthy: true}},
				},
			}
			result := rc.Compile(routes)
			assert.Len(t, result.HTTPRoutes, 1)
			assert.Empty(t, result.Conflicts)
		}(i)
	}
	wg.Wait()
}

// TestConcurrent_MetadataParserParallel verifies ParseServiceRoutes is safe under
// concurrent use with independent ServiceState inputs.
func TestConcurrent_MetadataParserParallel(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			svc := &ServiceState{
				Name: fmt.Sprintf("svc-%d", n),
				Meta: map[string]string{
					"caddy-host":     fmt.Sprintf("svc-%d.example.com", n),
					"caddy-protocol": "http",
				},
				Instances: []ServiceInstance{
					{Address: fmt.Sprintf("10.0.0.%d", n%256), Port: 8080, Healthy: true, Weight: 1, Tags: []string{"caddy-consul"}},
				},
			}
			routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
			assert.Len(t, routes, 1)
			assert.Equal(t, fmt.Sprintf("svc-%d.example.com", n), routes[0].Host)
		}(i)
	}
	wg.Wait()
}

// TestConcurrent_ReconcilerApplyTCP verifies that concurrent ApplyTCP() calls are
// serialized by the mutex and don't race on hash map or server name state.
func TestConcurrent_ReconcilerApplyTCP(t *testing.T) {
	ts := mockCaddyAdmin(nil)
	defer ts.Close()

	rec := NewReconciler(testLogger(), ts.Listener.Addr().String())

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			compiled := &CompiledConfig{
				TCPRoutes: []CompiledTCPRoute{
					{
						Port:        5000 + n,
						ServiceName: fmt.Sprintf("tcp-svc-%d", n),
						Upstreams:   []Upstream{{Address: fmt.Sprintf("10.0.0.%d:5432", n%256)}},
					},
				},
			}
			_ = rec.ApplyTCP(compiled)
		}(i)
	}
	wg.Wait()
}

// TestConcurrent_RouteTableUpdateMatch verifies that concurrent Update and Match
// calls on the RouteTable don't race.
func TestConcurrent_RouteTableUpdateMatch(t *testing.T) {
	rt := NewRouteTable()

	var wg sync.WaitGroup

	// Writers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				rt.Update([]CompiledHTTPRoute{
					{Host: fmt.Sprintf("svc-%d.example.com", n), Path: "/", ServiceName: fmt.Sprintf("svc-%d", n)},
				})
			}
		}(i)
	}

	// Readers
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = rt.Match("svc-0.example.com", "/")
			}
		}()
	}

	wg.Wait()
}

// TestConcurrent_ServiceStateClone verifies that cloning a ServiceState under
// a read lock while another goroutine modifies it under a write lock does not race.
// This mirrors the production pattern: watcher writes under w.mu, flushChanges
// clones under w.mu.RLock().
func TestConcurrent_ServiceStateClone(t *testing.T) {
	var mu sync.RWMutex

	svc := &ServiceState{
		Name: "test",
		Tags: []string{"a", "b"},
		Meta: map[string]string{"key": "val"},
		Instances: []ServiceInstance{
			{ID: "i1", Address: "10.0.0.1", Port: 8080, Healthy: true, Tags: []string{"t1"}, Meta: map[string]string{"m": "v"}},
		},
	}

	var wg sync.WaitGroup

	// Writer goroutine: mutates the original under write lock
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			mu.Lock()
			svc.Tags = []string{fmt.Sprintf("tag-%d", i)}
			svc.Meta = map[string]string{"iter": fmt.Sprintf("%d", i)}
			svc.Instances = []ServiceInstance{
				{ID: fmt.Sprintf("i-%d", i), Address: "10.0.0.1", Port: 8080 + i, Healthy: i%2 == 0},
			}
			mu.Unlock()
		}
	}()

	// Reader goroutines: clone under read lock (as flushChanges does)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				mu.RLock()
				c := svc.clone()
				mu.RUnlock()
				// After unlock, the clone is independent — verify it's usable
				_ = c.Name
				_ = len(c.Tags)
				_ = len(c.Instances)
			}
		}()
	}

	wg.Wait()
}

// TestConcurrent_WatcherDebounce verifies that rapid queueChanges calls don't
// race with flushChanges or Stop.
func TestConcurrent_WatcherDebounce(t *testing.T) {
	var mu sync.Mutex
	var callCount int

	onChange := func(changes []ServiceChange, services map[string]*ServiceState) {
		mu.Lock()
		callCount++
		mu.Unlock()
	}

	w := NewConsulWatcher(nil, testLogger(), HealthPolicyPassing, 50*time.Millisecond, 50*time.Millisecond, 5*time.Minute, DefaultServiceTag, DefaultConnectTag, onChange)

	// Rapidly queue changes from multiple goroutines
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				w.queueChanges([]ServiceChange{
					{Type: ChangeUpdated, Service: &ServiceState{Name: fmt.Sprintf("svc-%d", n)}},
				})
				time.Sleep(5 * time.Millisecond)
			}
		}(i)
	}

	wg.Wait()

	// Wait for final debounce to flush
	time.Sleep(200 * time.Millisecond)

	// Stop should be safe even after all this
	w.Stop()

	// Should have been called at least once
	mu.Lock()
	assert.Greater(t, callCount, 0, "onChange should have been called at least once")
	mu.Unlock()
}

// TestConcurrent_HashInterface verifies hashInterface is safe for concurrent use.
func TestConcurrent_HashInterface(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			val := map[string]interface{}{
				"handler": "reverse_proxy",
				"upstreams": []interface{}{
					map[string]interface{}{"dial": fmt.Sprintf("10.0.0.%d:8080", n)},
				},
			}
			h := hashInterface(val)
			assert.NotEmpty(t, h)
		}(i)
	}
	wg.Wait()
}

// BenchmarkCompile benchmarks the route compiler with varying route counts.
func BenchmarkCompile(b *testing.B) {
	for _, count := range []int{1, 10, 100, 500} {
		b.Run(fmt.Sprintf("routes_%d", count), func(b *testing.B) {
			rc := NewRouteCompiler(testLogger())
			routes := make([]RouteDefinition, count)
			for i := range routes {
				routes[i] = RouteDefinition{
					ServiceName: fmt.Sprintf("svc-%d", i),
					Protocol:    ProtocolHTTP,
					Host:        fmt.Sprintf("svc-%d.example.com", i),
					Path:        "/",
					Upstreams:   []Upstream{{Address: fmt.Sprintf("10.0.0.%d:8080", i%256), Healthy: true}},
				}
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rc.Compile(routes)
			}
		})
	}
}

// BenchmarkParseServiceRoutes benchmarks the metadata parser.
func BenchmarkParseServiceRoutes(b *testing.B) {
	logger := testLogger()

	b.Run("metadata", func(b *testing.B) {
		svc := &ServiceState{
			Name: "web",
			Meta: map[string]string{
				"caddy-host":     "app.example.com",
				"caddy-path":     "/api",
				"caddy-protocol": "http",
				"caddy-priority": "100",
				"caddy-weight":   "5",
			},
			Instances: []ServiceInstance{
				{Address: "10.0.0.1", Port: 8080, Healthy: true, Weight: 1, Tags: []string{"caddy-consul"}},
				{Address: "10.0.0.2", Port: 8080, Healthy: true, Weight: 2, Tags: []string{"caddy-consul"}},
			},
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", logger)
		}
	})

	b.Run("fabio_tags", func(b *testing.B) {
		svc := &ServiceState{
			Name: "legacy",
			Tags: []string{
				"urlprefix-app.example.com/",
				"urlprefix-app.example.com/api strip=/api",
			},
			Instances: []ServiceInstance{
				{Address: "10.0.0.1", Port: 8080, Healthy: true, Tags: []string{"urlprefix-app.example.com/", "urlprefix-app.example.com/api strip=/api"}},
			},
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", logger)
		}
	})

	b.Run("indexed_multi_route", func(b *testing.B) {
		svc := &ServiceState{
			Name: "multi",
			Meta: map[string]string{
				"caddy-route-0-protocol": "http",
				"caddy-route-0-host":     "web.example.com",
				"caddy-route-0-path":     "/",
				"caddy-route-1-protocol": "tcp",
				"caddy-route-1-port":     "5432",
				"caddy-route-2-protocol": "http",
				"caddy-route-2-host":     "api.example.com",
				"caddy-route-2-path":     "/v2",
			},
			Instances: []ServiceInstance{
				{Address: "10.0.0.1", Port: 8080, Healthy: true, Tags: []string{"caddy-consul"}},
			},
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", logger)
		}
	})
}

// BenchmarkBuildHTTPRouteJSON benchmarks HTTP route JSON generation.
func BenchmarkBuildHTTPRouteJSON(b *testing.B) {
	route := CompiledHTTPRoute{
		Host:        "app.example.com",
		Path:        "/api",
		ServiceName: "web",
		StripPrefix: true,
		Upstreams: []Upstream{
			{Address: "10.0.0.1:8080"},
			{Address: "10.0.0.2:8080"},
			{Address: "10.0.0.3:8080"},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = BuildHTTPRouteJSON(route)
	}
}

// BenchmarkHashInterface benchmarks the route fingerprinting used by the reconciler.
func BenchmarkHashInterface(b *testing.B) {
	route := map[string]interface{}{
		"match": []interface{}{
			map[string]interface{}{
				"host": []interface{}{"app.example.com"},
				"path": []interface{}{"/api*"},
			},
		},
		"handle": []interface{}{
			map[string]interface{}{
				"handler":   "reverse_proxy",
				"upstreams": []interface{}{map[string]interface{}{"dial": "10.0.0.1:8080"}},
			},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hashInterface(route)
	}
}

// BenchmarkServiceStateClone benchmarks deep-copying a ServiceState.
func BenchmarkServiceStateClone(b *testing.B) {
	svc := &ServiceState{
		Name: "web",
		Tags: []string{"urlprefix-app.example.com/", "env:prod", "version:1.0"},
		Meta: map[string]string{
			"caddy-host":     "app.example.com",
			"caddy-protocol": "http",
			"caddy-path":     "/",
		},
		Instances: []ServiceInstance{
			{ID: "i1", Address: "10.0.0.1", Port: 8080, Healthy: true, Weight: 1, Tags: []string{"az:us-east-1a"}, Meta: map[string]string{"version": "1.0"}},
			{ID: "i2", Address: "10.0.0.2", Port: 8080, Healthy: true, Weight: 2, Tags: []string{"az:us-east-1b"}, Meta: map[string]string{"version": "1.1"}},
			{ID: "i3", Address: "10.0.0.3", Port: 8080, Healthy: false, Weight: 1, Tags: []string{"az:us-east-1c"}, Meta: map[string]string{"version": "1.0"}},
		},
		LastIndex: 42,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		svc.clone()
	}
}

// BenchmarkRouteTableMatch benchmarks route table matching performance.
func BenchmarkRouteTableMatch(b *testing.B) {
	rt := NewRouteTable()
	routes := make([]CompiledHTTPRoute, 100)
	for i := range routes {
		routes[i] = CompiledHTTPRoute{
			Host:        fmt.Sprintf("svc-%d.example.com", i),
			Path:        "/",
			ServiceName: fmt.Sprintf("svc-%d", i),
			Upstreams:   []Upstream{{Address: fmt.Sprintf("10.0.0.%d:8080", i%256), Healthy: true}},
		}
	}
	rt.Update(routes)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rt.Match("svc-50.example.com", "/api/test")
	}
}
