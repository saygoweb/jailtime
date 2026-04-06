package engine

import (
	"sync"
	"time"
)

// PerfSnapshot is a point-in-time view of performance metrics.
type PerfSnapshot struct {
	CurrentLatencyMs float64 `json:"current_latency_ms"`
	CurrentDelayMs   float64 `json:"current_delay_ms"`
	AvgExecTimeMs    float64 `json:"avg_exec_time_ms"`
	AvgCPUPercent    float64 `json:"avg_cpu_percent"`
	WindowSize       int     `json:"window_size"`
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

	currentDelay   time.Duration
	currentLatency time.Duration

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
// batchSize 0 means the queue was empty; latency is not recorded in that case.
func (p *PerfMetrics) RecordExecution(execTime, measuredLatency time.Duration, batchSize int, currentDelay time.Duration) {
	cpuPct := p.cpuSampler.Sample()

	p.mu.Lock()
	defer p.mu.Unlock()

	p.currentDelay = currentDelay
	if batchSize > 0 {
		p.currentLatency = measuredLatency
	}

	// Push exec time
	p.execTimes[p.execIdx%p.windowSize] = execTime
	p.execIdx++
	if p.execCount < p.windowSize {
		p.execCount++
	}

	// Push latency (only when batch had items)
	if batchSize > 0 {
		p.latencies[p.latencyIdx%p.windowSize] = measuredLatency
		p.latencyIdx++
		if p.latencyCount < p.windowSize {
			p.latencyCount++
		}
	}

	// Push CPU
	p.cpuSamples[p.cpuIdx%p.windowSize] = cpuPct
	p.cpuIdx++
	if p.cpuCount < p.windowSize {
		p.cpuCount++
	}
}

// Snapshot returns a point-in-time view of performance metrics.
func (p *PerfMetrics) Snapshot() PerfSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return PerfSnapshot{
		CurrentLatencyMs: float64(p.currentLatency.Microseconds()) / 1000.0,
		CurrentDelayMs:   float64(p.currentDelay.Microseconds()) / 1000.0,
		AvgExecTimeMs:    avgDurationMs(p.execTimes, p.execCount),
		AvgCPUPercent:    avgFloat(p.cpuSamples, p.cpuCount),
		WindowSize:       p.windowSize,
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
