package action

import (
	"bytes"
	"context"
	"log/slog"
	"os/exec"
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
// A timeout of 0 means no additional per-command timeout is applied; the
// command runs until it exits naturally or the parent context is cancelled.
// A positive timeout adds a per-command deadline on top of any parent deadline.
// For on_match actions, callers should pass cfg.ActionTimeout so that slow
// scripts (e.g. those that perform a remote WHOIS lookup) are not killed
// prematurely.
func Run(ctx context.Context, tmpl string, actCtx Context, timeout time.Duration) (Result, error) {
	rendered, err := Render(tmpl, actCtx)
	if err != nil {
		return Result{Command: tmpl, Error: err}, err
	}

	cmdCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		cmdCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(cmdCtx, "/bin/sh", "-c", rendered)
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
