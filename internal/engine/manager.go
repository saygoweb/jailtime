package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sgw/jailtime/internal/config"
	"github.com/sgw/jailtime/internal/watch"
)

// Manager runs all jail runtimes and the watch backend.
type Manager struct {
	cfg             *config.Config
	configPath      string
	jails           map[string]*JailRuntime
	whitelists      map[string]*JailRuntime
	backend         watch.Backend
	mu              sync.RWMutex
	perf            *PerfMetrics
	currentInterval time.Duration
	lastDrainAt     time.Time
	lastExecTime    time.Duration
}

func NewManager(cfg *config.Config, configPath string) (*Manager, error) {
	jails := make(map[string]*JailRuntime, len(cfg.Jails))
	for i := range cfg.Jails {
		jailCfg := &cfg.Jails[i]
		jr, err := NewJailRuntime(jailCfg)
		if err != nil {
			return nil, fmt.Errorf("creating jail runtime %q: %w", jailCfg.Name, err)
		}
		jails[jailCfg.Name] = jr
	}

	whitelists := make(map[string]*JailRuntime, len(cfg.Whitelists))
	for i := range cfg.Whitelists {
		wlCfg := &cfg.Whitelists[i]
		jr, err := NewJailRuntime(wlCfg)
		if err != nil {
			return nil, fmt.Errorf("creating whitelist runtime %q: %w", wlCfg.Name, err)
		}
		whitelists[wlCfg.Name] = jr
	}

	targetLatency := cfg.Engine.TargetLatency.Duration
	if targetLatency == 0 {
		targetLatency = 2000 * time.Millisecond
	}
	backend := watch.NewAuto(cfg.Engine.WatcherMode, targetLatency)

	m := &Manager{
		cfg:             cfg,
		configPath:      configPath,
		jails:           jails,
		whitelists:      whitelists,
		backend:         backend,
		perf:            NewPerfMetrics(targetLatency, "jailtimed.service"),
		currentInterval: targetLatency,
	}
	m.injectIgnoreSets()
	return m, nil
}

// Run starts all enabled whitelists (before jails so membership is available),
// then starts all enabled jails, then starts the watch backend.
func (m *Manager) Run(ctx context.Context) error {
	// Start whitelists first so their static membership is ready before jails process events.
	for name, jr := range m.whitelists {
		if !jr.cfg.Enabled {
			continue
		}
		if err := jr.Start(ctx); err != nil {
			return fmt.Errorf("starting whitelist %q: %w", name, err)
		}
	}
	for name, jr := range m.jails {
		if !jr.cfg.Enabled {
			continue
		}
		if err := jr.Start(ctx); err != nil {
			return fmt.Errorf("starting jail %q: %w", name, err)
		}
	}
	m.mu.RLock()
	specs := m.buildAllSpecs()
	m.mu.RUnlock()

	err := m.backend.Start(ctx, specs, m.processDrain)

	stopCtx := context.Background()
	m.mu.RLock()
	for _, jr := range m.jails {
		if jr.Status() == StatusStarted {
			_ = jr.Stop(stopCtx)
		}
	}
	for _, jr := range m.whitelists {
		if jr.Status() == StatusStarted {
			_ = jr.Stop(stopCtx)
		}
	}
	m.mu.RUnlock()
	m.perf.Close()

	if err != nil && err != context.Canceled {
		return fmt.Errorf("watch backend: %w", err)
	}
	return nil
}

func (m *Manager) processDrain(ctx context.Context, lines []watch.RawLine) {
	drainStart := time.Now()
	if !m.lastDrainAt.IsZero() {
		m.currentInterval = drainStart.Sub(m.lastDrainAt)
	}
	m.lastDrainAt = drainStart

	// Sleep time = interval since last drain minus the previous drain's execution time.
	sleepTime := m.currentInterval - m.lastExecTime

	m.processBatch(ctx, lines)

	execTime := time.Since(drainStart)
	m.perf.RecordExecution(execTime, m.currentInterval, sleepTime, len(lines))
	m.lastExecTime = execTime
}

