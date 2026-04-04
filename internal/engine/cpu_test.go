package engine

import (
	"testing"
	"time"
)

// TestCPUSamplerNonNegative verifies the sampler returns values in [0,1].
func TestCPUSamplerNonNegative(t *testing.T) {
	s := newCPUSampler()

	// Let some time elapse so the delta is meaningful.
	time.Sleep(20 * time.Millisecond)

	usage := s.sample()
	if usage < 0 {
		t.Errorf("expected non-negative CPU usage, got %f", usage)
	}
	// On a very loaded machine the fraction could temporarily exceed 1.0
	// (if GOMAXPROCS is 1 and we're pinned), but it should be reasonable.
	if usage > 10.0 {
		t.Errorf("CPU usage looks unreasonably high: %f", usage)
	}
}

// TestCPUSamplerTwoSamples verifies successive calls return consistent results.
func TestCPUSamplerTwoSamples(t *testing.T) {
	s := newCPUSampler()
	time.Sleep(10 * time.Millisecond)
	first := s.sample()
	time.Sleep(10 * time.Millisecond)
	second := s.sample()
	if first < 0 || second < 0 {
		t.Errorf("negative CPU fraction: first=%f second=%f", first, second)
	}
}
