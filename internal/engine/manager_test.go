package engine

import (
	"context"
	"testing"
	"time"

	"github.com/sgw/jailtime/internal/watch"
)

// TestManagerCurrentInterval verifies that processDrain measures currentInterval
// as the elapsed time between successive drain calls.
func TestManagerCurrentInterval(t *testing.T) {
	m := &Manager{
		perf: NewPerfMetrics(3, ""),
	}

	ctx := context.Background()
	first := time.Now()

	// First processDrain call: lastDrainAt is zero so currentInterval is not set.
	m.processDrain(ctx, []watch.RawLine{})
	if m.lastDrainAt.IsZero() {
		t.Fatal("lastDrainAt should be set after first processDrain")
	}

	// Simulate a gap of ~50ms.
	time.Sleep(50 * time.Millisecond)

	// Second processDrain call: currentInterval should reflect elapsed time.
	m.processDrain(ctx, []watch.RawLine{})
	elapsed := time.Since(first)

	if m.currentInterval <= 0 {
		t.Fatalf("currentInterval should be > 0 after second processDrain, got %v", m.currentInterval)
	}
	if m.currentInterval > elapsed+10*time.Millisecond {
		t.Errorf("currentInterval %v seems too large (elapsed %v)", m.currentInterval, elapsed)
	}
}