func (m *Manager) processBatch(ctx context.Context, lines []watch.RawLine) {
	// Collect jail names needed for this batch.
	needed := make(map[string]struct{})
	for _, line := range lines {
		for _, name := range line.Jails {
			needed[name] = struct{}{}
		}
	}

	// Snapshot jail/whitelist runtime pointers under RLock, then release before running
	// HandleEvent (which may execute slow shell actions).
	m.mu.RLock()
	snapshot := make(map[string]*JailRuntime, len(needed))
	for name := range needed {
		if jr, exists := m.jails[name]; exists {
			snapshot[name] = jr
		} else if jr, exists := m.whitelists[name]; exists {
			snapshot[name] = jr
		}
	}
	m.mu.RUnlock()

	for _, line := range lines {
		for _, jailName := range line.Jails {
			jr, exists := snapshot[jailName]
			if !exists || jr.Status() != StatusStarted {
				continue
			}
			evt := watch.Event{
				JailName: jailName,
				FilePath: line.FilePath,
				Line:     line.Line,
				Time:     line.EnqueueAt,
				Kind:     line.Kind,
			}
			if err := jr.HandleEvent(ctx, evt); err != nil {
				slog.Warn("event processing error", "jail", jailName, "error", err)
			}
		}
	}
}

// PerfStats returns a snapshot of current performance metrics.
func (m *Manager) PerfStats() PerfSnapshot {
	return m.perf.Snapshot()
}

// StartJail starts a specific jail by name.
func (m *Manager) StartJail(ctx context.Context, name string) error {
	m.mu.RLock()
	jr, ok := m.jails[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("jail %q not found", name)
	}
	return jr.Start(ctx)
}

// StopJail stops a specific jail by name.
func (m *Manager) StopJail(ctx context.Context, name string) error {
	m.mu.RLock()
	jr, ok := m.jails[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("jail %q not found", name)
	}
	return jr.Stop(ctx)
}

// RestartJail reloads the config from disk, reconciles the set of running jails
// (stopping removed/disabled jails and starting newly-added ones), then starts
// or restarts the named jail.
func (m *Manager) RestartJail(ctx context.Context, name string) error {
	newCfg, err := config.Load(m.configPath)
	if err != nil {
		return fmt.Errorf("reloading config: %w", err)
	}

	newJailCfgs := make(map[string]*config.JailConfig, len(newCfg.Jails))
	for i := range newCfg.Jails {
		newJailCfgs[newCfg.Jails[i].Name] = &newCfg.Jails[i]
	}

	m.mu.Lock()

	// Collect jails to stop (removed or disabled) and remove them from the map.
	// For jails that remain enabled, apply the new config in-place.
	var toStop []*JailRuntime
	for jailName, jr := range m.jails {
		newJailCfg, exists := newJailCfgs[jailName]
		if !exists || !newJailCfg.Enabled {
			if jr.Status() == StatusStarted {
				toStop = append(toStop, jr)
			}
			delete(m.jails, jailName)
		} else {
			// Jail still exists and is enabled — update its config now so that
			// subsequent HandleEvent calls and on_restart actions use the new values.
			if err := jr.Reconfigure(newJailCfg); err != nil {
				m.mu.Unlock()
				return fmt.Errorf("reconfiguring jail %q: %w", jailName, err)
			}
		}
	}

	// Add runtimes for newly-discovered jails and collect those to start,
	// excluding the target jail which is handled separately below.
	var toStart []*JailRuntime
	for jailName, newJailCfg := range newJailCfgs {
		if _, exists := m.jails[jailName]; !exists {
			jr, err := NewJailRuntime(newJailCfg)
			if err != nil {
				m.mu.Unlock()
				return fmt.Errorf("creating jail runtime %q: %w", jailName, err)
			}
			m.jails[jailName] = jr
			if newJailCfg.Enabled && jailName != name {
				toStart = append(toStart, jr)
			}
		}
	}

	targetJr, targetFound := m.jails[name]
	targetWasStarted := targetFound && targetJr.Status() == StatusStarted

	// Update the stored config so buildAllSpecs uses the new engine settings.
	m.cfg = newCfg
	specs := m.buildAllSpecs()

	m.mu.Unlock()

	// Stop removed/disabled jails outside the lock (may run on_stop actions).
	for _, jr := range toStop {
		slog.Info("stopping removed/disabled jail", "jail", jr.cfg.Name)
		if stopErr := jr.Stop(ctx); stopErr != nil {
			slog.Warn("stopping jail", "jail", jr.cfg.Name, "error", stopErr)
		}
	}

	// Start newly-added jails outside the lock (may run on_start actions).
	for _, jr := range toStart {
		slog.Info("starting new jail", "jail", jr.cfg.Name)
		if startErr := jr.Start(ctx); startErr != nil {
			slog.Warn("starting new jail", "jail", jr.cfg.Name, "error", startErr)
		}
	}

	// Let the watch backend pick up new specs (new/removed jail file globs).
	m.backend.UpdateSpecs(specs)

	if !targetFound {
		return fmt.Errorf("jail %q not found", name)
	}

	if targetWasStarted {
		slog.Info("restarting jail", "jail", name)
		return targetJr.Restart(ctx)
	}
	slog.Info("starting jail", "jail", name)
	return targetJr.Start(ctx)
}

