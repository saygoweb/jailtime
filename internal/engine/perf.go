package engine

import (
	"sync"
	"time"
)

// PerfSnapshot is a point-in-time view of performance metrics.
type PerfSnapshot struct {
	TargetLatencyMs float64 `json:"target_latency_ms"`
	LatencyMs       float64 `json:"latency_ms"`
	InterDrainMs    float64 `json:"inter_drain_ms"`
	ExecutionMs     float64 `json:"execution_ms"`
	SleepMs         float64 `json:"sleep_ms"`
	LinesProcessed  int     `json:"lines_processed"`
	CPUPercent      float64 `json:"cpu_percent"`
}

// PerfMetrics collects performance metrics.
type PerfMetrics struct {
	mu sync.RWMutex

	targetLatency time.Duration

	lastLatency   time.Duration
	lastInterDrain time.Duration
	lastExec      time.Duration
	lastSleep     time.Duration
	lastLines     int

	// execWindow is a ring buffer holding the last perfWindow execution times.
	execWindow []time.Duration
	windowIdx  int
	windowFull bool

	cpuSampler *cgroupCPUSampler
	lastCPU    float64
}

func NewPerfMetrics(targetLatency time.Duration, perfWindow int, serviceName string) *PerfMetrics {
	if perfWindow < 1 {
		perfWindow = 1
	}
	return &PerfMetrics{
		targetLatency: targetLatency,
		execWindow:    make([]time.Duration, perfWindow),
		cpuSampler:    newCgroupCPUSampler(serviceName),
	}
}

// RecordExecution is called after each batch drain.
// latency is the time from the first event trigger to drain start.
// interDrain is the elapsed time between this drain and the previous one.
// sleepTime is the intended sleep before the next drain.
// batchSize 0 means no lines were processed; CPU is not sampled in that case.
func (p *PerfMetrics) RecordExecution(execTime, latency, interDrain, sleepTime time.Duration, batchSize int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.lastExec = execTime
	p.lastLatency = latency
	p.lastInterDrain = interDrain
	p.lastSleep = sleepTime
	p.lastLines = batchSize

	// Update the ring buffer with the latest execution time.
	p.execWindow[p.windowIdx] = execTime
	p.windowIdx++
	if p.windowIdx >= len(p.execWindow) {
		p.windowIdx = 0
		p.windowFull = true
	}

	if batchSize > 0 {
		p.lastCPU = p.cpuSampler.Sample()
	}
}

// SetTargetLatency updates the target latency displayed in performance snapshots.
func (p *PerfMetrics) SetTargetLatency(d time.Duration) {
	p.mu.Lock()
	p.targetLatency = d
	p.mu.Unlock()
}

// MovingAvgExec returns the moving average of execution times over the window.
// Must be called without p.mu held.
func (p *PerfMetrics) MovingAvgExec() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.movingAvgExecLocked()
}

// movingAvgExecLocked returns the moving average of execution times.
// Must be called with p.mu held (at least RLock).
func (p *PerfMetrics) movingAvgExecLocked() time.Duration {
	n := len(p.execWindow)
	if n == 0 {
		return 0
	}
	count := n
	if !p.windowFull {
		count = p.windowIdx
	}
	if count == 0 {
		return 0
	}
	var total time.Duration
	for _, d := range p.execWindow {
		total += d
	}
	return total / time.Duration(n)
}

// IntendedSleep returns the sleep duration that steers total cycle time
// towards targetLatency: targetLatency minus the moving-average execution
// time, clamped to [0, targetLatency].
func (p *PerfMetrics) IntendedSleep() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()

	sleep := p.targetLatency - p.movingAvgExecLocked()
	if sleep < 0 {
		return 0
	}
	if sleep > p.targetLatency {
		return p.targetLatency
	}
	return sleep
}

// Close releases resources held by the PerfMetrics (e.g. the open cgroup fd).
func (p *PerfMetrics) Close() {
	_ = p.cpuSampler.Close()
}

// Snapshot returns a point-in-time view of performance metrics.
func (p *PerfMetrics) Snapshot() PerfSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return PerfSnapshot{
		TargetLatencyMs: float64(p.targetLatency.Microseconds()) / 1000.0,
		LatencyMs:       float64(p.lastLatency.Microseconds()) / 1000.0,
		InterDrainMs:    float64(p.lastInterDrain.Microseconds()) / 1000.0,
		ExecutionMs:     float64(p.lastExec.Microseconds()) / 1000.0,
		SleepMs:         float64(p.lastSleep.Microseconds()) / 1000.0,
		LinesProcessed:  p.lastLines,
		CPUPercent:      p.lastCPU,
	}
}
