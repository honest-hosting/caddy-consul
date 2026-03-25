package caddyconsul

import (
	"strings"
	"sync"
	"time"

	consul "github.com/hashicorp/consul/api"
	"go.uber.org/zap"
)

// ConsulWatcher watches Consul for service changes using two blocking queries:
//   - /v1/catalog/services — detects service add/remove
//   - /v1/health/state/passing — detects health changes across ALL services
//
// This architecture uses 2 connections regardless of service count, scaling
// to 10,000+ services without overwhelming the Consul agent.
type ConsulWatcher struct {
	client       *consul.Client
	logger       *zap.Logger
	services     map[string]*ServiceState
	mu           sync.RWMutex
	onChange     func([]ServiceChange, map[string]*ServiceState)
	stopCh       chan struct{}
	healthPolicy HealthPolicy
	debounce     time.Duration
	pollInterval time.Duration
	fullSyncInterval time.Duration
	serviceTag   string
	connectTag   string

	// Debounce state
	debounceMu    sync.Mutex
	debounceTimer *time.Timer
	pendingChanges []ServiceChange
	inDebounce    bool

	// Blocking query indexes
	catalogIndex     uint64
	healthStateIndex uint64

	// Health state tracking for diffing
	lastPassingChecks map[string]map[string]bool // ServiceName → set of passing ServiceIDs

	// Names of routable services (filtered by tags)
	routableServices map[string]bool

	// stopOnce ensures Stop() is safe to call multiple times.
	stopOnce sync.Once
}

// NewConsulWatcher creates a new ConsulWatcher.
func NewConsulWatcher(
	client *consul.Client,
	logger *zap.Logger,
	healthPolicy HealthPolicy,
	debounce time.Duration,
	pollInterval time.Duration,
	fullSyncInterval time.Duration,
	serviceTag string,
	connectTag string,
	onChange func([]ServiceChange, map[string]*ServiceState),
) *ConsulWatcher {
	return &ConsulWatcher{
		client:            client,
		logger:            logger,
		services:          make(map[string]*ServiceState),
		onChange:           onChange,
		stopCh:            make(chan struct{}),
		healthPolicy:      healthPolicy,
		debounce:          debounce,
		pollInterval:      pollInterval,
		fullSyncInterval:  fullSyncInterval,
		serviceTag:        serviceTag,
		connectTag:        connectTag,
		lastPassingChecks: make(map[string]map[string]bool),
		routableServices:  make(map[string]bool),
	}
}

// RestoreState restores watcher state from a previous run.
func (w *ConsulWatcher) RestoreState(catalogIndex uint64, healthStateIndex uint64, services map[string]*persistedServiceState, passingChecks map[string][]string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.catalogIndex = catalogIndex
	w.healthStateIndex = healthStateIndex

	for name, pss := range services {
		w.services[name] = &ServiceState{
			Name:      pss.Name,
			Tags:      pss.Tags,
			Meta:      pss.Meta,
			Instances: pss.Instances,
			LastIndex: pss.LastIndex,
		}
		w.routableServices[name] = true
	}

	// Restore passing checks for diffing
	for svcName, ids := range passingChecks {
		idSet := make(map[string]bool, len(ids))
		for _, id := range ids {
			idSet[id] = true
		}
		w.lastPassingChecks[svcName] = idSet
	}

	if len(services) > 0 {
		w.logger.Info("restored watcher state from disk",
			zap.Uint64("catalog_index", catalogIndex),
			zap.Uint64("health_state_index", healthStateIndex),
			zap.Int("services", len(services)),
		)
	}
}

// Start begins watching Consul for service changes. Non-blocking.
// Launches 3 goroutines: catalog watcher, health state watcher, and periodic full sync.
func (w *ConsulWatcher) Start() {
	go w.watchCatalog()
	go w.watchHealthState()
	go w.periodicFullSync()
}

