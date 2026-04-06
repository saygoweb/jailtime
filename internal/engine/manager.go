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

const emaAlpha = 0.3

type pendingItem struct {
	line      watch.RawLine
	enqueueAt time.Time
}

type batchQueue struct {
	mu    sync.Mutex
	items []pendingItem
}

func (q *batchQueue) Enqueue(line watch.RawLine) {
	q.mu.Lock()
	q.items = append(q.items, pendingItem{line: line, enqueueAt: time.Now()})
	q.mu.Unlock()
}

func (q *batchQueue) Drain() []pendingItem {
	q.mu.Lock()
	batch := q.items
	q.items = make([]pendingItem, 0, cap(batch))
	q.mu.Unlock()
	return batch
}

// Manager runs all jail runtimes and the watch backend.
type Manager struct {
	cfg             *config.Config
	configPath      string
	jails           map[string]*JailRuntime
	backend         watch.Backend
	mu              sync.RWMutex
	perf            *PerfMetrics
	queue           batchQueue
	minLatency      time.Duration
	maxLatency      time.Duration
	currentInterval time.Duration
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

	minLatency := cfg.Engine.MinLatency.Duration
	if minLatency == 0 {
		minLatency = 2 * time.Second
	}
	maxLatency := cfg.Engine.MaxLatency.Duration
	if maxLatency == 0 {
		maxLatency = 10 * time.Second
	}
	perfWindow := cfg.Engine.PerfWindow
	if perfWindow == 0 {
		perfWindow = 3
	}

	return &Manager{
		cfg:             cfg,
		configPath:      configPath,
		jails:           jails,
		backend:         backend,
		perf:            NewPerfMetrics(perfWindow, "jailtimed.service"),
		minLatency:      minLatency,
		maxLatency:      maxLatency,
		currentInterval: minLatency,
	}, nil
}

// Run starts all enabled jails, starts the watch backend, and routes events
// via a timer-based batch queue with EMA-based adaptive interval.
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

	rawLines := make(chan watch.RawLine, 4096)
	backendErr := make(chan error, 1)
	go func() {
		if err := m.backend.Start(ctx, specs, rawLines); err != nil && err != context.Canceled {
			backendErr <- err
		}
		close(backendErr)
	}()

	// Enqueue goroutine: reads from rawLines channel, pushes to batch queue.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case line, ok := <-rawLines:
				if !ok {
					return
				}
				m.queue.Enqueue(line)
			}
		}
	}()

	// Drain loop: timer-based batch processing.
	timer := time.NewTimer(m.currentInterval)
	defer timer.Stop()

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
			for _, jr := range m.jails {
				if jr.Status() == StatusStarted {
					_ = jr.Stop(stopCtx)
				}
			}
			m.mu.RUnlock()
			return ctx.Err()

		case <-timer.C:
			batch := m.queue.Drain()
			drainStart := time.Now()

			var measuredLatency time.Duration
			if len(batch) > 0 {
				measuredLatency = drainStart.Sub(batch[0].enqueueAt)
			}

			m.processBatch(ctx, batch)
			execTime := time.Since(drainStart)

			m.perf.RecordExecution(execTime, measuredLatency, len(batch), m.currentInterval)
			nextInterval := m.adaptInterval(execTime, len(batch), measuredLatency)
			timer.Reset(nextInterval)
		}
	}
}

func (m *Manager) processBatch(ctx context.Context, batch []pendingItem) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, item := range batch {
		for _, jailName := range item.line.Jails {
			jr, exists := m.jails[jailName]
			if !exists || jr.Status() != StatusStarted {
				continue
			}
			evt := watch.Event{
				JailName: jailName,
				FilePath: item.line.FilePath,
				Line:     item.line.Line,
				Time:     item.line.EnqueueAt,
			}
			if err := jr.HandleEvent(ctx, evt); err != nil {
				slog.Warn("event processing error", "jail", jailName, "error", err)
			}
		}
	}
}

func (m *Manager) adaptInterval(execTime time.Duration, batchSize int, measuredLatency time.Duration) time.Duration {
	var target time.Duration

	switch {
	case batchSize == 0:
		target = time.Duration(float64(m.currentInterval) * 1.5)
	case measuredLatency > time.Duration(float64(m.maxLatency)*0.8):
		target = time.Duration(float64(m.currentInterval) * 0.5)
	case measuredLatency < time.Duration(float64(m.minLatency)*0.5):
		target = time.Duration(float64(m.currentInterval) * 1.25)
	default:
		target = m.currentInterval
	}

	if batchSize > 0 && execTime > m.currentInterval/2 {
		stretched := time.Duration(float64(execTime) * 2.0)
		if stretched > target {
			target = stretched
		}
	}

	next := time.Duration(
		float64(m.currentInterval)*(1-emaAlpha) + float64(target)*emaAlpha,
	)
	if next < m.minLatency {
		next = m.minLatency
	}
	if next > m.maxLatency {
		next = m.maxLatency
	}
	m.currentInterval = next
	return next
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
