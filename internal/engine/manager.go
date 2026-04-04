package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sgw/jailtime/internal/config"
	"github.com/sgw/jailtime/internal/watch"
)

// Manager runs all jail runtimes and the watch backend.
type Manager struct {
	cfg     *config.Config
	jails   map[string]*JailRuntime
	backend watch.Backend
	mu      sync.RWMutex
}

func NewManager(cfg *config.Config) (*Manager, error) {
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
		cfg:     cfg,
		jails:   jails,
		backend: backend,
	}, nil
}

// Run starts all enabled jails, starts the watch backend, routes events, and
// blocks until ctx is cancelled.
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

	events := make(chan watch.Event, 256)
	if err := m.backend.Start(ctx, specs, events); err != nil {
		return fmt.Errorf("starting watch backend: %w", err)
	}

	for {
		select {
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
				continue
			}
			go func(jr *JailRuntime, evt watch.Event) {
				_ = jr.HandleEvent(ctx, evt)
			}(jr, evt)
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

// RestartJail restarts a specific jail by name.
func (m *Manager) RestartJail(ctx context.Context, name string) error {
	m.mu.RLock()
	jr, ok := m.jails[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("jail %q not found", name)
	}
	return jr.Restart(ctx)
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
