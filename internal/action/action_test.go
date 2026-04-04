package action

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRenderBasic(t *testing.T) {
	result, err := Render("nft add {{ .IP }}", Context{IP: "1.2.3.4"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "nft add 1.2.3.4" {
		t.Errorf("got %q, want %q", result, "nft add 1.2.3.4")
	}
}

func TestRenderJailTime(t *testing.T) {
	result, err := Render("timeout {{ .JailTime }}s", Context{JailTime: 300})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "timeout 300s" {
		t.Errorf("got %q, want %q", result, "timeout 300s")
	}
}

func TestRenderInvalidTemplate(t *testing.T) {
	_, err := Render("{{ .IP", Context{})
	if err == nil {
		t.Fatal("expected error for invalid template, got nil")
	}
}

func TestRunEcho(t *testing.T) {
	result, err := Run(context.Background(), "echo {{ .IP }}", Context{IP: "1.2.3.4"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Stdout, "1.2.3.4") {
		t.Errorf("stdout %q does not contain IP", result.Stdout)
	}
}

func TestRunFailure(t *testing.T) {
	result, err := Run(context.Background(), "false", Context{}, 0)
	if err == nil {
		t.Fatal("expected error for failing command, got nil")
	}
	if result.ExitCode == 0 {
		t.Errorf("expected non-zero exit code, got 0")
	}
}

func TestRunTimeout(t *testing.T) {
	_, err := Run(context.Background(), "sleep 10", Context{}, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestRunAllStopsOnFailure(t *testing.T) {
	templates := []string{"echo ok", "false", "echo never"}
	results, err := RunAll(context.Background(), templates, Context{}, 0)
	if err == nil {
		t.Fatal("expected error from RunAll, got nil")
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results (stopped at second), got %d", len(results))
	}
}
