package engine

import (
	"sync"
	"time"
)

// PerfSnapshot is a point-in-time view of performance metrics.
type PerfSnapshot struct {
	CurrentLatencyMs  float64 `json:"current_latency_ms"`
	CurrentIntervalMs float64 `json:"current_interval_ms"`
	AvgExecTimeMs     float64 `json:"avg_exec_time_ms"`
	AvgCPUPercent     float64 `json:"avg_cpu_percent"`
	WindowSize        int     `json:"window_size"`
}

// PerfMetrics collects performance metrics in circular buffers.
type PerfMetrics struct {
	mu sync.RWMutex

	windowSize int

	latencies    []time.Duration // circular buffer
	latencyIdx   int
	latencyCount int

	execTimes []time.Duration // circular buffer
	execIdx   int
	execCount int

	cpuSamples []float64 // circular buffer
	cpuIdx     int
	cpuCount   int

	currentInterval time.Duration
	currentLatency  time.Duration

	cpuSampler *cgroupCPUSampler
}

func NewPerfMetrics(windowSize int, serviceName string) *PerfMetrics {
	if windowSize <= 0 {
		windowSize = 1
	}
	return &PerfMetrics{
		windowSize: windowSize,
		latencies:  make([]time.Duration, windowSize),
		execTimes:  make([]time.Duration, windowSize),
		cpuSamples: make([]float64, windowSize),
		cpuSampler: newCgroupCPUSampler(serviceName),
	}
}

// RecordExecution is called after each batch drain.
// batchSize 0 means no lines were processed; CPU is not sampled in that case.
func (p *PerfMetrics) RecordExecution(execTime, currentInterval time.Duration, batchSize int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.currentInterval = currentInterval

	// Push exec time
	p.execTimes[p.execIdx%p.windowSize] = execTime
	p.execIdx++
	if p.execCount < p.windowSize {
		p.execCount++
	}

	if batchSize > 0 {
		p.currentLatency = execTime

		// Push latency
		p.latencies[p.latencyIdx%p.windowSize] = execTime
		p.latencyIdx++
		if p.latencyCount < p.windowSize {
			p.latencyCount++
		}

		// Push CPU — only sample when there was actual work to do.
		cpuPct := p.cpuSampler.Sample()
		p.cpuSamples[p.cpuIdx%p.windowSize] = cpuPct
		p.cpuIdx++
		if p.cpuCount < p.windowSize {
			p.cpuCount++
		}
	}
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
		CurrentLatencyMs:  float64(p.currentLatency.Microseconds()) / 1000.0,
		CurrentIntervalMs: float64(p.currentInterval.Microseconds()) / 1000.0,
		AvgExecTimeMs:     avgDurationMs(p.execTimes, p.execCount),
		AvgCPUPercent:     avgFloat(p.cpuSamples, p.cpuCount),
		WindowSize:        p.windowSize,
	}
}

func avgDurationMs(buf []time.Duration, count int) float64 {
	if count == 0 {
		return 0
	}
	var sum time.Duration
	for i := 0; i < count; i++ {
		sum += buf[i]
	}
	return float64(sum.Microseconds()) / 1000.0 / float64(count)
}

func avgFloat(buf []float64, count int) float64 {
	if count == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < count; i++ {
		sum += buf[i]
	}
	return sum / float64(count)
}
