package action

import (
	"context"
	"strings"
	"testing"
	"text/template"
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

func TestRenderTags(t *testing.T) {
	result, err := Render("tags={{ .Tags }}", Context{Tags: "some-domain.com,webapp"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "tags=some-domain.com,webapp" {
		t.Errorf("got %q, want %q", result, "tags=some-domain.com,webapp")
	}
}

func TestRenderTagsEmpty(t *testing.T) {
	result, err := Render("{{ .IP }} tags={{ .Tags }}", Context{IP: "1.2.3.4"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "1.2.3.4 tags=" {
		t.Errorf("got %q, want %q", result, "1.2.3.4 tags=")
	}
}

func TestRenderInvalidTemplate(t *testing.T) {
	_, err := Render("{{ .IP", Context{})
	if err == nil {
		t.Fatal("expected error for invalid template, got nil")
	}
}

func TestCompileTemplate(t *testing.T) {
	tmpl, err := CompileTemplate("test", "nft add {{ .IP }}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tmpl == nil {
		t.Fatal("expected non-nil template")
	}
	if tmpl.Name() != "test" {
		t.Errorf("got name %q, want %q", tmpl.Name(), "test")
	}
}

func TestCompileTemplateInvalid(t *testing.T) {
	_, err := CompileTemplate("test", "{{ .IP")
	if err == nil {
		t.Fatal("expected error for invalid template, got nil")
	}
}

func TestRenderCompiled(t *testing.T) {
	tmpl, err := CompileTemplate("test", "nft add {{ .IP }}")
	if err != nil {
		t.Fatalf("unexpected error compiling: %v", err)
	}
	result, err := RenderCompiled(tmpl, Context{IP: "1.2.3.4"})
	if err != nil {
		t.Fatalf("unexpected error rendering: %v", err)
	}
	if result != "nft add 1.2.3.4" {
		t.Errorf("got %q, want %q", result, "nft add 1.2.3.4")
	}
}

func TestRenderCompiledMatchesRender(t *testing.T) {
	tmplStr := "block {{ .IP }} for {{ .JailTime }}s in {{ .Jail }}"
	ctx := Context{IP: "10.0.0.1", JailTime: 600, Jail: "ssh"}

	want, err := Render(tmplStr, ctx)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	tmpl, err := CompileTemplate("test", tmplStr)
	if err != nil {
		t.Fatalf("CompileTemplate error: %v", err)
	}
	got, err := RenderCompiled(tmpl, ctx)
	if err != nil {
		t.Fatalf("RenderCompiled error: %v", err)
	}
	if got != want {
		t.Errorf("RenderCompiled %q != Render %q", got, want)
	}
}

func TestRunCompiled(t *testing.T) {
	tmpl, err := CompileTemplate("test", "echo {{ .IP }}")
	if err != nil {
		t.Fatalf("unexpected error compiling: %v", err)
	}
	result, err := RunCompiled(context.Background(), tmpl, Context{IP: "1.2.3.4"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Stdout, "1.2.3.4") {
		t.Errorf("stdout %q does not contain IP", result.Stdout)
	}
}

func TestRunCompiledFailure(t *testing.T) {
	tmpl, err := CompileTemplate("test", "false")
	if err != nil {
		t.Fatalf("unexpected error compiling: %v", err)
	}
	result, err := RunCompiled(context.Background(), tmpl, Context{}, 0)
	if err == nil {
		t.Fatal("expected error for failing command, got nil")
	}
	if result.ExitCode == 0 {
		t.Errorf("expected non-zero exit code, got 0")
	}
}

func TestRunAllCompiled(t *testing.T) {
	tmpls := make([]*template.Template, 0, 2)
	for _, s := range []string{"echo hello", "echo {{ .IP }}"} {
		tmpl, err := CompileTemplate("test", s)
		if err != nil {
			t.Fatalf("unexpected error compiling: %v", err)
		}
		tmpls = append(tmpls, tmpl)
	}
	results, err := RunAllCompiled(context.Background(), tmpls, Context{IP: "1.2.3.4"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}
}

func TestRunAllCompiledStopsOnFailure(t *testing.T) {
	var tmpls []*template.Template
	for _, s := range []string{"echo ok", "false", "echo never"} {
		tmpl, err := CompileTemplate("test", s)
		if err != nil {
			t.Fatalf("compile error: %v", err)
		}
		tmpls = append(tmpls, tmpl)
	}
	results, err := RunAllCompiled(context.Background(), tmpls, Context{}, 0)
	if err == nil {
		t.Fatal("expected error from RunAllCompiled, got nil")
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results (stopped at second), got %d", len(results))
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
