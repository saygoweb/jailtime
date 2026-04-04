package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"context"
)

func startBackend(t *testing.T, b Backend, specs []WatchSpec) (chan Event, context.CancelFunc) {
	t.Helper()
	out := make(chan Event, 16)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = b.Start(ctx, specs, out)
	}()
	return out, cancel
}

func waitEvent(out chan Event, timeout time.Duration) (Event, bool) {
	select {
	case ev := <-out:
		return ev, true
	case <-time.After(timeout):
		return Event{}, false
	}
}

// TestPollBackendBasic creates a temp file, starts poll backend, appends a line,
// and verifies the Event is received with correct fields.
func TestPollBackendBasic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	b := NewPollBackend(100 * time.Millisecond)
	specs := []WatchSpec{{JailName: "jail1", Globs: []string{path}, ReadFromEnd: false}}
	out, cancel := startBackend(t, b, specs)
	defer cancel()

	// Give the backend a moment to initialize.
	time.Sleep(150 * time.Millisecond)

	af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(af, "hello world")
	af.Close()

	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for event")
	}
	if ev.Line != "hello world" {
		t.Errorf("expected line %q, got %q", "hello world", ev.Line)
	}
	if ev.JailName != "jail1" {
		t.Errorf("expected JailName %q, got %q", "jail1", ev.JailName)
	}
	if ev.FilePath != path {
		t.Errorf("expected FilePath %q, got %q", path, ev.FilePath)
	}
}

// TestPollBackendNewFile starts backend watching a glob, creates a new matching
// file, writes to it, and verifies the event is received.
func TestPollBackendNewFile(t *testing.T) {
	dir := t.TempDir()
	pattern := filepath.Join(dir, "*.log")

	b := NewPollBackend(100 * time.Millisecond)
	specs := []WatchSpec{{JailName: "jail2", Globs: []string{pattern}, ReadFromEnd: true}}
	out, cancel := startBackend(t, b, specs)
	defer cancel()

	time.Sleep(150 * time.Millisecond)

	path := filepath.Join(dir, "new.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Give the backend time to discover the file before writing.
	time.Sleep(200 * time.Millisecond)
	fmt.Fprintln(f, "new file line")
	f.Close()

	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for event from new file")
	}
	if ev.Line != "new file line" {
		t.Errorf("expected %q, got %q", "new file line", ev.Line)
	}
	if ev.FilePath != path {
		t.Errorf("expected FilePath %q, got %q", path, ev.FilePath)
	}
}

// TestPollBackendRotation starts backend on a file, renames it (rotation),
// creates a new file at the same path, writes a line, and verifies the event.
func TestPollBackendRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rotate.log")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, "old line")
	f.Close()

	b := NewPollBackend(100 * time.Millisecond)
	specs := []WatchSpec{{JailName: "jail3", Globs: []string{path}, ReadFromEnd: false}}
	out, cancel := startBackend(t, b, specs)
	defer cancel()

	// Drain the old line event.
	time.Sleep(150 * time.Millisecond)
	drainTimeout := time.After(500 * time.Millisecond)
drain:
	for {
		select {
		case <-out:
		case <-drainTimeout:
			break drain
		}
	}

	// Rotate: rename old file, create new one.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	newF, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(newF, "rotated line")
	newF.Close()

	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for event after rotation")
	}
	if ev.Line != "rotated line" {
		t.Errorf("expected %q, got %q", "rotated line", ev.Line)
	}
}

// TestPollBackendSubdirGlob verifies that a glob with a wildcard subdirectory
// component (e.g. apache2/*/access.log) picks up log files in new subdirectories
// created after the backend is already running.
func TestPollBackendSubdirGlob(t *testing.T) {
	base := t.TempDir()
	// Pattern: base/*/access.log — matches access.log in any direct subdirectory.
	pattern := filepath.Join(base, "*", "access.log")

	b := NewPollBackend(100 * time.Millisecond)
	specs := []WatchSpec{{JailName: "apache", Globs: []string{pattern}, ReadFromEnd: true}}
	out, cancel := startBackend(t, b, specs)
	defer cancel()

	// Let the backend complete at least one poll cycle before creating anything.
	time.Sleep(150 * time.Millisecond)

	// Create a new subdirectory and log file *after* the backend has started.
	subdir := filepath.Join(base, "site1")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(subdir, "access.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the backend to discover the new file via glob rescan.
	time.Sleep(200 * time.Millisecond)
	fmt.Fprintln(f, "192.168.1.1 GET /index.html 200")
	f.Close()

	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out: poll backend did not pick up file in new subdirectory")
	}
	if ev.Line != "192.168.1.1 GET /index.html 200" {
		t.Errorf("expected line %q, got %q", "192.168.1.1 GET /index.html 200", ev.Line)
	}
	if ev.FilePath != logPath {
		t.Errorf("expected FilePath %q, got %q", logPath, ev.FilePath)
	}
	if ev.JailName != "apache" {
		t.Errorf("expected JailName %q, got %q", "apache", ev.JailName)
	}
}

