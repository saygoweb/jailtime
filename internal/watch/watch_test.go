package watch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestFileTailerNoSeekBetweenReads verifies that ReadLines works across
// multiple calls without any seek/reset — the bufio reader retains state.
func TestFileTailerNoSeekBetweenReads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tailer.log")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, "line one")
	fmt.Fprintln(f, "line two")
	fmt.Fprintln(f, "line three")
	f.Close()

	ft, err := NewFileTailer(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer ft.Close()

	lines, err := ft.ReadLines()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("first ReadLines: want 3 lines, got %d: %v", len(lines), lines)
	}
	for i, want := range []string{"line one", "line two", "line three"} {
		if lines[i] != want {
			t.Errorf("line[%d]: want %q, got %q", i, want, lines[i])
		}
	}

	// Append more lines and read again — no seek should be needed.
	af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(af, "line four")
	fmt.Fprintln(af, "line five")
	af.Close()

	lines2, err := ft.ReadLines()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines2) != 2 {
		t.Fatalf("second ReadLines: want 2 lines, got %d: %v", len(lines2), lines2)
	}
	for i, want := range []string{"line four", "line five"} {
		if lines2[i] != want {
			t.Errorf("second read line[%d]: want %q, got %q", i, want, lines2[i])
		}
	}
}

// TestFileTailerReopen verifies that after Reopen(false) the tailer reads
// from the beginning of the file.
func TestFileTailerReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reopen.log")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, "original line")
	f.Close()

	ft, err := NewFileTailer(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer ft.Close()

	// Read original content.
	lines, err := ft.ReadLines()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "original line" {
		t.Fatalf("initial read: want [\"original line\"], got %v", lines)
	}

	// Reopen from start.
	if err := ft.Reopen(false); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	// Should read from the beginning again.
	lines2, err := ft.ReadLines()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines2) != 1 || lines2[0] != "original line" {
		t.Fatalf("after Reopen: want [\"original line\"], got %v", lines2)
	}
}

func startBackendDrain(t *testing.T, b Backend, specs []WatchSpec) (chan RawLine, context.CancelFunc) {
	t.Helper()
	out := make(chan RawLine, 64)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = b.Start(ctx, specs, func(_ context.Context, lines []RawLine) {
			for _, l := range lines {
				out <- l
			}
		})
	}()
	return out, cancel
}

func waitEvent(out chan RawLine, timeout time.Duration) (RawLine, bool) {
	select {
	case ev := <-out:
		return ev, true
	case <-time.After(timeout):
		return RawLine{}, false
	}
}