// buildAllSpecs builds watch specs for all enabled jails and whitelists.
func (m *Manager) buildAllSpecs() []watch.WatchSpec {
	specs := buildSpecs(m.jails, m.cfg.Engine.ReadFromEnd)
	specs = append(specs, buildSpecs(m.whitelists, m.cfg.Engine.ReadFromEnd)...)
	return specs
}

// injectIgnoreSets injects MembershipChecker closures into jail runtimes
// that have ignore_sets configured. Must be called after all runtimes are built.
// Not concurrency-safe; call only during construction or while holding m.mu.
func (m *Manager) injectIgnoreSets() {
	for _, jr := range m.jails {
		ignoreSets := jr.cfg.IgnoreSets
		if len(ignoreSets) == 0 {
			continue
		}
		// Capture the whitelist names and the whitelists map.
		wlNames := ignoreSets
		wls := m.whitelists
		checker := func(ip string) bool {
			for _, name := range wlNames {
				if wl, ok := wls[name]; ok {
					if wl.IsMember(ip) {
						return true
					}
				}
			}
			return false
		}
		jr.mu.Lock()
		jr.ignoreSetsChecker = checker
		jr.mu.Unlock()
	}
}

// buildSpecs builds watch specs for all enabled jails in the map.
func buildSpecs(jails map[string]*JailRuntime, readFromEnd bool) []watch.WatchSpec {
	specs := make([]watch.WatchSpec, 0, len(jails))
	for _, jr := range jails {
		if jr.cfg.Enabled {
			specs = append(specs, watch.WatchSpec{
				JailName:     jr.cfg.Name,
				Globs:        jr.cfg.Files,
				ExcludeGlobs: jr.cfg.ExcludeFiles,
				WatchMode:    jr.cfg.WatchMode,
				ReadFromEnd:  readFromEnd,
			})
		}
	}
	return specs
}

// JailStatus returns the status of a jail by name.
func (m *Manager) JailStatus(name string) (JailStatus, error) {
	m.mu.RLock()
	jr, ok := m.jails[name]
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("jail %q not found", name)
	}
	return jr.Status(), nil
}

// AllJailStatuses returns a snapshot of jail name → status.
func (m *Manager) AllJailStatuses() map[string]JailStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]JailStatus, len(m.jails))
	for name, jr := range m.jails {
		out[name] = jr.Status()
	}
	return out
}

// ConfigFiles returns the file paths matching the jail's configured globs,
// capped at limit (0 = no limit). If logFiles is true each match is logged.
func (m *Manager) ConfigFiles(name string, limit int, logFiles bool) ([]string, error) {
	m.mu.RLock()
	jr, ok := m.jails[name]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("jail %q not found", name)
	}
	return jr.ConfigFiles(limit, logFiles), nil
}

// ConfigTest runs the jail's filters against every line in filePath without
// triggering any actions. Returns total lines, matching lines, and optionally
// up to limit matching lines (0 = no limit).
func (m *Manager) ConfigTest(name, filePath string, limit int, returnMatching bool) (totalLines, matchingLines int, matches []string, err error) {
	m.mu.RLock()
	jr, ok := m.jails[name]
	m.mu.RUnlock()
	if !ok {
		return 0, 0, nil, fmt.Errorf("jail %q not found", name)
	}
	return jr.ConfigTest(filePath, limit, returnMatching)
}

