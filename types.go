package caddyconsul

// Protocol represents the routing protocol type.
type Protocol string

const (
	ProtocolHTTP           Protocol = "http"
	ProtocolHTTPS          Protocol = "https"
	ProtocolTCP            Protocol = "tcp"
	ProtocolTLSPassthrough Protocol = "tls-passthrough"
)

// UpstreamMode determines how traffic reaches the backend.
type UpstreamMode string

const (
	UpstreamDirect         UpstreamMode = "direct"          // no mesh, connect directly
	UpstreamConnectSidecar UpstreamMode = "connect-sidecar" // via local sidecar proxy
)

// IsConnect returns true if the mode involves Consul Connect.
func (m UpstreamMode) IsConnect() bool {
	return m == UpstreamConnectSidecar
}

// HealthPolicy controls which instances are considered routable.
type HealthPolicy int

const (
	HealthPolicyPassing HealthPolicy = iota
	HealthPolicyWarning
	HealthPolicyAny
)

// ChangeType represents the type of service change.
type ChangeType string

const (
	ChangeAdded   ChangeType = "added"
	ChangeUpdated ChangeType = "updated"
	ChangeRemoved ChangeType = "removed"
)

// ConflictType represents the type of route conflict.
type ConflictType string

const (
	ConflictDuplicateHostPath ConflictType = "duplicate_host_path"
	ConflictDuplicatePortSNI  ConflictType = "duplicate_port_sni"
	ConflictConflictingTLS    ConflictType = "conflicting_tls"
	ConflictStaticWins        ConflictType = "static_wins"
)

// ServiceState holds the current known state of a Consul service.
type ServiceState struct {
	Name      string
	Tags      []string
	Meta      map[string]string
	Instances []ServiceInstance
	LastIndex uint64
}

// clone returns a deep copy of the ServiceState, safe for concurrent use.
func (s *ServiceState) clone() *ServiceState {
	c := &ServiceState{
		Name:      s.Name,
		LastIndex: s.LastIndex,
	}
	if s.Tags != nil {
		c.Tags = make([]string, len(s.Tags))
		copy(c.Tags, s.Tags)
	}
	if s.Meta != nil {
		c.Meta = make(map[string]string, len(s.Meta))
		for k, v := range s.Meta {
			c.Meta[k] = v
		}
	}
	if s.Instances != nil {
		c.Instances = make([]ServiceInstance, len(s.Instances))
		for i, inst := range s.Instances {
			c.Instances[i] = inst.clone()
		}
	}
	return c
}

// ServiceInstance represents a single instance of a Consul service.
type ServiceInstance struct {
	ID       string
	Address  string
	Port     int
	Tags     []string
	Meta     map[string]string
	Healthy  bool
	Weight   int
	NodeName string // Consul node name where this instance runs
}

// clone returns a deep copy of the ServiceInstance.
func (si ServiceInstance) clone() ServiceInstance {
	c := si
	if si.Tags != nil {
		c.Tags = make([]string, len(si.Tags))
		copy(c.Tags, si.Tags)
	}
	if si.Meta != nil {
		c.Meta = make(map[string]string, len(si.Meta))
		for k, v := range si.Meta {
			c.Meta[k] = v
		}
	}
	return c
}

// ServiceChange represents a change to a Consul service.
type ServiceChange struct {
	Type    ChangeType
	Service *ServiceState
}

// RouteDefinition holds parsed routing instructions from Consul metadata/tags.
type RouteDefinition struct {
	ServiceName  string
	Protocol     Protocol
	Host         string
	Path         string
	Port         int
	UpstreamMode UpstreamMode
	Priority     int
	Weight       int
	StripPrefix  bool
	Enabled      bool
	Upstreams    []Upstream
	Via              string // routing tag value for X-Caddy-Consul-Via header
	RedirectCode     int    // HTTP redirect status code (301, 302, etc.); 0 = not a redirect
	RedirectURL      string // redirect target URL template (may contain {http.request.uri})
	NoCacheStatusRaw string // raw no-cache-status spec from metadata (empty = opt-out if HasNoCacheStatus)
	HasNoCacheStatus bool   // true if caddy-no-cache-status key was present (distinguishes unset from empty)
}

// IsRedirect returns true if this route is an HTTP redirect (not a proxy).
func (rd *RouteDefinition) IsRedirect() bool {
	return rd.RedirectCode > 0 && rd.RedirectURL != ""
}

// Upstream represents a single backend target.
type Upstream struct {
	Address  string
	Weight   int
	Healthy  bool
	NodeName string // Consul node name (used for l4_mode filtering; not serialized to Caddy)
}

// Conflict represents a detected route conflict.
type Conflict struct {
	Type   ConflictType
	Winner *RouteDefinition
	Loser  *RouteDefinition
	Reason string
}

// CompiledHTTPRoute is a Consul-managed HTTP route ready for injection into Caddy config.
type CompiledHTTPRoute struct {
	Host         string
	Path         string
	Upstreams    []Upstream
	StripPrefix  bool
	ServiceName  string
	Via            string // routing tag for X-Caddy-Consul-Via response header
	RedirectCode   int
	RedirectURL    string
	NoCacheMatcher *StatusMatcher `json:"-"` // per-service no-cache matcher; nil = use global
	NoCacheOptOut  bool           // true = service explicitly opted out of no-cache headers
}

// CompiledTCPRoute is a Consul-managed TCP route ready for injection into Caddy config.
type CompiledTCPRoute struct {
	Port        int
	SNI         string
	Upstreams   []Upstream
	Passthrough bool
	ServiceName string
}

// CompiledConfig holds the result of route compilation.
type CompiledConfig struct {
	HTTPRoutes []CompiledHTTPRoute
	TCPRoutes  []CompiledTCPRoute
	Conflicts  []Conflict
	Warnings   []string
}
