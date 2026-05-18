package integration_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	consul "github.com/hashicorp/consul/api"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

const (
	consulAddr = "127.0.0.1:8500"
	caddyHTTP  = "127.0.0.1:9090"
	caddyHTTPS = "127.0.0.1:9443"
	caddyAdmin = "127.0.0.1:2019"

	// testDomain is the default *.localdev hostname for TLS tests.
	testDomain = "caddy.localdev"

	// connectServiceName matches the Caddyfile's connect_service_name.
	connectServiceName = "caddy-test-ingress"
)

// --- HTTP clients ---

func tlsConf(serverName string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test only
		ServerName:         serverName,
	}
}

func dialDirect(addr string) func(ctx context.Context, network, _ string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
}

func http11Client(sniHost string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsConf(sniHost),
			TLSNextProto:    make(map[string]func(string, *tls.Conn) http.RoundTripper),
			DialContext:     dialDirect(caddyHTTPS),
		},
	}
}

func http2Client(sniHost string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   tlsConf(sniHost),
			ForceAttemptHTTP2: true,
			DialContext:       dialDirect(caddyHTTPS),
		},
	}
}

func http3Client(sniHost string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http3.Transport{
			TLSClientConfig: tlsConf(sniHost),
			Dial: func(ctx context.Context, _ string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
				udpAddr, err := net.ResolveUDPAddr("udp", caddyHTTPS)
				if err != nil {
					return nil, err
				}
				udpConn, err := net.ListenUDP("udp", nil)
				if err != nil {
					return nil, err
				}
				return quic.Dial(ctx, udpConn, udpAddr, tlsCfg, cfg)
			},
		},
	}
}

func plainHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: dialDirect(caddyHTTP),
		},
	}
}

// --- Consul helpers ---

func newConsulClient() (*consul.Client, error) {
	cfg := consul.DefaultConfig()
	cfg.Address = consulAddr
	return consul.NewClient(cfg)
}

func registerService(client *consul.Client, name, address string, port int, tags []string, meta map[string]string) error {
	reg := &consul.AgentServiceRegistration{
		ID:      name,
		Name:    name,
		Address: address,
		Port:    port,
		Tags:    tags,
		Meta:    meta,
		Check: &consul.AgentServiceCheck{
			TCP:      fmt.Sprintf("%s:%d", address, port),
			Interval: "1s",
			Timeout:  "1s",
		},
	}
	return client.Agent().ServiceRegister(reg)
}

func deregisterService(client *consul.Client, name string) error {
	return client.Agent().ServiceDeregister(name)
}

// registerConnectService registers a service with Connect sidecar enabled.
// If metadata with caddy-* keys is provided, the "caddy-consul-connect" sentinel
// tag is added automatically for catalog-level discovery with mesh routing.
func registerConnectService(client *consul.Client, name, address string, port int, meta map[string]string) error {
	var tags []string
	for k := range meta {
		if strings.HasPrefix(k, "caddy-") {
			tags = []string{"caddy-consul-connect"}
			break
		}
	}
	reg := &consul.AgentServiceRegistration{
		ID:      name,
		Name:    name,
		Address: address,
		Port:    port,
		Tags:    tags,
		Meta:    meta,
		Connect: &consul.AgentServiceConnect{
			SidecarService: &consul.AgentServiceRegistration{},
		},
		Check: &consul.AgentServiceCheck{
			TCP:      fmt.Sprintf("%s:%d", address, port),
			Interval: "1s",
			Timeout:  "1s",
		},
	}
	return client.Agent().ServiceRegister(reg)
}

// registerCaddySidecarWithUpstreams registers Caddy's own service with a sidecar
// proxy that has specific upstream definitions.
func registerCaddySidecarWithUpstreams(client *consul.Client, upstreams []consul.Upstream) error {
	reg := &consul.AgentServiceRegistration{
		ID:   connectServiceName,
		Name: connectServiceName,
		Port: 443,
		Connect: &consul.AgentServiceConnect{
			SidecarService: &consul.AgentServiceRegistration{
				Proxy: &consul.AgentServiceConnectProxyConfig{
					Upstreams: upstreams,
				},
			},
		},
		Check: &consul.AgentServiceCheck{
			TTL:    "30s",
			Status: consul.HealthPassing,
		},
	}
	return client.Agent().ServiceRegister(reg)
}

