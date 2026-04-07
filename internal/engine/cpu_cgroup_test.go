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

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := &cgroupCPUSampler{cgroupPath: path, file: f}
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
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := &cgroupCPUSampler{cgroupPath: path, file: f}
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

func TestCgroupCPUSamplerNoAlloc(t *testing.T) {
	// Verify that Sample() performs zero (or minimal) heap allocations.
	dir := t.TempDir()
	path := filepath.Join(dir, "cpu.stat")
	content := "usage_usec 1000000\nuser_usec 500000\nsystem_usec 500000\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	s := &cgroupCPUSampler{cgroupPath: path, file: f}
	s.useCgroup = true
	s.lastUsage = 1000000
	s.lastTime = time.Now()

	// Let some time pass so delta calculation works.
	time.Sleep(10 * time.Millisecond)

	// Run Sample() once to warm up any lazy allocations.
	_ = s.Sample()

	// Now measure allocations on the second call.
	allocs := testing.AllocsPerRun(100, func() {
		_ = s.Sample()
	})

	// We allow ≤1 allocation per run for the string conversion in strconv.ParseInt.
	// Ideally it would be 0, but 1 is acceptable for this optimization.
	if allocs > 1 {
		t.Errorf("Sample() allocates too much: got %.2f allocs/op, want ≤1", allocs)
	}
}
