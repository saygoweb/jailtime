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