// setIntention creates or updates a Connect intention.
func setIntention(client *consul.Client, source, destination, action string) error {
	_, err := client.Connect().IntentionUpsert(&consul.Intention{
		SourceName:      source,
		DestinationName: destination,
		Action:          consul.IntentionAction(action),
	}, nil)
	return err
}

// deleteIntention removes a Connect intention.
func deleteIntention(client *consul.Client, source, destination string) error {
	_, err := client.Connect().IntentionDeleteExact(source, destination, nil)
	return err
}

// --- TCP helpers ---

const (
	caddyTCPPostgres = "127.0.0.1:15432"
	caddyTCPMySQL    = "127.0.0.1:13306"
)

// dialTCP connects to a TCP address, sends data, reads the response, and returns it.
func dialTCP(addr string, send string, timeout time.Duration) (string, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(timeout))

	if send != "" {
		_, err = conn.Write([]byte(send))
		if err != nil {
			return "", fmt.Errorf("write failed: %w", err)
		}
	}

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil && err.Error() != "EOF" {
		return "", fmt.Errorf("read failed: %w", err)
	}
	return string(buf[:n]), nil
}

// waitForTCP polls a TCP address until a connection succeeds.
func waitForTCP(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("TCP %s not reachable within %s", addr, timeout)
}

// registerTCPService registers a TCP service in Consul with the urlprefix- tag.
func registerTCPService(client *consul.Client, name, address string, servicePort, listenPort int) error {
	return registerService(client, name, address, servicePort,
		[]string{fmt.Sprintf("urlprefix-:%d proto=tcp", listenPort)},
		nil,
	)
}

// registerTCPServiceMeta registers a TCP service using caddy-* metadata.
// Includes the "caddy-consul" sentinel tag required for catalog-level discovery.
func registerTCPServiceMeta(client *consul.Client, name, address string, servicePort, listenPort int) error {
	return registerService(client, name, address, servicePort,
		[]string{"caddy-consul"},
		map[string]string{
			"caddy-protocol": "tcp",
			"caddy-port":     fmt.Sprintf("%d", listenPort),
		},
	)
}

// --- Response helpers ---

func readBody(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func waitForEndpoint(client *http.Client, url string, timeout time.Duration) (*http.Response, error) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode < 500 {
			return resp, nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}

		time.Sleep(500 * time.Millisecond)
	}

	return nil, fmt.Errorf("endpoint %s did not become available within %s", url, timeout)
}

func getCaddyConfig() (map[string]interface{}, error) {
	url := fmt.Sprintf("http://%s/config/", caddyAdmin)
	resp, err := http.Get(url) //nolint:noctx // test helper
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var config map[string]interface{}
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, err
	}

	return config, nil
}

// --- Consul route table inspection helpers ---

// consulRouteEntry represents a route from the /consul/routes admin endpoint.
type consulRouteEntry struct {
	Host            string `json:"Host"`
	Path            string `json:"Path"`
	ServiceName     string `json:"ServiceName"`
	StripPrefix     bool   `json:"StripPrefix"`
	Via             string `json:"Via"`
	RedirectCode    int    `json:"RedirectCode"`
	RedirectURL     string `json:"RedirectURL"`
	RedirectNoCache bool   `json:"RedirectNoCache"`
	NoCacheOptOut   bool   `json:"NoCacheOptOut"`
	Upstreams       []struct {
		Address string `json:"Address"`
		Weight  int    `json:"Weight"`
		Healthy bool   `json:"Healthy"`
	} `json:"Upstreams"`
}

// getConsulRoutes fetches the in-memory route table from the /consul/routes admin endpoint.
func getConsulRoutes() ([]consulRouteEntry, error) {
	url := fmt.Sprintf("http://%s/consul/routes", caddyAdmin)
	resp, err := http.Get(url) //nolint:noctx // test helper
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var routes []consulRouteEntry
	if err := json.Unmarshal(body, &routes); err != nil {
		return nil, err
	}
	return routes, nil
}

// findHTTPRouteByHost searches the in-memory consul route table for a route matching the given host.
// Returns the route as a map (for compatibility with existing tests) and true if found.
func findHTTPRouteByHost(host string) (map[string]interface{}, bool) {
	routes, err := getConsulRoutes()
	if err != nil {
		return nil, false
	}

	for _, r := range routes {
		if r.Host == host {
			// Convert to map for backward compatibility with existing test helpers
			result := map[string]interface{}{
				"Host":         r.Host,
				"Path":         r.Path,
				"ServiceName":  r.ServiceName,
				"Via":          r.Via,
				"RedirectCode": float64(r.RedirectCode),
				"RedirectURL":  r.RedirectURL,
			}
			if len(r.Upstreams) > 0 {
				var upstreams []interface{}
				for _, u := range r.Upstreams {
					upstreams = append(upstreams, map[string]interface{}{
						"Address": u.Address,
						"Healthy": u.Healthy,
					})
				}
				result["Upstreams"] = upstreams
			}
			return result, true
		}
	}
	return nil, false
}

