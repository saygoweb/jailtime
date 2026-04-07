package engine

import (
	"testing"
	"time"
)

func TestPerfMetrics_SnapshotAverages(t *testing.T) {
	p := NewPerfMetrics(5, "nonexistent-service-for-test.service")

	execTimes := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond}

	// Push exec times; first two with batchSize=1 (records latency/CPU), third is idle.
	for i, et := range execTimes {
		bs := 0
		if i < 2 {
			bs = 1
		}
		p.RecordExecution(et, 100*time.Millisecond, bs)
	}

	snap := p.Snapshot()

	wantAvgExec := 20.0 // (10+20+30)/3
	if snap.AvgExecTimeMs != wantAvgExec {
		t.Errorf("AvgExecTimeMs = %v, want %v", snap.AvgExecTimeMs, wantAvgExec)
	}

	wantInterval := 100.0
	if snap.CurrentIntervalMs != wantInterval {
		t.Errorf("CurrentIntervalMs = %v, want %v", snap.CurrentIntervalMs, wantInterval)
	}

	if snap.WindowSize != 5 {
		t.Errorf("WindowSize = %v, want 5", snap.WindowSize)
	}
}

func TestPerfMetrics_CircularBufferWrapping(t *testing.T) {
	const windowSize = 3
	p := NewPerfMetrics(windowSize, "nonexistent-service-for-test.service")

	// Push windowSize+2 items: 10, 20, 30, 40, 50 ms
	values := []time.Duration{10, 20, 30, 40, 50}
	for _, v := range values {
		p.RecordExecution(v*time.Millisecond, 0, 0)
	}

	// After wrapping with windowSize=3 and 5 items:
	//   idx=0: 10 → overwritten by idx=3 (40)  → slot 0 = 40
	//   idx=1: 20 → overwritten by idx=4 (50)  → slot 1 = 50
	//   idx=2: 30                               → slot 2 = 30
	// avg of buf[0..2] = (40+50+30)/3 = 40ms
	snap := p.Snapshot()
	wantAvg := 40.0
	if snap.AvgExecTimeMs != wantAvg {
		t.Errorf("AvgExecTimeMs after wrap = %v, want %v", snap.AvgExecTimeMs, wantAvg)
	}
}

func TestPerfMetrics_BatchSizeZeroSkipsLatency(t *testing.T) {
	p := NewPerfMetrics(5, "nonexistent-service-for-test.service")

	// Call with batchSize=0 — latency and CPU should not be recorded
	p.RecordExecution(5*time.Millisecond, 50*time.Millisecond, 0)

	snap := p.Snapshot()

	// currentLatency was never set (batchSize was 0)
	if snap.CurrentLatencyMs != 0 {
		t.Errorf("CurrentLatencyMs = %v, want 0 (batchSize was 0)", snap.CurrentLatencyMs)
	}

	// exec time was still recorded
	if snap.AvgExecTimeMs != 5.0 {
		t.Errorf("AvgExecTimeMs = %v, want 5.0", snap.AvgExecTimeMs)
	}

	// latency buffer count should be 0; verify via internal field
	p.mu.RLock()
	lc := p.latencyCount
	p.mu.RUnlock()
	if lc != 0 {
		t.Errorf("latencyCount = %d, want 0", lc)
	}
}

func TestPerfMetrics_UnavailableCgroupNoPanic(t *testing.T) {
	// Must not panic even when cgroup path doesn't exist
	p := NewPerfMetrics(10, "no-such-service-xyz-123.service")
	p.RecordExecution(1*time.Millisecond, 10*time.Millisecond, 1)
	snap := p.Snapshot()
	if snap.WindowSize != 10 {
		t.Errorf("WindowSize = %v, want 10", snap.WindowSize)
	}
}
