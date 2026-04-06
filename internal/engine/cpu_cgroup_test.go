package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCgroupCPUSamplerFallback(t *testing.T) {
	// Non-existent service → should fall back to Go runtime sampler.
	s := newCgroupCPUSampler("nonexistent-service-xyz.service")
	if s.useCgroup {
		t.Error("expected fallback when cgroup path doesn't exist")
	}
	if s.fallback == nil {
		t.Error("expected fallback sampler to be initialized")
	}
	// Sample should return a non-negative value (may be 0 on first call).
	v := s.Sample()
	if v < 0 {
		t.Errorf("expected non-negative CPU percentage, got %f", v)
	}
}

func TestReadUsageUsec(t *testing.T) {
	// Create a mock cpu.stat file.
	dir := t.TempDir()
	path := filepath.Join(dir, "cpu.stat")
	content := "usage_usec 123456789\nuser_usec 100000000\nsystem_usec 23456789\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	s := &cgroupCPUSampler{cgroupPath: path}
	usage, err := s.readUsageUsec()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage != 123456789 {
		t.Errorf("expected 123456789, got %d", usage)
	}
}

func TestCgroupCPUSamplerDelta(t *testing.T) {
	// Create a mock cpu.stat that we can update.
	dir := t.TempDir()
	path := filepath.Join(dir, "cpu.stat")

	writeUsage := func(usec int64) {
		content := fmt.Sprintf("usage_usec %d\n", usec)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Prime with initial value.
	writeUsage(1000000) // 1 second of CPU time
	s := &cgroupCPUSampler{cgroupPath: path}
	usage, err := s.readUsageUsec()
	if err != nil {
		t.Fatal(err)
	}
	s.useCgroup = true
	s.lastUsage = usage
	s.lastTime = time.Now()

	// Advance: simulate 500ms wall time, 100ms CPU usage.
	time.Sleep(50 * time.Millisecond)
	writeUsage(1100000) // added 100ms CPU time

	pct := s.Sample()
	// The exact value depends on wall-clock elapsed, but it should be positive.
	if pct <= 0 {
		t.Errorf("expected positive CPU percentage, got %f", pct)
	}
}
