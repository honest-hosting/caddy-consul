package caddyconsul

import (
	"math/rand"
	"strings"
	"sync"
	"time"

	consul "github.com/hashicorp/consul/api"
	"go.uber.org/zap"
)

// ConsulWatcher watches Consul for service changes using blocking queries.
type ConsulWatcher struct {
	client       *consul.Client
	logger       *zap.Logger
	services     map[string]*ServiceState
	mu           sync.RWMutex
	onChange     func([]ServiceChange, map[string]*ServiceState)
	stopCh       chan struct{}
	healthPolicy HealthPolicy
	debounce     time.Duration
	serviceTag   string // sentinel tag for service proxy discovery (default: "caddy-consul")
	connectTag   string // sentinel tag for connect proxy discovery (default: "caddy-consul-connect")

	// Per-service watch goroutine management
	serviceStopChs map[string]chan struct{}
	serviceStopMu  sync.Mutex

	// Semaphore to limit concurrent health API calls
	healthSem chan struct{}

	// Debounce state
	debounceMu    sync.Mutex
	debounceTimer *time.Timer
	pendingChanges []ServiceChange
	inDebounce    bool

	// Catalog watch index
	catalogIndex uint64

	// stopOnce ensures Stop() is safe to call multiple times.
	stopOnce sync.Once
}

// NewConsulWatcher creates a new ConsulWatcher.
func NewConsulWatcher(
	client *consul.Client,
	logger *zap.Logger,
	healthPolicy HealthPolicy,
	debounce time.Duration,
	maxConcurrentChecks int,
	serviceTag string,
	connectTag string,
	onChange func([]ServiceChange, map[string]*ServiceState),
) *ConsulWatcher {
	if maxConcurrentChecks < 1 {
		maxConcurrentChecks = 5
	}
	return &ConsulWatcher{
		client:         client,
		logger:         logger,
		services:       make(map[string]*ServiceState),
		onChange:        onChange,
		stopCh:         make(chan struct{}),
		healthPolicy:   healthPolicy,
		debounce:       debounce,
		serviceTag:     serviceTag,
		connectTag:     connectTag,
		serviceStopChs: make(map[string]chan struct{}),
		healthSem:      make(chan struct{}, maxConcurrentChecks),
	}
}

// RestoreState restores watcher state from a previous run. This allows the
// watcher to resume blocking queries from where it left off instead of
// re-fetching all services from scratch, avoiding Consul 429 rate limits.
func (w *ConsulWatcher) RestoreState(catalogIndex uint64, services map[string]*persistedServiceState) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.catalogIndex = catalogIndex

	for name, pss := range services {
		w.services[name] = &ServiceState{
			Name:      pss.Name,
			Tags:      pss.Tags,
			Meta:      pss.Meta,
			Instances: pss.Instances,
			LastIndex: pss.LastIndex,
		}
	}

	if len(services) > 0 {
		w.logger.Info("restored watcher state from disk",
			zap.Uint64("catalog_index", catalogIndex),
			zap.Int("services", len(services)),
		)
	}
}

// Start begins watching Consul for service changes. Non-blocking.
// If state was restored via RestoreState, the watcher does NOT re-start
// health watchers for all restored services — the routes are already served
// from persisted state. Only the catalog watcher starts, and it will detect
// changes incrementally using the restored catalog index.
func (w *ConsulWatcher) Start() {
	go w.watchCatalog()
}

// Stop gracefully stops all watchers. Safe to call multiple times.
func (w *ConsulWatcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)

		w.serviceStopMu.Lock()
		for _, ch := range w.serviceStopChs {
			close(ch)
		}
		w.serviceStopChs = make(map[string]chan struct{})
		w.serviceStopMu.Unlock()

		w.debounceMu.Lock()
		if w.debounceTimer != nil {
			w.debounceTimer.Stop()
		}
		w.debounceMu.Unlock()
	})
}

