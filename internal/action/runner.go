package action

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"text/template"
	"time"
)

const defaultTimeout = 10 * time.Second

// Result holds the outcome of running a single command.
type Result struct {
	Command  string
	Stdout   string
	Stderr   string
	ExitCode int
	Error    error
}

// RunAll renders and executes a list of command templates sequentially.
// Stops on first command failure (non-zero exit or exec error).
// Each command is executed with: /bin/sh -c <rendered_command>
// Timeout is applied per command (default 10s if zero).
// Returns the results for all executed commands and any error.
func RunAll(ctx context.Context, templates []string, actCtx Context, timeout time.Duration) ([]Result, error) {
	var results []Result
	for _, tmpl := range templates {
		result, err := Run(ctx, tmpl, actCtx, timeout)
		results = append(results, result)
		if err != nil {
			return results, err
		}
	}
	return results, nil
}

// Run renders and executes a single command template.
func Run(ctx context.Context, tmpl string, actCtx Context, timeout time.Duration) (Result, error) {
	if timeout == 0 {
		timeout = defaultTimeout
	}

	rendered, err := Render(tmpl, actCtx)
	if err != nil {
		return Result{Command: tmpl, Error: err}, err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", rendered)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	result := Result{
		Command:  rendered,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		Error:    runErr,
	}

	slog.Info("action run",
		"command", rendered,
		"stdout", result.Stdout,
		"stderr", result.Stderr,
		"exitCode", exitCode,
	)

	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

// RunCompiled executes a pre-compiled template and runs the result as a shell command.
// If timeout is 0, defaultTimeout is used.
func RunCompiled(ctx context.Context, tmpl *template.Template, actCtx Context, timeout time.Duration) (Result, error) {
	if timeout == 0 {
		timeout = defaultTimeout
	}

	rendered, err := RenderCompiled(tmpl, actCtx)
	if err != nil {
		return Result{Command: tmpl.Name(), Error: err}, fmt.Errorf("rendering template: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", rendered)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	result := Result{
		Command:  rendered,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		Error:    runErr,
	}

	slog.Info("action run",
		"command", rendered,
		"stdout", result.Stdout,
		"stderr", result.Stderr,
		"exitCode", exitCode,
	)

	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

// RunAllCompiled executes a slice of pre-compiled templates sequentially.
// Stops on first error. Returns results for all attempted commands.
func RunAllCompiled(ctx context.Context, tmpls []*template.Template, actCtx Context, timeout time.Duration) ([]Result, error) {
	var results []Result
	for _, tmpl := range tmpls {
		res, err := RunCompiled(ctx, tmpl, actCtx, timeout)
		results = append(results, res)
		if err != nil {
			return results, err
		}
		if res.ExitCode != 0 {
			return results, fmt.Errorf("command exited with code %d: %s", res.ExitCode, res.Command)
		}
	}
	return results, nil
}