// WhitelistStatus returns the status of a whitelist by name.
func (m *Manager) WhitelistStatus(name string) (JailStatus, error) {
	m.mu.RLock()
	jr, ok := m.whitelists[name]
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("whitelist %q not found", name)
	}
	return jr.Status(), nil
}

// AllWhitelistStatuses returns a snapshot of whitelist name → status.
func (m *Manager) AllWhitelistStatuses() map[string]JailStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]JailStatus, len(m.whitelists))
	for name, jr := range m.whitelists {
		out[name] = jr.Status()
	}
	return out
}

// StartWhitelist starts a specific whitelist by name.
func (m *Manager) StartWhitelist(ctx context.Context, name string) error {
	m.mu.RLock()
	jr, ok := m.whitelists[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("whitelist %q not found", name)
	}
	return jr.Start(ctx)
}

// StopWhitelist stops a specific whitelist by name.
func (m *Manager) StopWhitelist(ctx context.Context, name string) error {
	m.mu.RLock()
	jr, ok := m.whitelists[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("whitelist %q not found", name)
	}
	return jr.Stop(ctx)
}

// RestartWhitelist reloads the config and restarts the named whitelist.
func (m *Manager) RestartWhitelist(ctx context.Context, name string) error {
	newCfg, err := config.Load(m.configPath)
	if err != nil {
		return fmt.Errorf("reloading config: %w", err)
	}

	newWlCfgs := make(map[string]*config.JailConfig, len(newCfg.Whitelists))
	for i := range newCfg.Whitelists {
		newWlCfgs[newCfg.Whitelists[i].Name] = &newCfg.Whitelists[i]
	}

	m.mu.Lock()

	var toStop []*JailRuntime
	for wlName, jr := range m.whitelists {
		newWlCfg, exists := newWlCfgs[wlName]
		if !exists || !newWlCfg.Enabled {
			if jr.Status() == StatusStarted {
				toStop = append(toStop, jr)
			}
			delete(m.whitelists, wlName)
		} else {
			if err := jr.Reconfigure(newWlCfg); err != nil {
				m.mu.Unlock()
				return fmt.Errorf("reconfiguring whitelist %q: %w", wlName, err)
			}
		}
	}

	var toStart []*JailRuntime
	for wlName, newWlCfg := range newWlCfgs {
		if _, exists := m.whitelists[wlName]; !exists {
			jr, err := NewJailRuntime(newWlCfg)
			if err != nil {
				m.mu.Unlock()
				return fmt.Errorf("creating whitelist runtime %q: %w", wlName, err)
			}
			m.whitelists[wlName] = jr
			if newWlCfg.Enabled && wlName != name {
				toStart = append(toStart, jr)
			}
		}
	}

	targetJr, targetFound := m.whitelists[name]
	targetWasStarted := targetFound && targetJr.Status() == StatusStarted

	m.cfg = newCfg
	m.injectIgnoreSets()
	specs := m.buildAllSpecs()

	m.mu.Unlock()

	for _, jr := range toStop {
		slog.Info("stopping removed/disabled whitelist", "whitelist", jr.cfg.Name)
		if stopErr := jr.Stop(ctx); stopErr != nil {
			slog.Warn("stopping whitelist", "whitelist", jr.cfg.Name, "error", stopErr)
		}
	}

	for _, jr := range toStart {
		slog.Info("starting new whitelist", "whitelist", jr.cfg.Name)
		if startErr := jr.Start(ctx); startErr != nil {
			slog.Warn("starting new whitelist", "whitelist", jr.cfg.Name, "error", startErr)
		}
	}

	m.backend.UpdateSpecs(specs)

	if !targetFound {
		return fmt.Errorf("whitelist %q not found", name)
	}

	if targetWasStarted {
		slog.Info("restarting whitelist", "whitelist", name)
		return targetJr.Restart(ctx)
	}
	slog.Info("starting whitelist", "whitelist", name)
	return targetJr.Start(ctx)
}
