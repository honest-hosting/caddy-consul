package caddyconsul

import (
	"fmt"

	consul "github.com/hashicorp/consul/api"
	"go.uber.org/zap"
)

// SidecarResolver resolves upstream addresses by querying Caddy's own sidecar
// proxy for local bind ports.
type SidecarResolver struct {
	client      *consul.Client
	logger      *zap.Logger
	serviceName string // Caddy's service name in the mesh
}

// NewSidecarResolver creates a new SidecarResolver.
func NewSidecarResolver(client *consul.Client, logger *zap.Logger, serviceName string) *SidecarResolver {
	return &SidecarResolver{
		client:      client,
		logger:      logger,
		serviceName: serviceName,
	}
}

// ResolveUpstreams replaces the route's upstreams with the sidecar proxy's local
// bind address for the target service. It queries Caddy's own sidecar at
// /v1/agent/service/<serviceName>-sidecar-proxy and matches the route's service
// name against Proxy.Upstreams[].DestinationName.
func (sr *SidecarResolver) ResolveUpstreams(route *RouteDefinition) error {
	proxyServiceID := sr.serviceName + "-sidecar-proxy"

	svc, _, err := sr.client.Agent().Service(proxyServiceID, nil)
	if err != nil {
		return fmt.Errorf("failed to query sidecar proxy %s: %w", proxyServiceID, err)
	}
	if svc == nil {
		return fmt.Errorf("sidecar proxy %s not found", proxyServiceID)
	}

	// Search for the target service in the proxy's upstream list
	if svc.Proxy != nil {
		for _, upstream := range svc.Proxy.Upstreams {
			if upstream.DestinationName == route.ServiceName {
				addr := upstream.LocalBindAddress
				if addr == "" {
					addr = "127.0.0.1"
				}
				port := upstream.LocalBindPort
				if port == 0 {
					return fmt.Errorf("sidecar upstream for %s has no bind port", route.ServiceName)
				}

				sr.logger.Debug("resolved sidecar upstream",
					zap.String("service", route.ServiceName),
					zap.String("sidecar", proxyServiceID),
					zap.String("bind_addr", fmt.Sprintf("%s:%d", addr, port)),
				)

				route.Upstreams = []Upstream{
					{
						Address: fmt.Sprintf("%s:%d", addr, port),
						Weight:  1,
						Healthy: true,
					},
				}
				return nil
			}
		}
	}

	return fmt.Errorf("no upstream entry for service %s in sidecar proxy %s (the sidecar's upstream list does not include this service)",
		route.ServiceName, proxyServiceID)
}