// TestPollBackendSubdirGlobMultiple verifies that a subdirectory glob continues
// to pick up files in additional new subdirectories added over time.
func TestPollBackendSubdirGlobMultiple(t *testing.T) {
	base := t.TempDir()
	pattern := filepath.Join(base, "*", "access.log")

	b := NewPollBackend(100 * time.Millisecond)
	specs := []WatchSpec{{JailName: "apache2", Globs: []string{pattern}, ReadFromEnd: true}}
	out, cancel := startBackend(t, b, specs)
	defer cancel()

	time.Sleep(150 * time.Millisecond)

	writeAndExpect := func(subdirName, line string) {
		t.Helper()
		subdir := filepath.Join(base, subdirName)
		if err := os.Mkdir(subdir, 0755); err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(subdir, "access.log")
		f, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
		fmt.Fprintln(f, line)
		f.Close()

		ev, ok := waitEvent(out, 2*time.Second)
		if !ok {
			t.Fatalf("timed out waiting for event from %s/access.log", subdirName)
		}
		if ev.Line != line {
			t.Errorf("subdir %s: expected line %q, got %q", subdirName, line, ev.Line)
		}
		if ev.FilePath != logPath {
			t.Errorf("subdir %s: expected FilePath %q, got %q", subdirName, logPath, ev.FilePath)
		}
	}

	writeAndExpect("vhost1", "10.0.0.1 GET /a 200")
	writeAndExpect("vhost2", "10.0.0.2 GET /b 404")
}

// TestFsnotifyBackendSubdirGlob mirrors TestPollBackendSubdirGlob for the
// fsnotify backend — new subdirectories created after startup must be picked
// up via the periodic glob rescan.
func TestFsnotifyBackendSubdirGlob(t *testing.T) {
	base := t.TempDir()
	pattern := filepath.Join(base, "*", "access.log")

	b := NewFsnotifyBackend(100 * time.Millisecond)
	specs := []WatchSpec{{JailName: "apache", Globs: []string{pattern}, ReadFromEnd: true}}
	out, cancel := startBackend(t, b, specs)
	defer cancel()

	time.Sleep(150 * time.Millisecond)

	subdir := filepath.Join(base, "site1")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(subdir, "access.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)
	fmt.Fprintln(f, "192.168.1.1 GET /index.html 200")
	f.Close()

	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out: fsnotify backend did not pick up file in new subdirectory")
	}
	if ev.Line != "192.168.1.1 GET /index.html 200" {
		t.Errorf("expected line %q, got %q", "192.168.1.1 GET /index.html 200", ev.Line)
	}
	if ev.FilePath != logPath {
		t.Errorf("expected FilePath %q, got %q", logPath, ev.FilePath)
	}
	if ev.JailName != "apache" {
		t.Errorf("expected JailName %q, got %q", "apache", ev.JailName)
	}
}

// TestFsnotifyBackendBasic mirrors TestPollBackendBasic but uses fsnotify backend.
func TestFsnotifyBackendBasic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fsnotify.log")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	b := NewFsnotifyBackend(100 * time.Millisecond)
	specs := []WatchSpec{{JailName: "jail4", Globs: []string{path}, ReadFromEnd: false}}
	out, cancel := startBackend(t, b, specs)
	defer cancel()

	time.Sleep(150 * time.Millisecond)

	af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(af, "fsnotify hello")
	af.Close()

	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for fsnotify event")
	}
	if ev.Line != "fsnotify hello" {
		t.Errorf("expected %q, got %q", "fsnotify hello", ev.Line)
	}
	if ev.JailName != "jail4" {
		t.Errorf("expected JailName %q, got %q", "jail4", ev.JailName)
	}
	if ev.FilePath != path {
		t.Errorf("expected FilePath %q, got %q", path, ev.FilePath)
	}
}

// TestInotifyBackendBasic verifies that the "inotify" mode alias creates a
// working backend on Linux (fsnotify uses inotify under the hood).
func TestInotifyBackendBasic(t *testing.T) {
dir := t.TempDir()
path := filepath.Join(dir, "inotify.log")

f, err := os.Create(path)
if err != nil {
t.Fatal(err)
}
f.Close()

b := NewAuto("inotify", 100*time.Millisecond)
specs := []WatchSpec{{JailName: "jail-inotify", Globs: []string{path}, ReadFromEnd: false}}
out, cancel := startBackend(t, b, specs)
defer cancel()

time.Sleep(150 * time.Millisecond)

af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
if err != nil {
t.Fatal(err)
}
fmt.Fprintln(af, "inotify test line")
af.Close()

ev, ok := waitEvent(out, 2*time.Second)
if !ok {
t.Fatal("timed out waiting for inotify event")
}
if ev.Line != "inotify test line" {
t.Errorf("expected line %q, got %q", "inotify test line", ev.Line)
}
if ev.JailName != "jail-inotify" {
t.Errorf("expected JailName %q, got %q", "jail-inotify", ev.JailName)
}
if ev.FilePath != path {
t.Errorf("expected FilePath %q, got %q", path, ev.FilePath)
}
}

