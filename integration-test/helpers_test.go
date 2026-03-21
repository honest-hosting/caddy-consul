package integration_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	consul "github.com/hashicorp/consul/api"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

const (
	consulAddr = "127.0.0.1:8500"
	caddyHTTP  = "127.0.0.1:8080"
	caddyHTTPS = "127.0.0.1:8443"
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
func registerConnectService(client *consul.Client, name, address string, port int, meta map[string]string) error {
	reg := &consul.AgentServiceRegistration{
		ID:      name,
		Name:    name,
		Address: address,
		Port:    port,
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
func registerTCPServiceMeta(client *consul.Client, name, address string, servicePort, listenPort int) error {
	return registerService(client, name, address, servicePort,
		nil,
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

// --- Caddy config inspection helpers ---

// getCaddyHTTPRoutes returns routes from ALL HTTP servers in Caddy's config.
func getCaddyHTTPRoutes() ([]map[string]interface{}, error) {
	config, err := getCaddyConfig()
	if err != nil {
		return nil, err
	}

	apps, _ := config["apps"].(map[string]interface{})
	if apps == nil {
		return nil, fmt.Errorf("no apps in config")
	}
	httpApp, _ := apps["http"].(map[string]interface{})
	if httpApp == nil {
		return nil, fmt.Errorf("no http app in config")
	}
	servers, _ := httpApp["servers"].(map[string]interface{})
	if servers == nil {
		return nil, fmt.Errorf("no servers in http app")
	}

	// Collect routes from ALL servers
	var allRoutes []map[string]interface{}
	for _, srv := range servers {
		srvMap, _ := srv.(map[string]interface{})
		if srvMap == nil {
			continue
		}
		routesRaw, _ := srvMap["routes"].([]interface{})
		for _, r := range routesRaw {
			if rm, ok := r.(map[string]interface{}); ok {
				allRoutes = append(allRoutes, rm)
			}
		}
	}

	if len(allRoutes) == 0 {
		return nil, fmt.Errorf("no routes found in any HTTP server")
	}

	return allRoutes, nil
}

// findHTTPRouteByHost searches Caddy's HTTP routes for one matching the given host.
// Returns the route map and true if found.
func findHTTPRouteByHost(host string) (map[string]interface{}, bool) {
	routes, err := getCaddyHTTPRoutes()
	if err != nil {
		return nil, false
	}

	for _, route := range routes {
		matchList, _ := route["match"].([]interface{})
		for _, m := range matchList {
			match, _ := m.(map[string]interface{})
			hosts, _ := match["host"].([]interface{})
			for _, h := range hosts {
				if h == host {
					return route, true
				}
			}
		}
	}
	return nil, false
}

// getReverseProxyHandler extracts the reverse_proxy handler from a route.
func getReverseProxyHandler(route map[string]interface{}) (map[string]interface{}, bool) {
	handlers, _ := route["handle"].([]interface{})
	for _, h := range handlers {
		hm, _ := h.(map[string]interface{})
		if hm["handler"] == "reverse_proxy" {
			return hm, true
		}
	}
	return nil, false
}

// getStaticResponseHandler extracts the static_response handler from a route.
func getStaticResponseHandler(route map[string]interface{}) (map[string]interface{}, bool) {
	handlers, _ := route["handle"].([]interface{})
	for _, h := range handlers {
		hm, _ := h.(map[string]interface{})
		if hm["handler"] == "static_response" {
			return hm, true
		}
	}
	return nil, false
}

// getStaticResponseLocation extracts the Location header from a static_response handler.
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

// reverseProxyHasTLSTransport checks if a reverse_proxy handler has a TLS transport configured.
func reverseProxyHasTLSTransport(handler map[string]interface{}) bool {
	transport, _ := handler["transport"].(map[string]interface{})
	if transport == nil {
		return false
	}
	tls, _ := transport["tls"].(map[string]interface{})
	return tls != nil
}

// getReverseProxyUpstreams extracts the upstream dial addresses from a reverse_proxy handler.
func getReverseProxyUpstreams(handler map[string]interface{}) []string {
	upstreamsRaw, _ := handler["upstreams"].([]interface{})
	var addrs []string
	for _, u := range upstreamsRaw {
		um, _ := u.(map[string]interface{})
		if dial, ok := um["dial"].(string); ok {
			addrs = append(addrs, dial)
		}
	}
	return addrs
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