// isProxyRoute checks if a route from the consul route table is a proxy route (not a redirect).
func isProxyRoute(route map[string]interface{}) bool {
	code, _ := route["RedirectCode"].(float64)
	return code == 0
}

// isRedirectRoute checks if a route from the consul route table is a redirect route.
func isRedirectRoute(route map[string]interface{}) bool {
	code, _ := route["RedirectCode"].(float64)
	return code > 0
}

// getRouteRedirectCode returns the redirect status code from a consul route.
func getRouteRedirectCode(route map[string]interface{}) int {
	code, _ := route["RedirectCode"].(float64)
	return int(code)
}

// getRouteRedirectURL returns the redirect URL from a consul route.
func getRouteRedirectURL(route map[string]interface{}) string {
	url, _ := route["RedirectURL"].(string)
	return url
}

// getRouteUpstreams extracts upstream addresses from a consul route.
func getRouteUpstreams(route map[string]interface{}) []string {
	upstreamsRaw, _ := route["Upstreams"].([]interface{})
	var addrs []string
	for _, u := range upstreamsRaw {
		um, _ := u.(map[string]interface{})
		if addr, ok := um["Address"].(string); ok {
			addrs = append(addrs, addr)
		}
	}
	return addrs
}

// Backward-compatible aliases for tests that use the old helper names.
func getReverseProxyHandler(route map[string]interface{}) (map[string]interface{}, bool) {
	if isProxyRoute(route) && len(getRouteUpstreams(route)) > 0 {
		return route, true
	}
	return nil, false
}

func getStaticResponseHandler(route map[string]interface{}) (map[string]interface{}, bool) {
	if isRedirectRoute(route) {
		// Return a map that mimics the old static_response handler format
		result := map[string]interface{}{
			"handler":     "static_response",
			"status_code": fmt.Sprintf("%d", getRouteRedirectCode(route)),
			"headers": map[string]interface{}{
				"Location": []interface{}{getRouteRedirectURL(route)},
			},
		}
		return result, true
	}
	return nil, false
}

func getStaticResponseLocation(handler map[string]interface{}) string {
	headers, _ := handler["headers"].(map[string]interface{})
	if headers == nil {
		return ""
	}
	locations, _ := headers["Location"].([]interface{})
	if len(locations) == 0 {
		return ""
	}
	loc, _ := locations[0].(string)
	return loc
}

func reverseProxyHasTLSTransport(_ map[string]interface{}) bool {
	return false // in-memory routes don't have TLS transport config
}

func getReverseProxyUpstreams(route map[string]interface{}) []string {
	return getRouteUpstreams(route)
}

// getRouteVia returns the Via field from a consul route.
func getRouteVia(route map[string]interface{}) string {
	via, _ := route["Via"].(string)
	return via
}

