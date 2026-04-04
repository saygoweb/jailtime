package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sgw/jailtime/internal/config"
	"github.com/sgw/jailtime/internal/watch"
)

// TestHitTrackerNoTrigger verifies that hits below threshold do not trigger.
func TestHitTrackerNoTrigger(t *testing.T) {
	ht := NewHitTracker()
	threshold := 3
	findTime := time.Minute
	now := time.Now()

	for i := 1; i < threshold; i++ {
		count, triggered := ht.Record("192.168.1.1", now.Add(time.Duration(i)*time.Second), findTime, threshold)
		if triggered {
			t.Fatalf("hit %d: expected triggered=false, got true (count=%d)", i, count)
		}
	}
}

// TestHitTrackerTrigger verifies that reaching threshold triggers and resets count.
func TestHitTrackerTrigger(t *testing.T) {
	ht := NewHitTracker()
	threshold := 3
	findTime := time.Minute
	now := time.Now()

	var lastCount int
	var triggered bool
	for i := 1; i <= threshold; i++ {
		lastCount, triggered = ht.Record("10.0.0.1", now.Add(time.Duration(i)*time.Second), findTime, threshold)
	}
	if !triggered {
		t.Fatalf("expected triggered=true at threshold %d, count=%d", threshold, lastCount)
	}
	if lastCount != threshold {
		t.Fatalf("expected count=%d at trigger, got %d", threshold, lastCount)
	}

	// Next hit after reset should not re-trigger with threshold=3.
	count, triggered := ht.Record("10.0.0.1", now.Add(time.Duration(threshold+1)*time.Second), findTime, threshold)
	if triggered {
		t.Fatalf("expected no retrigger after reset, got triggered=true count=%d", count)
	}
	if count != 1 {
		t.Fatalf("expected count=1 after reset, got %d", count)
	}
}

// TestHitTrackerWindowExpiry verifies that the count resets after the window expires.
func TestHitTrackerWindowExpiry(t *testing.T) {
	ht := NewHitTracker()
	threshold := 5
	findTime := 100 * time.Millisecond
	now := time.Now()

	// Record threshold-1 hits inside the window.
	for i := 0; i < threshold-1; i++ {
		_, triggered := ht.Record("172.16.0.1", now, findTime, threshold)
		if triggered {
			t.Fatal("unexpected trigger during fill phase")
		}
	}

	// Advance time past window expiry.
	expired := now.Add(findTime + 10*time.Millisecond)

	// First hit after expiry should reset to count=1.
	count, triggered := ht.Record("172.16.0.1", expired, findTime, threshold)
	if triggered {
		t.Fatalf("expected no trigger after expiry, got triggered=true count=%d", count)
	}
	if count != 1 {
		t.Fatalf("expected count=1 after expiry reset, got %d", count)
	}
}

// TestJailRuntimeHandleEvent verifies that a matching event fires on_match actions.
func TestJailRuntimeHandleEvent(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "output.txt")

	cfg := &config.JailConfig{
		Name:     "test-jail",
		Enabled:  true,
		Filters:  []string{`(?P<ip>\d+\.\d+\.\d+\.\d+)`},
		HitCount: 1,
		FindTime: config.Duration{Duration: time.Minute},
		Actions: config.JailActions{
			OnMatch: []string{"echo {{ .IP }} > " + outFile},
		},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	evt := watch.Event{
		JailName: "test-jail",
		FilePath: "/var/log/auth.log",
		Line:     "Failed password from 1.2.3.4",
		Time:     time.Now(),
	}

	ctx := context.Background()
	if err := jr.HandleEvent(ctx, evt); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("output file not created: %v", err)
	}
	got := strings.TrimSpace(string(data))
	if got != "1.2.3.4" {
		t.Fatalf("expected output %q, got %q", "1.2.3.4", got)
	}
}

// TestJailRuntimeExcludeFilter verifies that exclude filters suppress on_match.
func TestJailRuntimeExcludeFilter(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "output.txt")

	cfg := &config.JailConfig{
		Name:           "excl-jail",
		Enabled:        true,
		Filters:        []string{`(?P<ip>\d+\.\d+\.\d+\.\d+)`},
		ExcludeFilters: []string{`whitelist`},
		HitCount:       1,
		FindTime:       config.Duration{Duration: time.Minute},
		Actions: config.JailActions{
			OnMatch: []string{"echo {{ .IP }} > " + outFile},
		},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	evt := watch.Event{
		JailName: "excl-jail",
		FilePath: "/var/log/auth.log",
		Line:     "Failed password from 5.6.7.8 whitelist",
		Time:     time.Now(),
	}

	ctx := context.Background()
	if err := jr.HandleEvent(ctx, evt); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	if _, err := os.Stat(outFile); err == nil {
		t.Fatal("on_match should not have fired for excluded line, but output file exists")
	}
}
