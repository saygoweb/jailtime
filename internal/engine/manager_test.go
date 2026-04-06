package engine

import (
	"testing"
	"time"
)

// newTestManager returns a Manager configured for interval-adaptation tests.
func newTestManager(minLatency, maxLatency time.Duration) *Manager {
	return &Manager{
		minLatency:      minLatency,
		maxLatency:      maxLatency,
		currentInterval: minLatency,
	}
}

// TestAdaptInterval_IdleCapAt2xMin verifies that repeated idle cycles
// (batchSize==0) do not grow the interval beyond 2× minLatency.
func TestAdaptInterval_IdleCapAt2xMin(t *testing.T) {
	m := newTestManager(2*time.Second, 10*time.Second)
	for i := 0; i < 100; i++ {
		m.adaptInterval(0, 0, 0)
	}
	cap := 2 * m.minLatency
	if m.currentInterval > cap {
		t.Errorf("currentInterval = %v after 100 idle cycles, want ≤ %v", m.currentInterval, cap)
	}
}

// TestAdaptInterval_HighLatencySnapToMin verifies that when latency is
// significantly above minLatency the interval snaps back in one cycle.
func TestAdaptInterval_HighLatencySnapToMin(t *testing.T) {
	m := newTestManager(2*time.Second, 10*time.Second)
	m.currentInterval = 7500 * time.Millisecond // simulates drifted interval

	got := m.adaptInterval(1*time.Millisecond, 3, 7*time.Second) // latency=7s >> 1.5×min=3s
	if got != m.minLatency {
		t.Errorf("interval after high-latency snap = %v, want %v", got, m.minLatency)
	}
	if m.currentInterval != m.minLatency {
		t.Errorf("currentInterval = %v after snap, want %v", m.currentInterval, m.minLatency)
	}
}

// TestAdaptInterval_ModeratelyHighLatencyReduces verifies that latency just
// above minLatency (but below 1.5× threshold) is reduced via EMA toward min.
func TestAdaptInterval_ModeratelyHighLatencyReduces(t *testing.T) {
	m := newTestManager(2*time.Second, 10*time.Second)
	m.currentInterval = 3 * time.Second // slightly above min
	before := m.currentInterval

	// latency=2.5s > minLatency=2s but < 1.5×min=3s → EMA reduction toward min
	got := m.adaptInterval(1*time.Millisecond, 1, 2500*time.Millisecond)
	if got >= before {
		t.Errorf("interval should decrease from %v when latency>min, got %v", before, got)
	}
	if got < m.minLatency {
		t.Errorf("interval should not go below minLatency, got %v", got)
	}
}

// TestAdaptInterval_LowLatencyGrows verifies that very low latency
// (< 50% of minLatency) causes the interval to grow.
func TestAdaptInterval_LowLatencyGrows(t *testing.T) {
	m := newTestManager(2*time.Second, 10*time.Second)
	before := m.currentInterval

	got := m.adaptInterval(1*time.Millisecond, 1, 500*time.Millisecond) // latency=0.5s < 1s
	if got <= before {
		t.Errorf("interval should grow when latency is very low, got %v (was %v)", got, before)
	}
}

// TestAdaptInterval_NeverExceedsMaxLatency verifies that even extreme exec
// times cannot push the interval above maxLatency.
func TestAdaptInterval_NeverExceedsMaxLatency(t *testing.T) {
	m := newTestManager(2*time.Second, 10*time.Second)

	// exec time 30s would naively stretch to 60s
	got := m.adaptInterval(30*time.Second, 5, 3*time.Second)
	if got > m.maxLatency {
		t.Errorf("interval %v exceeds maxLatency %v", got, m.maxLatency)
	}
}

// TestAdaptInterval_NeverBelowMinLatency verifies the lower bound is respected.
func TestAdaptInterval_NeverBelowMinLatency(t *testing.T) {
	m := newTestManager(2*time.Second, 10*time.Second)

	got := m.adaptInterval(1*time.Millisecond, 1, 100*time.Millisecond)
	if got < m.minLatency {
		t.Errorf("interval %v is below minLatency %v", got, m.minLatency)
	}
}

// TestAdaptInterval_IdleThenBusyRecovery simulates the production scenario:
// idle cycles drift the interval up, then a burst of items with high latency
// arrives and the interval should recover to minLatency in one step.
func TestAdaptInterval_IdleThenBusyRecovery(t *testing.T) {
	m := newTestManager(2*time.Second, 10*time.Second)

	// Simulate idle drift (interval should be capped at 2×min = 4s)
	for i := 0; i < 50; i++ {
		m.adaptInterval(0, 0, 0)
	}
	if m.currentInterval > 2*m.minLatency {
		t.Fatalf("interval drifted to %v before burst, expected ≤ %v", m.currentInterval, 2*m.minLatency)
	}

	// Now a batch arrives with latency = current interval (items waited the full cycle)
	latency := m.currentInterval
	got := m.adaptInterval(1*time.Millisecond, 5, latency)

	// latency should be > 1.5×min (3s) since idle cap is 4s, so snap should occur
	if latency > time.Duration(float64(m.minLatency)*1.5) {
		if got != m.minLatency {
			t.Errorf("expected snap to minLatency=%v after high-latency burst, got %v", m.minLatency, got)
		}
	} else {
		// latency ≤ 1.5×min: still should reduce toward min
		if got >= latency {
			t.Errorf("expected interval to reduce toward min, got %v (was %v)", got, latency)
		}
	}
}
