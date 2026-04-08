package engine

import (
	"sync"
	"time"
)

// PerfSnapshot is a point-in-time view of performance metrics.
type PerfSnapshot struct {
	TargetLatencyMs float64 `json:"target_latency_ms"`
	LatencyMs       float64 `json:"latency_ms"`
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
	lastExec      time.Duration
	lastSleep     time.Duration
	lastLines     int

	cpuSampler *cgroupCPUSampler
	lastCPU    float64
}

func NewPerfMetrics(targetLatency time.Duration, serviceName string) *PerfMetrics {
	return &PerfMetrics{
		targetLatency: targetLatency,
		cpuSampler:    newCgroupCPUSampler(serviceName),
	}
}

// RecordExecution is called after each batch drain.
// sleepTime is the duration the backend slept before this drain.
// batchSize 0 means no lines were processed; CPU is not sampled in that case.
func (p *PerfMetrics) RecordExecution(execTime, latency, sleepTime time.Duration, batchSize int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.lastExec = execTime
	p.lastLatency = latency
	p.lastSleep = sleepTime
	p.lastLines = batchSize

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
		ExecutionMs:     float64(p.lastExec.Microseconds()) / 1000.0,
		SleepMs:         float64(p.lastSleep.Microseconds()) / 1000.0,
		LinesProcessed:  p.lastLines,
		CPUPercent:      p.lastCPU,
	}
}
