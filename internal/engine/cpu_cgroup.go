package engine

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// cgroupCPUSampler reads CPU usage from a cgroup v2 cpu.stat file.
// If the cgroup path is unavailable, it falls back to Go runtime metrics
// via the existing cpuSampler.
type cgroupCPUSampler struct {
	cgroupPath string
	lastUsage  int64     // microseconds (usage_usec)
	lastTime   time.Time
	useCgroup  bool

	// Fallback to Go runtime metrics if cgroup unavailable.
	fallback *cpuSampler
}

// newCgroupCPUSampler creates a sampler for the given systemd service name.
// serviceName should be e.g. "jailtimed.service".
// The cgroup path used is: /sys/fs/cgroup/system.slice/<serviceName>/cpu.stat
func newCgroupCPUSampler(serviceName string) *cgroupCPUSampler {
	path := fmt.Sprintf("/sys/fs/cgroup/system.slice/%s/cpu.stat", serviceName)
	s := &cgroupCPUSampler{cgroupPath: path}

	// Prime: test if cgroup path is readable.
	if usage, err := s.readUsageUsec(); err == nil {
		s.useCgroup = true
		s.lastUsage = usage
		s.lastTime = time.Now()
	} else {
		s.useCgroup = false
		s.fallback = newCPUSampler() // existing Go runtime sampler
	}
	return s
}

// readUsageUsec reads the usage_usec field from the cgroup cpu.stat file.
func (s *cgroupCPUSampler) readUsageUsec() (int64, error) {
	f, err := os.Open(s.cgroupPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "usage_usec ") {
			val, err := strconv.ParseInt(strings.TrimPrefix(line, "usage_usec "), 10, 64)
			return val, err
		}
	}
	return 0, fmt.Errorf("usage_usec not found in %s", s.cgroupPath)
}

// Sample returns CPU usage as a percentage since the last call.
// For cgroup: percentage of a single CPU core (can exceed 100% on multi-core).
// For fallback: Go runtime CPU fraction converted to percentage.
// Returns 0 on error.
func (s *cgroupCPUSampler) Sample() float64 {
	if !s.useCgroup {
		return s.fallback.sample() * 100.0
	}

	usage, err := s.readUsageUsec()
	if err != nil {
		return 0
	}
	now := time.Now()
	elapsed := now.Sub(s.lastTime).Microseconds()
	if elapsed <= 0 {
		return 0
	}

	delta := usage - s.lastUsage
	pct := float64(delta) / float64(elapsed) * 100.0

	s.lastUsage = usage
	s.lastTime = now
	return pct
}
