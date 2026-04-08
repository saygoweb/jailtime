package engine

import (
	"testing"
	"time"
)

func TestPerfMetrics_SnapshotLastValues(t *testing.T) {
	p := NewPerfMetrics(2*time.Second, 3, "nonexistent-service-for-test.service")

	p.RecordExecution(10*time.Millisecond, 100*time.Millisecond, 90*time.Millisecond, 5)
	p.RecordExecution(30*time.Millisecond, 120*time.Millisecond, 95*time.Millisecond, 8)

	snap := p.Snapshot()

	if snap.TargetLatencyMs != 2000.0 {
		t.Errorf("TargetLatencyMs = %v, want 2000.0", snap.TargetLatencyMs)
	}
	if snap.ExecutionMs != 30.0 {
		t.Errorf("ExecutionMs = %v, want 30.0 (last value)", snap.ExecutionMs)
	}
	if snap.LatencyMs != 120.0 {
		t.Errorf("LatencyMs = %v, want 120.0 (last value)", snap.LatencyMs)
	}
	if snap.SleepMs != 95.0 {
		t.Errorf("SleepMs = %v, want 95.0 (last value)", snap.SleepMs)
	}
	if snap.LinesProcessed != 8 {
		t.Errorf("LinesProcessed = %v, want 8 (last value)", snap.LinesProcessed)
	}
}

func TestPerfMetrics_ZeroBeforeFirstRecord(t *testing.T) {
	p := NewPerfMetrics(2*time.Second, 3, "nonexistent-service-for-test.service")
	snap := p.Snapshot()

	if snap.ExecutionMs != 0 {
		t.Errorf("ExecutionMs = %v before any record, want 0", snap.ExecutionMs)
	}
	if snap.LatencyMs != 0 {
		t.Errorf("LatencyMs = %v before any record, want 0", snap.LatencyMs)
	}
	if snap.LinesProcessed != 0 {
		t.Errorf("LinesProcessed = %v before any record, want 0", snap.LinesProcessed)
	}
}

func TestPerfMetrics_UnavailableCgroupNoPanic(t *testing.T) {
	// Must not panic even when cgroup path doesn't exist.
	p := NewPerfMetrics(2*time.Second, 3, "no-such-service-xyz-123.service")
	p.RecordExecution(1*time.Millisecond, 10*time.Millisecond, 9*time.Millisecond, 1)
	snap := p.Snapshot()
	if snap.TargetLatencyMs != 2000.0 {
		t.Errorf("TargetLatencyMs = %v, want 2000.0", snap.TargetLatencyMs)
	}
}

// TestPerfMetrics_IntendedSleepNeverExceedsTarget verifies that IntendedSleep
// is always <= targetLatency, even when execution time is zero.
func TestPerfMetrics_IntendedSleepNeverExceedsTarget(t *testing.T) {
	target := 2 * time.Second
	p := NewPerfMetrics(target, 3, "nonexistent-service-for-test.service")

	// Before any recording, sleep should equal targetLatency (avg exec = 0).
	sleep := p.IntendedSleep()
	if sleep > target {
		t.Errorf("IntendedSleep = %v, want <= %v before any record", sleep, target)
	}
	if sleep != target {
		t.Errorf("IntendedSleep = %v, want %v when no exec recorded", sleep, target)
	}

	// After recording a fast execution, sleep should be slightly below target.
	p.RecordExecution(10*time.Millisecond, target, target-10*time.Millisecond, 5)
	sleep = p.IntendedSleep()
	if sleep > target {
		t.Errorf("IntendedSleep = %v exceeds targetLatency %v", sleep, target)
	}

	// After recording a slow execution (larger than target), sleep should be 0.
	p2 := NewPerfMetrics(50*time.Millisecond, 1, "nonexistent-service-for-test.service")
	p2.RecordExecution(100*time.Millisecond, 100*time.Millisecond, 0, 1)
	sleep2 := p2.IntendedSleep()
	if sleep2 != 0 {
		t.Errorf("IntendedSleep = %v, want 0 when exec > target", sleep2)
	}
}

// TestPerfMetrics_MovingAvgWindow verifies that the ring buffer averages over
// the last perfWindow samples.
func TestPerfMetrics_MovingAvgWindow(t *testing.T) {
	// Window size 3; record 4 values: 10, 20, 30, 40ms. After 4 records the
	// ring buffer holds [40, 20, 30] (oldest entry overwritten), avg = 30ms.
	p := NewPerfMetrics(2*time.Second, 3, "nonexistent-service-for-test.service")
	for _, d := range []time.Duration{10, 20, 30, 40} {
		p.RecordExecution(d*time.Millisecond, 2*time.Second, 0, 1)
	}

	avg := p.MovingAvgExec()
	// Window contains [40, 20, 30] → average = 90/3 = 30ms.
	want := 30 * time.Millisecond
	if avg != want {
		t.Errorf("MovingAvgExec = %v, want %v", avg, want)
	}
}