// Stop gracefully stops all watchers. Safe to call multiple times.
func (w *ConsulWatcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)

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
	firstQuery := true

	for {
		select {
		case <-w.stopCh:
			return
		default:
		}

		waitIndex := w.catalogIndex
		if firstQuery {
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

// reconcileServices compares the catalog response with known services.
// New routable services get a single health fetch. Removed services are cleaned up.
func (w *ConsulWatcher) reconcileServices(catalogServices map[string][]string) {
	var filtered, skippedNoRouting int
	currentRoutable := make(map[string]bool)

	for name, tags := range catalogServices {
		if shouldSkipService(name) {
			filtered++
			continue
		}
		if !hasCaddyRoutingTag(tags, w.serviceTag, w.connectTag) {
			skippedNoRouting++
			continue
		}
		currentRoutable[name] = true
	}

	if filtered > 0 || skippedNoRouting > 0 {
		w.logger.Debug("filtered services from catalog",
			zap.Int("sidecar_proxy_filtered", filtered),
			zap.Int("no_routing_tag", skippedNoRouting),
			zap.Int("routable", len(currentRoutable)),
		)
	}

	// Detect new and removed services under the lock
	w.mu.Lock()

	var newServices []string
	for name := range currentRoutable {
		if _, exists := w.services[name]; !exists {
			newServices = append(newServices, name)
		}
	}

	var removedChanges []ServiceChange
	for name, svc := range w.services {
		if !currentRoutable[name] {
			w.logger.Info("service removed", zap.String("service", name))
			removedChanges = append(removedChanges, ServiceChange{
				Type:    ChangeRemoved,
				Service: svc,
			})
			delete(w.services, name)
			delete(w.routableServices, name)
		}
	}

	w.routableServices = currentRoutable
	w.mu.Unlock()

	// Queue removals
	if len(removedChanges) > 0 {
		w.queueChanges(removedChanges)
	}

	// Fetch health for new services (sequential, rate-limited, outside lock)
	for _, name := range newServices {
		select {
		case <-w.stopCh:
			return
		default:
		}

		w.logger.Debug("new service discovered, fetching health",
			zap.String("service", name),
		)
		w.fetchServiceHealth(name)

		if w.pollInterval > 0 {
			select {
			case <-time.After(w.pollInterval):
			case <-w.stopCh:
				return
			}
		}
	}
}

// watchHealthState watches /v1/health/state/passing for ANY health change
// across ALL services. When a change is detected, diffs the passing checks
// and updates Healthy flags on cached instances — no extra queries needed.
func (w *ConsulWatcher) watchHealthState() {
	backoff := time.Second
	firstQuery := true

	w.logger.Info("health state watcher started",
		zap.Uint64("initial_index", w.healthStateIndex),
	)

	for {
		select {
		case <-w.stopCh:
			return
		default:
		}

		opts := &consul.QueryOptions{
			WaitIndex: w.healthStateIndex,
			WaitTime:  5 * time.Minute,
		}

		checks, meta, err := w.client.Health().State("passing", opts)
		if err != nil {
			w.logger.Error("failed to query health state",
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

		if meta.LastIndex == w.healthStateIndex {
			continue
		}
		w.healthStateIndex = meta.LastIndex

		if firstQuery {
			firstQuery = false
			w.logger.Info("health state watcher initial response",
				zap.Int("passing_checks", len(checks)),
				zap.Uint64("index", meta.LastIndex),
			)
		}

		// Build current passing checks map: ServiceName → set of ServiceIDs
		currentPassing := make(map[string]map[string]bool)
		for _, check := range checks {
			if check.ServiceName == "" {
				continue // node-level check, not service-specific
			}
			if _, ok := currentPassing[check.ServiceName]; !ok {
				currentPassing[check.ServiceName] = make(map[string]bool)
			}
			currentPassing[check.ServiceName][check.ServiceID] = true
		}

		// Diff against last known state — find services with changed health
		w.mu.Lock()
		var changedServices []string

		// Check for services that lost or gained healthy instances
		allServiceNames := make(map[string]bool)
		for name := range w.lastPassingChecks {
			allServiceNames[name] = true
		}
		for name := range currentPassing {
			allServiceNames[name] = true
		}

		for name := range allServiceNames {
			if !w.routableServices[name] {
				continue // not a service we care about
			}

			oldIDs := w.lastPassingChecks[name]
			newIDs := currentPassing[name]

			if !sameIDSet(oldIDs, newIDs) {
				changedServices = append(changedServices, name)
			}
		}

		// Update passing checks snapshot
		w.lastPassingChecks = currentPassing

		// Update Healthy flags on cached instances, or re-fetch if new IDs appear
		var changes []ServiceChange
		var needsRefetch []string

		for _, name := range changedServices {
			svc, exists := w.services[name]
			if !exists {
				continue
			}

			passingIDs := currentPassing[name]

			// Check if there are new ServiceIDs not in our cached instances
			// (e.g., health check just passed for the first time, or new instance added)
			cachedIDs := make(map[string]bool)
			for _, inst := range svc.Instances {
				cachedIDs[inst.ID] = true
			}

			hasNewIDs := false
			for id := range passingIDs {
				if !cachedIDs[id] {
					hasNewIDs = true
					break
				}
			}

			if hasNewIDs || len(svc.Instances) == 0 {
				// New instance IDs or empty cache — need full re-fetch
				needsRefetch = append(needsRefetch, name)
				continue
			}

			// Just update Healthy flags on existing instances
			anyChanged := false
			for i := range svc.Instances {
				wasHealthy := svc.Instances[i].Healthy
				nowHealthy := passingIDs[svc.Instances[i].ID]

				if wasHealthy != nowHealthy {
					svc.Instances[i].Healthy = nowHealthy
					anyChanged = true
				}
			}

			if anyChanged {
				changes = append(changes, ServiceChange{
					Type:    ChangeUpdated,
					Service: svc,
				})
			}
		}

		w.mu.Unlock()

		// Re-fetch services with new/unknown instance IDs
		for _, name := range needsRefetch {
			w.fetchServiceHealth(name)
		}

		if len(changes) > 0 {
			w.logger.Info("health state changed",
				zap.Int("affected_services", len(changes)),
				zap.Int("refetched", len(needsRefetch)),
			)
			w.queueChanges(changes)
		}
	}
}

// sameIDSet returns true if two sets contain the same elements.
func sameIDSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if !b[id] {
			return false
		}
	}
	return true
}

// fetchServiceHealth fetches full health details for a single service and
// updates the service state. Used for new services and full sync.
func (w *ConsulWatcher) fetchServiceHealth(name string) {
	passingOnly := w.healthPolicy == HealthPolicyPassing

	entries, meta, err := w.client.Health().Service(name, "", passingOnly, nil)
	if err != nil {
		w.logger.Error("failed to fetch service health",
			zap.String("service", name),
			zap.Error(err),
		)
		return
	}

	var instances []ServiceInstance
	serviceMeta := make(map[string]string)
	tagSet := make(map[string]bool)

	for _, entry := range entries {
		healthy := w.isHealthy(entry)

		inst := ServiceInstance{
			ID:       entry.Service.ID,
			Address:  serviceAddress(entry),
			Port:     entry.Service.Port,
			Tags:     entry.Service.Tags,
			Meta:     entry.Service.Meta,
			Healthy:  healthy,
			Weight:   1,
			NodeName: entry.Node.Node,
		}

		if entry.Service.Weights.Passing > 0 {
			inst.Weight = entry.Service.Weights.Passing
		}

		instances = append(instances, inst)

		for _, tag := range entry.Service.Tags {
			tagSet[tag] = true
		}
		for k, v := range entry.Service.Meta {
			if _, exists := serviceMeta[k]; !exists {
				serviceMeta[k] = v
			}
		}
	}

	var tags []string
	for tag := range tagSet {
		tags = append(tags, tag)
	}

	w.mu.Lock()
	svc, exists := w.services[name]
	changeType := ChangeUpdated
	if !exists {
		svc = &ServiceState{Name: name}
		w.services[name] = svc
		changeType = ChangeAdded
	}

	svc.Instances = instances
	svc.Tags = tags
	svc.Meta = serviceMeta
	svc.LastIndex = meta.LastIndex
	w.mu.Unlock()

	w.queueChanges([]ServiceChange{
		{Type: changeType, Service: svc},
	})
}

// periodicFullSync periodically re-fetches all routable services to catch
// metadata/tag changes that aren't detected by the health state watcher.
func (w *ConsulWatcher) periodicFullSync() {
	if w.fullSyncInterval <= 0 {
		return
	}

	ticker := time.NewTicker(w.fullSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.fullSync()
		}
	}
}

// fullSync re-fetches health for all routable services sequentially.
func (w *ConsulWatcher) fullSync() {
	w.mu.RLock()
	var names []string
	for name := range w.routableServices {
		names = append(names, name)
	}
	w.mu.RUnlock()

	w.logger.Info("starting full sync",
		zap.Int("services", len(names)),
	)

	for _, name := range names {
		select {
		case <-w.stopCh:
			return
		default:
		}

		w.fetchServiceHealth(name)

		if w.pollInterval > 0 {
			select {
			case <-time.After(w.pollInterval):
			case <-w.stopCh:
				return
			}
		}
	}

	w.logger.Info("full sync completed",
		zap.Int("services", len(names)),
	)
}

// hasCaddyRoutingTag returns true if any tag signals routing config.
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

	serviceSet := make(map[string]bool)
	for _, c := range changes {
		serviceSet[c.Service.Name] = true
	}

	w.logger.Warn("debounce flushed",
		zap.Int("changes", len(changes)),
		zap.Int("services", len(serviceSet)),
	)

	w.mu.RLock()
	snapshot := make(map[string]*ServiceState, len(w.services))
	for k, v := range w.services {
		if v.LastIndex > 0 || len(v.Instances) > 0 {
			snapshot[k] = v.clone()
		}
	}
	w.mu.RUnlock()

	w.onChange(changes, snapshot)
}

// HealthStateIndex returns the current health state blocking query index.
func (w *ConsulWatcher) HealthStateIndex() uint64 {
	return w.healthStateIndex
}

// PassingChecks returns the current passing checks map for state persistence.
func (w *ConsulWatcher) PassingChecks() map[string][]string {
	w.mu.RLock()
	defer w.mu.RUnlock()

	result := make(map[string][]string, len(w.lastPassingChecks))
	for svcName, ids := range w.lastPassingChecks {
		idList := make([]string, 0, len(ids))
		for id := range ids {
			idList = append(idList, id)
		}
		result[svcName] = idList
	}
	return result
}

// CatalogIndex returns the current catalog blocking query index (exported for state persistence).
func (w *ConsulWatcher) CatalogIndex() uint64 {
	return w.catalogIndex
}