func containsJail(jails []string, name string) bool {
	for _, j := range jails {
		if j == name {
			return true
		}
	}
	return false
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
	out, cancel := startBackendDrain(t, b, specs)
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
	if !containsJail(ev.Jails, "jail1") {
		t.Errorf("expected Jails to contain %q, got %v", "jail1", ev.Jails)
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
	out, cancel := startBackendDrain(t, b, specs)
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
	out, cancel := startBackendDrain(t, b, specs)
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
	out, cancel := startBackendDrain(t, b, specs)
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
	if !containsJail(ev.Jails, "apache") {
		t.Errorf("expected Jails to contain %q, got %v", "apache", ev.Jails)
	}
}

// TestPollBackendSubdirGlobMultiple verifies that a subdirectory glob continues
// to pick up files in additional new subdirectories added over time.
func TestPollBackendSubdirGlobMultiple(t *testing.T) {
	base := t.TempDir()
	pattern := filepath.Join(base, "*", "access.log")

	b := NewPollBackend(100 * time.Millisecond)
	specs := []WatchSpec{{JailName: "apache2", Globs: []string{pattern}, ReadFromEnd: true}}
	out, cancel := startBackendDrain(t, b, specs)
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
	out, cancel := startBackendDrain(t, b, specs)
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
	if !containsJail(ev.Jails, "apache") {
		t.Errorf("expected Jails to contain %q, got %v", "apache", ev.Jails)
	}
}

// TestFsnotifyBackendSubdirGlobRotation verifies that an already-matched file in
// an existing wildcard subdirectory is rediscovered after log rotation
// (rename + recreate) and continues to emit events from the recreated file.
func TestFsnotifyBackendSubdirGlobRotation(t *testing.T) {
	base := t.TempDir()
	subdir := filepath.Join(base, "site1")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(subdir, "access.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	pattern := filepath.Join(base, "*", "access.log")
	b := NewFsnotifyBackend(100 * time.Millisecond)
	specs := []WatchSpec{{JailName: "apache", Globs: []string{pattern}, ReadFromEnd: false}}
	out, cancel := startBackendDrain(t, b, specs)
	defer cancel()

	time.Sleep(150 * time.Millisecond)
	drainTimeout := time.After(300 * time.Millisecond)
drainLoop:
	for {
		select {
		case <-out:
		case <-drainTimeout:
			break drainLoop
		}
	}

	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	newF, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	fmt.Fprintln(newF, "9.8.7.6 GET /rotated 200")
	newF.Close()

	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for event after glob-backed rotation")
	}
	if ev.Line != "9.8.7.6 GET /rotated 200" {
		t.Errorf("expected line %q, got %q", "9.8.7.6 GET /rotated 200", ev.Line)
	}
	if ev.FilePath != path {
		t.Errorf("expected FilePath %q, got %q", path, ev.FilePath)
	}
	if !containsJail(ev.Jails, "apache") {
		t.Errorf("expected Jails to contain %q, got %v", "apache", ev.Jails)
	}
}

// TestFsnotifyBackendSubdirGlobRotationMultiple verifies that log rotation is
// correctly detected for all domain directories when multiple subdirectory files
// are rotated simultaneously — the typical Apache/logrotate pattern.
func TestFsnotifyBackendSubdirGlobRotationMultiple(t *testing.T) {
	base := t.TempDir()
	domains := []string{"site-a.com", "site-b.com", "site-c.com"}

	paths := make(map[string]string) // domain → log path
	for _, d := range domains {
		subdir := filepath.Join(base, d)
		if err := os.Mkdir(subdir, 0755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(subdir, "access.log")
		if err := os.WriteFile(p, []byte("old line\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths[d] = p
	}

	pattern := filepath.Join(base, "*", "access.log")
	b := NewFsnotifyBackend(100 * time.Millisecond)
	specs := []WatchSpec{{JailName: "apache", Globs: []string{pattern}, ReadFromEnd: true}}
	out, cancel := startBackendDrain(t, b, specs)
	defer cancel()

	// Let the backend settle; drain any startup events.
	time.Sleep(200 * time.Millisecond)
	drainTimeout := time.After(400 * time.Millisecond)
drainLoop:
	for {
		select {
		case <-out:
		case <-drainTimeout:
			break drainLoop
		}
	}

	// Rotate all domains simultaneously (rename then create new empty file).
	for _, d := range domains {
		p := paths[d]
		if err := os.Rename(p, p+".1"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Create(p); err != nil {
			t.Fatal(err)
		}
	}

	time.Sleep(150 * time.Millisecond) // let Create events propagate

	// Write a distinct line to each new log file.
	want := make(map[string]string)
	for _, d := range domains {
		line := d + " - rotated line"
		want[paths[d]] = line
		if err := os.WriteFile(paths[d], []byte(line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Collect events; every domain must be heard from within 3 seconds.
	seen := make(map[string]bool)
	deadline := time.After(3 * time.Second)
	for len(seen) < len(domains) {
		select {
		case ev := <-out:
			if wantLine, ok := want[ev.FilePath]; ok && ev.Line == wantLine {
				seen[ev.FilePath] = true
			}
		case <-deadline:
			t.Errorf("timed out: only %d/%d domains received events after simultaneous rotation; missing files: %v",
				len(seen), len(domains), missingKeys(want, seen))
			return
		}
	}
}

// missingKeys returns keys present in want but not in seen.
func missingKeys(want map[string]string, seen map[string]bool) []string {
	var out []string
	for k := range want {
		if !seen[k] {
			out = append(out, k)
		}
	}
	return out
}

// TestFsnotifyBackendSubdirGlobRotationReadFromEnd verifies that after rotation,
// only content from the new file is emitted — the old pre-rotation content is
// not re-read — when ReadFromEnd is true (the production default).
func TestFsnotifyBackendSubdirGlobRotationReadFromEnd(t *testing.T) {
	base := t.TempDir()
	subdir := filepath.Join(base, "site1")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(subdir, "access.log")
	// File already has content; with ReadFromEnd:true this must NOT be re-emitted.
	if err := os.WriteFile(path, []byte("old line 1\nold line 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pattern := filepath.Join(base, "*", "access.log")
	b := NewFsnotifyBackend(100 * time.Millisecond)
	specs := []WatchSpec{{JailName: "apache", Globs: []string{pattern}, ReadFromEnd: true}}
	out, cancel := startBackendDrain(t, b, specs)
	defer cancel()

	time.Sleep(200 * time.Millisecond)
	// Drain any (unwanted) startup events.
	drainTimeout := time.After(400 * time.Millisecond)
drainLoop:
	for {
		select {
		case <-out:
		case <-drainTimeout:
			break drainLoop
		}
	}

	// Rotate and write a fresh line to the new file.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	newF, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	fmt.Fprintln(newF, "new line after rotation")
	newF.Close()

	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for event after rotation with ReadFromEnd:true")
	}
	if ev.Line != "new line after rotation" {
		t.Errorf("expected %q, got %q (old content must not be re-emitted)", "new line after rotation", ev.Line)
	}
}

// TestFsnotifyBackendSubdirGlobRotationImmediate verifies that rotation is
// detected even when writes to the new file begin immediately (no sleep between
// create and write), mirroring how Apache opens and writes to the new log file
// right after logrotate sends SIGHUP.
func TestFsnotifyBackendSubdirGlobRotationImmediate(t *testing.T) {
	base := t.TempDir()
	subdir := filepath.Join(base, "site1")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(subdir, "access.log")
	if _, err := os.Create(path); err != nil {
		t.Fatal(err)
	}

	pattern := filepath.Join(base, "*", "access.log")
	b := NewFsnotifyBackend(100 * time.Millisecond)
	specs := []WatchSpec{{JailName: "apache", Globs: []string{pattern}, ReadFromEnd: false}}
	out, cancel := startBackendDrain(t, b, specs)
	defer cancel()

	time.Sleep(150 * time.Millisecond)
	drainTimeout := time.After(300 * time.Millisecond)
drainLoop:
	for {
		select {
		case <-out:
		case <-drainTimeout:
			break drainLoop
		}
	}

	// Rename then immediately create and write — no sleep.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	newF, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(newF, "immediate write after rotation")
	newF.Close()

	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for event after immediate-write rotation")
	}
	if ev.Line != "immediate write after rotation" {
		t.Errorf("expected %q, got %q", "immediate write after rotation", ev.Line)
	}
}

// TestFsnotifyBackendSubdirGlobCopyTruncate verifies that copytruncate-style log
// rotation (file is truncated in-place rather than renamed) is detected by the
// fsnotify backend.  This exercises the CheckRotation path in readTailLines.
func TestFsnotifyBackendSubdirGlobCopyTruncate(t *testing.T) {
	base := t.TempDir()
	subdir := filepath.Join(base, "site1")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(subdir, "access.log")
	if err := os.WriteFile(path, []byte("old content line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pattern := filepath.Join(base, "*", "access.log")
	b := NewFsnotifyBackend(100 * time.Millisecond)
	// ReadFromEnd:false so we'd pick up old content if CheckRotation is broken.
	specs := []WatchSpec{{JailName: "apache", Globs: []string{pattern}, ReadFromEnd: false}}
	out, cancel := startBackendDrain(t, b, specs)
	defer cancel()

	// Drain startup events (the existing "old content line").
	time.Sleep(150 * time.Millisecond)
	drainTimeout := time.After(400 * time.Millisecond)
drainLoop:
	for {
		select {
		case <-out:
		case <-drainTimeout:
			break drainLoop
		}
	}

	// copytruncate step 1: truncate the file to zero (simulates logrotate's
	// truncation of the live log). This generates a Write event; the drain
	// will call CheckRotation and detect size(0) < offset → Reopen.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	// Wait long enough for the drain to fire and detect the truncation.
	time.Sleep(300 * time.Millisecond)

	// copytruncate step 2: append new content (simulates Apache writing after
	// receiving SIGHUP and having its fd reset to the same file).
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, "post-truncate line")
	f.Close()

	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for event after copytruncate-style rotation")
	}
	if ev.Line != "post-truncate line" {
		t.Errorf("expected %q, got %q", "post-truncate line", ev.Line)
	}
}

// TestFsnotifyBackendSubdirGlobRotationSecond verifies that a second rotation
// (the day after the first) is detected correctly — i.e. the backend does not
// get stuck after the first rotation has already been handled.
func TestFsnotifyBackendSubdirGlobRotationSecond(t *testing.T) {
	base := t.TempDir()
	subdir := filepath.Join(base, "site1")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(subdir, "access.log")
	if _, err := os.Create(path); err != nil {
		t.Fatal(err)
	}

	pattern := filepath.Join(base, "*", "access.log")
	b := NewFsnotifyBackend(100 * time.Millisecond)
	specs := []WatchSpec{{JailName: "apache", Globs: []string{pattern}, ReadFromEnd: false}}
	out, cancel := startBackendDrain(t, b, specs)
	defer cancel()

	rotate := func(expectLine string) {
		t.Helper()
		// Drain pending events first.
		time.Sleep(150 * time.Millisecond)
		drainTimeout := time.After(300 * time.Millisecond)
	drain:
		for {
			select {
			case <-out:
			case <-drainTimeout:
				break drain
			}
		}

		if err := os.Rename(path, path+".1"); err != nil {
			t.Fatal(err)
		}
		newF, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(150 * time.Millisecond)
		fmt.Fprintln(newF, expectLine)
		newF.Close()

		ev, ok := waitEvent(out, 2*time.Second)
		if !ok {
			t.Fatalf("timed out waiting for %q", expectLine)
		}
		if ev.Line != expectLine {
			t.Errorf("expected %q, got %q", expectLine, ev.Line)
		}
	}

	rotate("line after first rotation")
	rotate("line after second rotation")
}

// TestPollBackendSubdirGlobRotation verifies that the poll backend also handles
// log rotation in a wildcard-subdirectory glob pattern.
func TestPollBackendSubdirGlobRotation(t *testing.T) {
	base := t.TempDir()
	subdir := filepath.Join(base, "site1")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(subdir, "access.log")
	if _, err := os.Create(path); err != nil {
		t.Fatal(err)
	}

	pattern := filepath.Join(base, "*", "access.log")
	b := NewPollBackend(100 * time.Millisecond)
	specs := []WatchSpec{{JailName: "apache", Globs: []string{pattern}, ReadFromEnd: false}}
	out, cancel := startBackendDrain(t, b, specs)
	defer cancel()

	time.Sleep(150 * time.Millisecond)
	drainTimeout := time.After(400 * time.Millisecond)
drainLoop:
	for {
		select {
		case <-out:
		case <-drainTimeout:
			break drainLoop
		}
	}

	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	newF, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	fmt.Fprintln(newF, "poll rotation line")
	newF.Close()

	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for event after poll-backend glob rotation")
	}
	if ev.Line != "poll rotation line" {
		t.Errorf("expected %q, got %q", "poll rotation line", ev.Line)
	}
	if !containsJail(ev.Jails, "apache") {
		t.Errorf("expected Jails to contain 'apache', got %v", ev.Jails)
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
	out, cancel := startBackendDrain(t, b, specs)
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
	if !containsJail(ev.Jails, "jail4") {
		t.Errorf("expected Jails to contain %q, got %v", "jail4", ev.Jails)
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
	out, cancel := startBackendDrain(t, b, specs)
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
	if !containsJail(ev.Jails, "jail-inotify") {
		t.Errorf("expected Jails to contain %q, got %v", "jail-inotify", ev.Jails)
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
	out, cancel := startBackendDrain(t, b, specs)
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
	if !containsJail(ev.Jails, "jail-os") {
		t.Errorf("expected Jails to contain %q, got %v", "jail-os", ev.Jails)
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
	out, cancel := startBackendDrain(t, b, specs)
	defer cancel()

	time.Sleep(150 * time.Millisecond)

	af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(af, "shared line")
	af.Close()

	seen := make(map[string]bool)
	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for RawLine event")
	}
	if ev.Line != "shared line" {
		t.Errorf("expected line %q, got %q", "shared line", ev.Line)
	}
	for _, j := range ev.Jails {
		seen[j] = true
	}
	if !seen["jail-a"] || !seen["jail-b"] {
		t.Errorf("expected RawLine.Jails to contain both jails, got: %v", ev.Jails)
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
	out, cancel := startBackendDrain(t, b, specs)
	defer cancel()

	time.Sleep(150 * time.Millisecond)

	af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(af, "shared fsnotify line")
	af.Close()

	seen := make(map[string]bool)
	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for RawLine event")
	}
	if ev.Line != "shared fsnotify line" {
		t.Errorf("expected line %q, got %q", "shared fsnotify line", ev.Line)
	}
	for _, j := range ev.Jails {
		seen[j] = true
	}
	if !seen["jail-c"] || !seen["jail-d"] {
		t.Errorf("expected RawLine.Jails to contain both jails, got: %v", ev.Jails)
	}
}

// TestFsnotifyBackendCoalescing verifies that multiple WRITE events occurring
// within the same batch window are coalesced: all lines still arrive, with no
// duplicates, and no immediate reads on every individual kernel event.
func TestFsnotifyBackendCoalescing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "coalesce.log")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	b := NewFsnotifyBackend(500 * time.Millisecond) // rescan interval; batch is 50ms
	specs := []WatchSpec{{JailName: "jail-coalesce", Globs: []string{path}, ReadFromEnd: false}}
	out, cancel := startBackendDrain(t, b, specs)
	defer cancel()

	time.Sleep(150 * time.Millisecond)

	// Write several lines rapidly — well within a single 50 ms batch window.
	af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	const numLines = 5
	for i := 0; i < numLines; i++ {
		fmt.Fprintf(af, "coalesced line %d\n", i)
	}
	af.Close()

	// All lines must arrive; duplicates indicate a double-read bug.
	received := make([]string, 0, numLines)
	deadline := time.After(2 * time.Second)
	for len(received) < numLines {
		select {
		case ev := <-out:
			received = append(received, ev.Line)
		case <-deadline:
			t.Fatalf("timed out: received %d/%d lines", len(received), numLines)
		}
	}

	seen := make(map[string]int, numLines)
	for _, l := range received {
		seen[l]++
	}
	for l, count := range seen {
		if count > 1 {
			t.Errorf("duplicate line %q received %d times", l, count)
		}
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

// TestPollBackendSharedFileNewFile verifies that when two jails watch the same
// path and the file is created after the backend starts, both jails receive
// events for lines appended to that file.
func TestPollBackendSharedFileNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blockhandler.log")

	b := NewPollBackend(100 * time.Millisecond)
	specs := []WatchSpec{
		{JailName: "blockhandler", Globs: []string{path}, WatchMode: "tail", ReadFromEnd: true},
		{JailName: "whitelist-blockhandler", Globs: []string{path}, WatchMode: "tail", ReadFromEnd: true},
	}
	out, cancel := startBackendDrain(t, b, specs)
	defer cancel()

	time.Sleep(150 * time.Millisecond)

	// Create the file after the backend is running (simulates a log file that
	// does not yet exist at daemon startup).
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	fmt.Fprintln(f, "1.2.3.4 blocked")
	f.Close()

	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for event")
	}
	if ev.Line != "1.2.3.4 blocked" {
		t.Errorf("expected line %q, got %q", "1.2.3.4 blocked", ev.Line)
	}
	seen := make(map[string]bool)
	for _, j := range ev.Jails {
		seen[j] = true
	}
	if !seen["blockhandler"] || !seen["whitelist-blockhandler"] {
		t.Errorf("expected both jails in RawLine.Jails, got: %v", ev.Jails)
	}
}

// TestFsnotifyBackendSharedFileNewFile verifies that when two jails watch the
// same path and the file is created after the backend starts, both jails
// receive events. This is the regression test for the handleCreate early-return
// bug where only the first matching jail was registered in pathToJails.
func TestFsnotifyBackendSharedFileNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blockhandler.log")

	b := NewFsnotifyBackend(100 * time.Millisecond)
	specs := []WatchSpec{
		{JailName: "blockhandler", Globs: []string{path}, WatchMode: "tail", ReadFromEnd: true},
		{JailName: "whitelist-blockhandler", Globs: []string{path}, WatchMode: "tail", ReadFromEnd: true},
	}
	out, cancel := startBackendDrain(t, b, specs)
	defer cancel()

	time.Sleep(150 * time.Millisecond)

	// Create the file after the backend has started — this triggers handleCreate.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	fmt.Fprintln(f, "1.2.3.4 blocked")
	f.Close()

	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for event after late-created shared file")
	}
	if ev.Line != "1.2.3.4 blocked" {
		t.Errorf("expected line %q, got %q", "1.2.3.4 blocked", ev.Line)
	}
	seen := make(map[string]bool)
	for _, j := range ev.Jails {
		seen[j] = true
	}
	if !seen["blockhandler"] || !seen["whitelist-blockhandler"] {
		t.Errorf("expected both jails in RawLine.Jails after late-created file, got: %v", ev.Jails)
	}
}

// TestFsnotifyBackendSharedFileRotation verifies that after a log rotation
// (rename + recreate) both jails remain registered and continue to receive
// events for the new file.
func TestFsnotifyBackendSharedFileRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blockhandler.log")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	b := NewFsnotifyBackend(100 * time.Millisecond)
	specs := []WatchSpec{
		{JailName: "blockhandler", Globs: []string{path}, WatchMode: "tail", ReadFromEnd: false},
		{JailName: "whitelist-blockhandler", Globs: []string{path}, WatchMode: "tail", ReadFromEnd: false},
	}
	out, cancel := startBackendDrain(t, b, specs)
	defer cancel()

	time.Sleep(150 * time.Millisecond)
	// Drain any events from the initial empty file.
	drainTimeout := time.After(300 * time.Millisecond)
drainLoop:
	for {
		select {
		case <-out:
		case <-drainTimeout:
			break drainLoop
		}
	}

	// Rotate: rename old file, create new one at the same path.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	newF, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	fmt.Fprintln(newF, "5.6.7.8 blocked after rotation")
	newF.Close()

	ev, ok := waitEvent(out, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for event after rotation")
	}
	if ev.Line != "5.6.7.8 blocked after rotation" {
		t.Errorf("expected line %q, got %q", "5.6.7.8 blocked after rotation", ev.Line)
	}
	seen := make(map[string]bool)
	for _, j := range ev.Jails {
		seen[j] = true
	}
	if !seen["blockhandler"] || !seen["whitelist-blockhandler"] {
		t.Errorf("expected both jails after rotation, got: %v", ev.Jails)
	}
}

// TestFsnotifyBackendIdleNoDrain verifies that when no writes occur the drain
// callback is never invoked, i.e., the backend is truly idle with no wakeups.
func TestFsnotifyBackendIdleNoDrain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idle.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	const drainInterval = 50 * time.Millisecond
	var drainCount atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := NewFsnotifyBackend(drainInterval)
	specs := []WatchSpec{{JailName: "idle-jail", Globs: []string{path}, ReadFromEnd: true}}
	go func() {
		_ = b.Start(ctx, specs, func(_ context.Context, lines []RawLine) {
			drainCount.Add(int64(len(lines)))
		})
	}()

	// Wait 4× drainInterval with no writes — drain should NOT be called.
	time.Sleep(4 * drainInterval)
	if n := drainCount.Load(); n != 0 {
		t.Errorf("expected 0 drain calls while idle, got drain count %d", n)
	}

	// Now write a line — it should arrive.
	af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(af, "after idle")
	af.Close()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-time.After(10 * time.Millisecond):
			if drainCount.Load() > 0 {
				goto done
			}
		case <-deadline:
			t.Fatal("timed out waiting for line after idle period")
		}
	}
done:
}

// ---- New tests for ExcludeGlobs and static watch_mode ----

// drainToSlice collects RawLines from a backend into a slice via channel.
// It cancels ctx after first drain call that produces at least minLines.
func drainToSliceN(minLines int) (DrainFunc, func() []RawLine) {
	var mu sync.Mutex
	var collected []RawLine
	done := make(chan struct{})
	var once sync.Once

	drain := func(ctx context.Context, lines []RawLine) {
		mu.Lock()
		collected = append(collected, lines...)
		total := len(collected)
		mu.Unlock()
		if total >= minLines {
			once.Do(func() { close(done) })
		}
	}
	wait := func() []RawLine {
		<-done
		mu.Lock()
		defer mu.Unlock()
		out := make([]RawLine, len(collected))
		copy(out, collected)
		return out
	}
	return drain, wait
}

func TestPollBackendExcludeGlobs(t *testing.T) {
	dir := t.TempDir()
	included := filepath.Join(dir, "included.log")
	excluded := filepath.Join(dir, "excluded.log")

	if err := os.WriteFile(included, []byte("line-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(excluded, []byte("line-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := WatchSpec{
		JailName:     "test",
		Globs:        []string{filepath.Join(dir, "*.log")},
		ExcludeGlobs: []string{excluded},
		WatchMode:    "tail",
		ReadFromEnd:  false,
	}

	drain, wait := drainToSliceN(1)
	ctx, cancel := context.WithCancel(context.Background())

	b := NewPollBackend(50 * time.Millisecond)
	go b.Start(ctx, []WatchSpec{spec}, drain)
	time.Sleep(150 * time.Millisecond)

	lines := wait()
	cancel()

	for _, l := range lines {
		if l.FilePath == excluded {
			t.Errorf("excluded file appeared in drain: %s", l.FilePath)
		}
	}
	found := false
	for _, l := range lines {
		if l.Line == "line-a" {
			found = true
		}
	}
	if !found {
		t.Error("included file line-a not found in drain")
	}
}

// TestPollBackendExcludeGlobsPartialName verifies that exclude_files entries
// that are partial path components (e.g. "kitchendraw.co.nz") correctly
// exclude files whose full path contains that substring, such as
// /var/log/apache2/kitchendraw.co.nz/access.log.
func TestPollBackendExcludeGlobsPartialName(t *testing.T) {
	dir := t.TempDir()

	// Create subdirectory structure mirroring /var/log/apache2/<vhost>/access.log
	excludedDir := filepath.Join(dir, "kitchendraw.co.nz")
	includedDir := filepath.Join(dir, "example.com")
	if err := os.MkdirAll(excludedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(includedDir, 0o755); err != nil {
		t.Fatal(err)
	}

	excludedFile := filepath.Join(excludedDir, "access.log")
	includedFile := filepath.Join(includedDir, "access.log")

	if err := os.WriteFile(excludedFile, []byte("excluded-line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(includedFile, []byte("included-line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := WatchSpec{
		JailName:     "test",
		Globs:        []string{filepath.Join(dir, "*/access.log")},
		ExcludeGlobs: []string{"kitchendraw.co.nz"}, // partial name only
		WatchMode:    "tail",
		ReadFromEnd:  false,
	}

	drain, wait := drainToSliceN(1)
	ctx, cancel := context.WithCancel(context.Background())

	b := NewPollBackend(50 * time.Millisecond)
	go b.Start(ctx, []WatchSpec{spec}, drain)
	time.Sleep(150 * time.Millisecond)

	lines := wait()
	cancel()

	for _, l := range lines {
		if l.FilePath == excludedFile {
			t.Errorf("excluded file appeared in drain: %s", l.FilePath)
		}
		if l.Line == "excluded-line" {
			t.Errorf("excluded line appeared in drain: %s", l.Line)
		}
	}
	found := false
	for _, l := range lines {
		if l.Line == "included-line" {
			found = true
		}
	}
	if !found {
		t.Error("included file line not found in drain")
	}
}

func TestPollBackendStaticMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "whitelist.txt")

	if err := os.WriteFile(path, []byte("1.2.3.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	type kindLine struct {
		line string
		kind EventKind
	}
	var mu sync.Mutex
	var got []kindLine
	addedSeen := make(chan struct{})
	var addedOnce sync.Once

	drain := func(_ context.Context, lines []RawLine) {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range lines {
			got = append(got, kindLine{l.Line, l.Kind})
		}
		for _, l := range got {
			if l.kind == EventAdded {
				addedOnce.Do(func() { close(addedSeen) })
			}
		}
	}

	spec := WatchSpec{
		JailName:  "wl",
		Globs:     []string{path},
		WatchMode: "static",
	}

	ctx, cancel := context.WithCancel(context.Background())
	b := NewPollBackend(50 * time.Millisecond)
	go b.Start(ctx, []WatchSpec{spec}, drain)

	select {
	case <-addedSeen:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timed out waiting for EventAdded")
	}

	// Remove the IP and wait for EventRemoved.
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			cancel()
			t.Fatal("timed out waiting for EventRemoved")
		case <-time.After(100 * time.Millisecond):
			mu.Lock()
			for _, l := range got {
				if l.kind == EventRemoved && l.line == "1.2.3.4" {
					mu.Unlock()
					cancel()
					return
				}
			}
			mu.Unlock()
		}
	}
}
