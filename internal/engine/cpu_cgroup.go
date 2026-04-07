package engine

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

// cgroupCPUSampler reads CPU usage from a cgroup v2 cpu.stat file.
// If the cgroup path is unavailable, it falls back to Go runtime metrics
// via the existing cpuSampler.
type cgroupCPUSampler struct {
	cgroupPath string
	file       *os.File  // kept open; nil if unavailable
	buf        [512]byte // pre-allocated read buffer
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

	f, err := os.Open(path)
	if err == nil {
		s.file = f
		if usage, err := s.readUsageUsec(); err == nil {
			s.useCgroup = true
			s.lastUsage = usage
			s.lastTime = time.Now()
		} else {
			f.Close()
			s.file = nil
			s.fallback = newCPUSampler()
		}
	} else {
		s.fallback = newCPUSampler()
	}
	return s
}

// readUsageUsec reads the usage_usec field from the cgroup cpu.stat file
// using seek + read + manual parse — no Scanner, no allocation per call.
func (s *cgroupCPUSampler) readUsageUsec() (int64, error) {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := s.file.Read(s.buf[:])
	if err != nil && err != io.EOF {
		return 0, err
	}
	data := s.buf[:n]
	const prefix = "usage_usec "
	idx := bytes.Index(data, []byte(prefix))
	if idx < 0 {
		return 0, fmt.Errorf("usage_usec not found")
	}
	start := idx + len(prefix)
	end := start
	for end < len(data) && data[end] >= '0' && data[end] <= '9' {
		end++
	}
	return strconv.ParseInt(string(data[start:end]), 10, 64)
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

// Close closes the underlying cgroup file descriptor if one is held open.
func (s *cgroupCPUSampler) Close() error {
	if s.file != nil {
		return s.file.Close()
	}
	return nil
}