// TestOsModeBackendBasic verifies that the "os" mode alias creates a working
// backend (resolves to the platform-native watcher).
func TestOsModeBackendBasic(t *testing.T) {
dir := t.TempDir()
path := filepath.Join(dir, "os-mode.log")

f, err := os.Create(path)
if err != nil {
t.Fatal(err)
}
f.Close()

b := NewAuto("os", 100*time.Millisecond)
specs := []WatchSpec{{JailName: "jail-os", Globs: []string{path}, ReadFromEnd: false}}
out, cancel := startBackend(t, b, specs)
defer cancel()

time.Sleep(150 * time.Millisecond)

af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
if err != nil {
t.Fatal(err)
}
fmt.Fprintln(af, "os mode test line")
af.Close()

ev, ok := waitEvent(out, 2*time.Second)
if !ok {
t.Fatal("timed out waiting for os-mode event")
}
if ev.Line != "os mode test line" {
t.Errorf("expected line %q, got %q", "os mode test line", ev.Line)
}
if ev.JailName != "jail-os" {
t.Errorf("expected JailName %q, got %q", "jail-os", ev.JailName)
}
if ev.FilePath != path {
t.Errorf("expected FilePath %q, got %q", path, ev.FilePath)
}
}

// TestPollBackendSharedFile verifies that when two jails watch the same file,
// each line is delivered as a separate event to each jail (file read once per tick).
func TestPollBackendSharedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shared.log")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	b := NewPollBackend(100 * time.Millisecond)
	specs := []WatchSpec{
		{JailName: "jail-a", Globs: []string{path}, ReadFromEnd: false},
		{JailName: "jail-b", Globs: []string{path}, ReadFromEnd: false},
	}
	out, cancel := startBackend(t, b, specs)
	defer cancel()

	time.Sleep(150 * time.Millisecond)

	af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(af, "shared line")
	af.Close()

	seen := make(map[string]bool)
	for i := 0; i < 2; i++ {
		ev, ok := waitEvent(out, 2*time.Second)
		if !ok {
			t.Fatalf("timed out waiting for event %d/2 (got jails: %v)", i+1, seen)
		}
		if ev.Line != "shared line" {
			t.Errorf("expected line %q, got %q", "shared line", ev.Line)
		}
		seen[ev.JailName] = true
	}
	if !seen["jail-a"] || !seen["jail-b"] {
		t.Errorf("expected events for both jails, got: %v", seen)
	}
}

// TestFsnotifyBackendSharedFile mirrors TestPollBackendSharedFile for fsnotify.
func TestFsnotifyBackendSharedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shared-fsnotify.log")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	b := NewFsnotifyBackend(100 * time.Millisecond)
	specs := []WatchSpec{
		{JailName: "jail-c", Globs: []string{path}, ReadFromEnd: false},
		{JailName: "jail-d", Globs: []string{path}, ReadFromEnd: false},
	}
	out, cancel := startBackend(t, b, specs)
	defer cancel()

	time.Sleep(150 * time.Millisecond)

	af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(af, "shared fsnotify line")
	af.Close()

	seen := make(map[string]bool)
	for i := 0; i < 2; i++ {
		ev, ok := waitEvent(out, 2*time.Second)
		if !ok {
			t.Fatalf("timed out waiting for event %d/2 (got jails: %v)", i+1, seen)
		}
		if ev.Line != "shared fsnotify line" {
			t.Errorf("expected line %q, got %q", "shared fsnotify line", ev.Line)
		}
		seen[ev.JailName] = true
	}
	if !seen["jail-c"] || !seen["jail-d"] {
		t.Errorf("expected events for both jails, got: %v", seen)
	}
}

// TestDebugRateLimiterInWatch verifies the watch package's debugRateLimiter
// allows exactly maxPerSec entries per window and resets after one second.
func TestDebugRateLimiterInWatch(t *testing.T) {
base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
mockNow := base

rl := newDebugRateLimiter(2)
rl.now = func() time.Time { return mockNow }

if !rl.Allow() {
t.Fatal("1st call in window should be allowed")
}
if !rl.Allow() {
t.Fatal("2nd call in window should be allowed")
}
if rl.Allow() {
t.Fatal("3rd call in window should be denied")
}

mockNow = base.Add(time.Second)
if !rl.Allow() {
t.Fatal("1st call after window reset should be allowed")
}
if !rl.Allow() {
t.Fatal("2nd call after window reset should be allowed")
}
if rl.Allow() {
t.Fatal("3rd call after window reset should be denied")
}
}
