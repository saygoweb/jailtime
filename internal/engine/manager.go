package engine

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/sgw/jailtime/internal/config"
	"github.com/sgw/jailtime/internal/watch"
)

// eventTask is dispatched to the worker pool for asynchronous HandleEvent calls.
type eventTask struct {
	jr  *JailRuntime
	evt watch.Event
}

const (
	// targetCPUFraction is the maximum fraction of available CPU (across all
	// GOMAXPROCS cores) that the daemon should consume on average.
	targetCPUFraction = 0.02

	// cpuCheckInterval is how often the event loop samples CPU usage.
	cpuCheckInterval = time.Second

	// maxDispatchDelay caps how long the event loop sleeps when CPU is over target.
	maxDispatchDelay = 2 * time.Second

	// eventQueueSize is the buffered event channel capacity. A large buffer lets
	// the watch backend keep writing even while the event loop is sleeping to
	// throttle CPU.
	eventQueueSize = 65536

	// taskQueueSize is the worker-pool task queue depth.
	taskQueueSize = 4096
)

// Manager runs all jail runtimes and the watch backend.
type Manager struct {
	cfg        *config.Config
	configPath string
	jails      map[string]*JailRuntime
	backend    watch.Backend
	mu         sync.RWMutex
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

	pollInterval := cfg.Engine.PollInterval.Duration
	if pollInterval == 0 {
		pollInterval = 2 * time.Second
	}
	backend := watch.NewAuto(cfg.Engine.WatcherMode, pollInterval)

	return &Manager{
		cfg:        cfg,
		configPath: configPath,
		jails:      jails,
		backend:    backend,
	}, nil
}

// Run starts all enabled jails, starts the watch backend, routes events, and
// blocks until ctx is cancelled.
//
// Events from the watch backend are queued in a large buffered channel
// (eventQueueSize) so the backend never blocks. A bounded worker pool
// (runtime.GOMAXPROCS * 4 goroutines) handles HandleEvent calls, avoiding
// unbounded goroutine creation under high log volume.
//
// CPU usage is sampled every cpuCheckInterval; if it exceeds targetCPUFraction
// the event loop sleeps proportionally before dispatching more work, keeping
// average CPU below the target.
func (m *Manager) Run(ctx context.Context) error {
	// Start all enabled jails.
	for name, jr := range m.jails {
		if !jr.cfg.Enabled {
			continue
		}
		if err := jr.Start(ctx); err != nil {
			return fmt.Errorf("starting jail %q: %w", name, err)
		}
	}

	// Build watch specs for enabled jails.
	m.mu.RLock()
	specs := make([]watch.WatchSpec, 0, len(m.jails))
	for _, jr := range m.jails {
		if jr.cfg.Enabled {
			specs = append(specs, watch.WatchSpec{
				JailName:    jr.cfg.Name,
				Globs:       jr.cfg.Files,
				ReadFromEnd: m.cfg.Engine.ReadFromEnd,
			})
		}
	}
	m.mu.RUnlock()

	events := make(chan watch.Event, eventQueueSize)

	// Worker pool: bounded goroutines handle HandleEvent calls so we never
	// create an unbounded number of goroutines under high log volume.
	numWorkers := runtime.GOMAXPROCS(0) * 4
	taskQueue := make(chan eventTask, taskQueueSize)
	var workerWg sync.WaitGroup
	workerWg.Add(numWorkers)
	for range numWorkers {
		go func() {
			defer workerWg.Done()
			for task := range taskQueue {
				if err := task.jr.HandleEvent(ctx, task.evt); err != nil {
					slog.Warn("event processing error", "jail", task.evt.JailName, "error", err)
				}
			}
		}()
	}
	defer func() {
		close(taskQueue)
		workerWg.Wait()
	}()

	// CPU-aware throttle: sleep when usage exceeds targetCPUFraction.
	cpuSampler := newCPUSampler()
	var (
		lastCPUCheck  = time.Now()
		dispatchDelay time.Duration
		throttling    bool
	)
	checkCPU := func() {
		if time.Since(lastCPUCheck) < cpuCheckInterval {
			return
		}
		lastCPUCheck = time.Now()
		usage := cpuSampler.sample()
		if usage > targetCPUFraction {
			// Overshoot ratio drives the sleep duration: at 2× target we sleep
			// ~targetCPUFraction * cpuCheckInterval, at 4× we sleep twice as long.
			overshoot := usage/targetCPUFraction - 1.0
			dispatchDelay = time.Duration(overshoot * float64(cpuCheckInterval))
			if dispatchDelay > maxDispatchDelay {
				dispatchDelay = maxDispatchDelay
			}
			if !throttling {
				slog.Info("event dispatch throttled", "cpu_usage_pct", fmt.Sprintf("%.1f", usage*100), "delay_ms", dispatchDelay.Milliseconds())
				throttling = true
			}
		} else {
			if throttling {
				slog.Info("event dispatch throttle lifted", "cpu_usage_pct", fmt.Sprintf("%.1f", usage*100))
			}
			dispatchDelay = 0
			throttling = false
		}
	}

	// Start the watch backend in a goroutine — its Start method blocks until
	// ctx is cancelled, so the event-routing loop below must run concurrently.
	backendErr := make(chan error, 1)
	go func() {
		if err := m.backend.Start(ctx, specs, events); err != nil && err != context.Canceled {
			backendErr <- err
		}
		close(backendErr)
	}()

	for {
		select {
		case err := <-backendErr:
			if err != nil {
				return fmt.Errorf("watch backend: %w", err)
			}
			return nil

		case <-ctx.Done():
			stopCtx := context.Background()
			m.mu.RLock()
			for name, jr := range m.jails {
				if jr.Status() == StatusStarted {
					_ = jr.Stop(stopCtx)
					_ = name
				}
			}
			m.mu.RUnlock()
			return ctx.Err()

		case evt, ok := <-events:
			if !ok {
				return nil
			}
			m.mu.RLock()
			jr, exists := m.jails[evt.JailName]
			m.mu.RUnlock()
			if !exists {
				slog.Warn("event for unknown jail, dropping", "jail", evt.JailName)
				continue
			}
			select {
			case taskQueue <- eventTask{jr: jr, evt: evt}:
			case <-ctx.Done():
				return ctx.Err()
			}
			checkCPU()
			if dispatchDelay > 0 {
				select {
				case <-time.After(dispatchDelay):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
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

	specs := buildSpecs(m.jails, newCfg.Engine.ReadFromEnd)

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

// buildSpecs builds watch specs for all enabled jails.
func buildSpecs(jails map[string]*JailRuntime, readFromEnd bool) []watch.WatchSpec {
	specs := make([]watch.WatchSpec, 0, len(jails))
	for _, jr := range jails {
		if jr.cfg.Enabled {
			specs = append(specs, watch.WatchSpec{
				JailName:    jr.cfg.Name,
				Globs:       jr.cfg.Files,
				ReadFromEnd: readFromEnd,
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
