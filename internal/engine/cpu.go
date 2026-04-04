package engine

import (
	"runtime"
	"runtime/metrics"
	"time"
)

// cpuSampler estimates this process's CPU utilisation between calls to sample().
// It measures the fraction of total available CPU (across all GOMAXPROCS cores)
// consumed by this Go process, including GC and all goroutines.
type cpuSampler struct {
	samples  []metrics.Sample
	lastSec  float64
	lastTime time.Time
}

func newCPUSampler() *cpuSampler {
	s := &cpuSampler{
		samples: []metrics.Sample{
			{Name: "/cpu/classes/user:cpu-seconds"},
			{Name: "/cpu/classes/system:cpu-seconds"},
		},
	}
	// Prime the sampler so the first real call returns a meaningful delta.
	metrics.Read(s.samples)
	s.lastSec = s.totalSec()
	s.lastTime = time.Now()
	return s
}

func (s *cpuSampler) totalSec() float64 {
	var total float64
	for _, m := range s.samples {
		if m.Value.Kind() == metrics.KindFloat64 {
			total += m.Value.Float64()
		}
	}
	return total
}

// sample returns the fraction of total available CPU used since the last call.
// The value is in the range [0, 1] where 1.0 means all GOMAXPROCS cores were
// fully occupied. Returns 0 if the elapsed time since the last call is zero.
func (s *cpuSampler) sample() float64 {
	metrics.Read(s.samples)
	now := time.Now()

	elapsed := now.Sub(s.lastTime).Seconds()
	if elapsed <= 0 {
		return 0
	}

	delta := s.totalSec() - s.lastSec
	usage := delta / elapsed / float64(runtime.GOMAXPROCS(0))

	s.lastSec = s.totalSec()
	s.lastTime = now
	return usage
}