// waitForHTTPRoute polls Caddy config until a route matching the host appears.
func waitForHTTPRoute(host string, timeout time.Duration) (map[string]interface{}, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		route, found := findHTTPRouteByHost(host)
		if found {
			return route, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("HTTP route for host %s not found in Caddy config within %s", host, timeout)
}

// waitForHTTPRouteWithVia polls until a route with the expected Via value appears.
func waitForHTTPRouteWithVia(host, expectedVia string, timeout time.Duration) (map[string]interface{}, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		route, found := findHTTPRouteByHost(host)
		if found && getRouteVia(route) == expectedVia {
			return route, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("HTTP route for host %s with Via=%q not found within %s", host, expectedVia, timeout)
}

// waitForViaHeader polls an HTTP endpoint until the X-Caddy-Consul-Via response header matches.
func waitForViaHeader(client *http.Client, url, expectedVia string, timeout time.Duration) (*http.Response, error) {
	deadline := time.Now().Add(timeout)
	var lastStatus int
	var lastHeaders http.Header
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		lastStatus = resp.StatusCode
		lastHeaders = resp.Header.Clone()
		lastErr = nil
		if resp.Header.Get("X-Caddy-Consul-Via") == expectedVia {
			return resp, nil
		}
		_ = resp.Body.Close()
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		return nil, fmt.Errorf("X-Caddy-Consul-Via=%q not found at %s within %s (last error: %v)", expectedVia, url, timeout, lastErr)
	}
	return nil, fmt.Errorf("X-Caddy-Consul-Via=%q not found at %s within %s (last status: %d, last headers: %v)", expectedVia, url, timeout, lastStatus, lastHeaders)
}

// waitForHTTPRouteGone polls Caddy config until a route matching the host disappears.
func waitForHTTPRouteGone(host string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, found := findHTTPRouteByHost(host)
		if !found {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("HTTP route for host %s still present in Caddy config after %s", host, timeout)
}

// getCaddyTCPServer returns a specific L4 TCP server from Caddy's config.
func getCaddyTCPServer(serverName string) (map[string]interface{}, error) {
	url := fmt.Sprintf("http://%s/config/apps/layer4/servers/%s", caddyAdmin, serverName)
	resp, err := http.Get(url) //nolint:noctx // test helper
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d", url, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var server map[string]interface{}
	if err := json.Unmarshal(body, &server); err != nil {
		return nil, err
	}
	if server == nil {
		return nil, fmt.Errorf("GET %s returned null", url)
	}
	return server, nil
}

// getL4ProxyUpstreamDial extracts the first dial address from an L4 proxy upstream.
// In caddy-l4, the "dial" field is an array of strings (e.g. ["10.0.0.1:8080"]).
func getL4ProxyUpstreamDial(upstream map[string]interface{}) string {
	switch d := upstream["dial"].(type) {
	case string:
		return d
	case []interface{}:
		if len(d) > 0 {
			if s, ok := d[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

// waitForCaddyTCPServerGone polls Caddy config until an L4 server disappears.
func waitForCaddyTCPServerGone(serverName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := getCaddyTCPServer(serverName)
		if err != nil {
			return nil // server is gone
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("L4 server %s still present in Caddy config after %s", serverName, timeout)
}

// getConsulCheck returns a specific health check by ID from the local agent.
func getConsulCheck(client *consul.Client, checkID string) (*consul.AgentCheck, error) {
	checks, err := client.Agent().Checks()
	if err != nil {
		return nil, err
	}
	check, ok := checks[checkID]
	if !ok {
		return nil, fmt.Errorf("check %s not found", checkID)
	}
	return check, nil
}

// waitForConsulCheck polls until a check with the given ID exists and has the expected status.
func waitForConsulCheck(client *consul.Client, checkID, status string, timeout time.Duration) (*consul.AgentCheck, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		check, err := getConsulCheck(client, checkID)
		if err == nil && check.Status == status {
			return check, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("check %s not in status %s within %s", checkID, status, timeout)
}

// getConsulServiceProxy returns the Connect proxy config for a sidecar service.
func getConsulServiceProxy(client *consul.Client, sidecarID string) (*consul.AgentServiceConnectProxyConfig, error) {
	svc, _, err := client.Agent().Service(sidecarID, nil)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, fmt.Errorf("service %s not found", sidecarID)
	}
	if svc.Proxy == nil {
		return nil, fmt.Errorf("service %s has no proxy config", sidecarID)
	}
	return svc.Proxy, nil
}

// waitForUpstreamInSidecar polls until the sidecar proxy has an upstream with the given destination name.
func waitForUpstreamInSidecar(client *consul.Client, sidecarID, destName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		proxy, err := getConsulServiceProxy(client, sidecarID)
		if err == nil {
			for _, u := range proxy.Upstreams {
				if u.DestinationName == destName {
					return nil
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("upstream %s not found in sidecar %s within %s", destName, sidecarID, timeout)
}

// waitForUpstreamGoneFromSidecar polls until the sidecar proxy no longer has an upstream with the given destination name.
func waitForUpstreamGoneFromSidecar(client *consul.Client, sidecarID, destName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		proxy, err := getConsulServiceProxy(client, sidecarID)
		if err != nil {
			return nil // sidecar gone entirely
		}
		found := false
		for _, u := range proxy.Upstreams {
			if u.DestinationName == destName {
				found = true
				break
			}
		}
		if !found {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("upstream %s still in sidecar %s after %s", destName, sidecarID, timeout)
}

// waitForConsulService polls Consul until a service is registered and has healthy instances.
func waitForConsulService(client *consul.Client, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, _, err := client.Health().Service(name, "", true, nil)
		if err == nil && len(entries) > 0 {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("service %s not healthy within %s", name, timeout)
}
