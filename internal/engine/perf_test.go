package engine

import (
	"testing"
	"time"
)

func TestPerfMetrics_SnapshotLastValues(t *testing.T) {
	p := NewPerfMetrics(2*time.Second, "nonexistent-service-for-test.service")

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
	p := NewPerfMetrics(2*time.Second, "nonexistent-service-for-test.service")
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
	p := NewPerfMetrics(2*time.Second, "no-such-service-xyz-123.service")
	p.RecordExecution(1*time.Millisecond, 10*time.Millisecond, 9*time.Millisecond, 1)
	snap := p.Snapshot()
	if snap.TargetLatencyMs != 2000.0 {
		t.Errorf("TargetLatencyMs = %v, want 2000.0", snap.TargetLatencyMs)
	}
}