// watchCatalog watches the Consul service catalog for new/removed services.
func (w *ConsulWatcher) watchCatalog() {
	backoff := time.Second

	// On first iteration after restore, use WaitIndex=0 to force a fresh
	// catalog fetch. This ensures reconcileServices runs immediately and
	// starts health watchers for restored services, rather than waiting
	// up to 5 minutes for a catalog change that may never come.
	firstQuery := true

	for {
		select {
		case <-w.stopCh:
			return
		default:
		}

		waitIndex := w.catalogIndex
		if firstQuery {
			// Force a non-blocking fetch on first query to ensure
			// reconcileServices runs immediately and starts health watchers.
			waitIndex = 0
			firstQuery = false
		}

		opts := &consul.QueryOptions{
			WaitIndex: waitIndex,
			WaitTime:  5 * time.Minute,
		}

		services, meta, err := w.client.Catalog().Services(opts)
		if err != nil {
			w.logger.Error("failed to query consul catalog",
				zap.Error(err),
				zap.Duration("retry_in", backoff),
			)
			select {
			case <-w.stopCh:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		backoff = time.Second
		w.catalogIndex = meta.LastIndex

		w.reconcileServices(services)
	}
}

// shouldSkipService returns true for services that can never have routing metadata.
func shouldSkipService(name string) bool {
	if name == "consul" {
		return true
	}
	if strings.HasSuffix(name, "-sidecar-proxy") {
		return true
	}
	return false
}

// reconcileServices compares the catalog response with known services and starts/stops
// per-service health watchers as needed.
func (w *ConsulWatcher) reconcileServices(catalogServices map[string][]string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	var filtered int
	currentNames := make(map[string]bool)
	for name := range catalogServices {
		if shouldSkipService(name) {
			filtered++
			continue
		}
		currentNames[name] = true
	}

	if filtered > 0 {
		w.logger.Debug("filtered services from catalog",
			zap.Int("filtered", filtered),
			zap.Int("remaining", len(currentNames)),
		)
	}

	// Start watchers for new or restored services.
	// Only watch services that signal routing config via tags:
	//   - "urlprefix-*" (Fabio compatibility)
	//   - "caddy-consul" (sentinel tag for metadata-based routing)
	// Services with no tags at all are also watched (they might be legacy).
	var skippedNoRouting int
	for name := range currentNames {
		catalogTags := catalogServices[name]

		if !hasCaddyRoutingTag(catalogTags, w.serviceTag, w.connectTag) {
			skippedNoRouting++
			continue
		}

		if _, exists := w.services[name]; !exists {
			w.logger.Debug("new service discovered", zap.String("service", name))
			w.services[name] = &ServiceState{
				Name: name,
				Tags: catalogTags,
			}
			w.startServiceWatch(name)
		} else {
			// Service exists (possibly from restored state) — ensure it has
			// a running health watcher.
			w.serviceStopMu.Lock()
			_, hasWatcher := w.serviceStopChs[name]
			w.serviceStopMu.Unlock()

			if !hasWatcher {
				w.startServiceWatch(name)
			}
		}
	}
	if skippedNoRouting > 0 {
		w.logger.Debug("skipped services with no routing tags in catalog",
			zap.Int("skipped", skippedNoRouting),
		)
	}

	// Stop watchers for removed services
	var removedChanges []ServiceChange
	for name, svc := range w.services {
		if !currentNames[name] {
			w.logger.Info("service removed", zap.String("service", name))
			w.stopServiceWatch(name)
			removedChanges = append(removedChanges, ServiceChange{
				Type:    ChangeRemoved,
				Service: svc,
			})
			delete(w.services, name)
		}
	}

	if len(removedChanges) > 0 {
		w.queueChanges(removedChanges)
	}
}

// startServiceWatch starts a goroutine to watch health for a specific service.
func (w *ConsulWatcher) startServiceWatch(name string) {
	w.serviceStopMu.Lock()
	stopCh := make(chan struct{})
	w.serviceStopChs[name] = stopCh
	w.serviceStopMu.Unlock()

	go w.watchServiceHealth(name, stopCh)
}

// stopServiceWatch stops the health watcher for a specific service.
func (w *ConsulWatcher) stopServiceWatch(name string) {
	w.serviceStopMu.Lock()
	if ch, ok := w.serviceStopChs[name]; ok {
		close(ch)
		delete(w.serviceStopChs, name)
	}
	w.serviceStopMu.Unlock()
}

// watchServiceHealth watches health endpoint for a specific service using blocking queries.
func (w *ConsulWatcher) watchServiceHealth(name string, stopCh chan struct{}) {
	// Resume from persisted index if available (skips initial fetch burst)
	w.mu.RLock()
	var lastIndex uint64
	if svc, ok := w.services[name]; ok {
		lastIndex = svc.LastIndex
	}
	w.mu.RUnlock()

	backoff := time.Second
	firstQuery := true

	// Stagger initial queries with random jitter to avoid thundering herd
	jitter := time.Duration(rand.Int63n(int64(100 * time.Millisecond)))
	select {
	case <-time.After(jitter):
	case <-w.stopCh:
		return
	case <-stopCh:
		return
	}

	for {
		select {
		case <-w.stopCh:
			return
		case <-stopCh:
			return
		default:
		}

		// Acquire semaphore for the first query from this goroutine (whether
		// fresh or restored) and for non-blocking retries. This prevents a
		// thundering herd when many services are restored or discovered at once.
		// Subsequent blocking queries (long-polls) bypass the semaphore since
		// Consul handles them efficiently.
		needsSemaphore := firstQuery || lastIndex == 0
		if needsSemaphore {
			select {
			case w.healthSem <- struct{}{}:
			case <-w.stopCh:
				return
			case <-stopCh:
				return
			}
		}

		passingOnly := w.healthPolicy == HealthPolicyPassing

		opts := &consul.QueryOptions{
			WaitIndex: lastIndex,
			WaitTime:  5 * time.Minute,
		}

		entries, meta, err := w.client.Health().Service(name, "", passingOnly, opts)

		// Release semaphore
		if needsSemaphore {
			<-w.healthSem
			firstQuery = false
		}

		if err != nil {
			w.logger.Error("failed to query service health",
				zap.String("service", name),
				zap.Error(err),
				zap.Duration("retry_in", backoff),
			)
			select {
			case <-w.stopCh:
				return
			case <-stopCh:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		backoff = time.Second

		if meta.LastIndex == lastIndex {
			continue
		}
		lastIndex = meta.LastIndex

		// If there are no entries (no healthy instances yet), wait briefly and retry
		// instead of falling into a 5-minute blocking query.
		if len(entries) == 0 {
			select {
			case <-time.After(1 * time.Second):
			case <-w.stopCh:
				return
			case <-stopCh:
				return
			}
			lastIndex = 0 // reset to trigger immediate non-blocking query
			continue
		}

		// Convert to our types
		var instances []ServiceInstance
		serviceMeta := make(map[string]string)
		tagSet := make(map[string]bool)

		for _, entry := range entries {
			healthy := w.isHealthy(entry)

			inst := ServiceInstance{
				ID:      entry.Service.ID,
				Address: serviceAddress(entry),
				Port:    entry.Service.Port,
				Tags:    entry.Service.Tags,
				Meta:    entry.Service.Meta,
				Healthy: healthy,
				Weight:  1,
			}

			if entry.Service.Weights.Passing > 0 {
				inst.Weight = entry.Service.Weights.Passing
			}

			instances = append(instances, inst)

			// Collect tags from ALL instances (union)
			for _, tag := range entry.Service.Tags {
				tagSet[tag] = true
			}
			// Collect meta from all instances (first-seen wins for duplicates)
			for k, v := range entry.Service.Meta {
				if _, exists := serviceMeta[k]; !exists {
					serviceMeta[k] = v
				}
			}
		}

		// Convert tag set to slice
		var tags []string
		for tag := range tagSet {
			tags = append(tags, tag)
		}

		w.mu.Lock()
		svc, exists := w.services[name]
		if !exists {
			w.mu.Unlock()
			return
		}

		changeType := ChangeUpdated
		if svc.LastIndex == 0 {
			changeType = ChangeAdded
		}

		svc.Instances = instances
		svc.Tags = tags
		svc.Meta = serviceMeta
		svc.LastIndex = lastIndex
		w.mu.Unlock()

		w.queueChanges([]ServiceChange{
			{Type: changeType, Service: svc},
		})

		// Note: routing config detection is handled at the catalog level
		// (in reconcileServices). Services that pass the catalog filter
		// keep their health watcher running — instances may register
		// gradually and not all have routing tags initially.
	}
}



// hasCaddyRoutingTag returns true if any tag signals routing config:
//   - "urlprefix-*" (Fabio-compatible routing)
//   - serviceTag (sentinel tag for service proxy, default: "caddy-consul")
//   - connectTag (sentinel tag for connect proxy, default: "caddy-consul-connect")
func hasCaddyRoutingTag(tags []string, serviceTag, connectTag string) bool {
	for _, tag := range tags {
		if strings.HasPrefix(tag, "urlprefix-") {
			return true
		}
		if tag == serviceTag || tag == connectTag {
			return true
		}
	}
	return false
}

// isHealthy determines if a service entry is healthy based on the health policy.
func (w *ConsulWatcher) isHealthy(entry *consul.ServiceEntry) bool {
	switch w.healthPolicy {
	case HealthPolicyAny:
		return true
	case HealthPolicyWarning:
		for _, check := range entry.Checks {
			if check.Status == consul.HealthCritical {
				return false
			}
		}
		return true
	default: // HealthPolicyPassing
		for _, check := range entry.Checks {
			if check.Status != consul.HealthPassing {
				return false
			}
		}
		return true
	}
}

// serviceAddress returns the best address for a service entry.
func serviceAddress(entry *consul.ServiceEntry) string {
	if entry.Service.Address != "" {
		return entry.Service.Address
	}
	return entry.Node.Address
}

// queueChanges adds changes to the debounce buffer.
func (w *ConsulWatcher) queueChanges(changes []ServiceChange) {
	w.debounceMu.Lock()
	defer w.debounceMu.Unlock()

	if !w.inDebounce {
		w.inDebounce = true
		w.logger.Warn("debounce started, buffering changes",
			zap.Duration("window", w.debounce),
		)
	}

	w.pendingChanges = append(w.pendingChanges, changes...)

	// Reset or start the debounce timer
	if w.debounceTimer != nil {
		w.debounceTimer.Stop()
	}

	w.debounceTimer = time.AfterFunc(w.debounce, w.flushChanges)
}

// flushChanges fires the onChange callback with all pending changes.
func (w *ConsulWatcher) flushChanges() {
	w.debounceMu.Lock()
	changes := w.pendingChanges
	w.pendingChanges = nil
	w.inDebounce = false
	w.debounceMu.Unlock()

	if len(changes) == 0 {
		return
	}

	// Count unique services
	serviceSet := make(map[string]bool)
	for _, c := range changes {
		serviceSet[c.Service.Name] = true
	}

	w.logger.Warn("debounce flushed",
		zap.Int("changes", len(changes)),
		zap.Int("services", len(serviceSet)),
	)

	// Deep-copy snapshot so the onChange callback gets immutable data.
	// Only include services that have received at least one health response
	// (LastIndex > 0). Services just discovered from the catalog but not yet
	// health-checked would have 0 instances and produce empty routes.
	w.mu.RLock()
	snapshot := make(map[string]*ServiceState, len(w.services))
	for k, v := range w.services {
		if v.LastIndex > 0 {
			snapshot[k] = v.clone()
		}
	}
	w.mu.RUnlock()

	w.onChange(changes, snapshot)
}
