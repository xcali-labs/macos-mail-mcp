package mail

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultTimeout = 90 * time.Second
	timeoutEnvVar  = "APPLE_MAIL_MCP_TIMEOUT"
)

type ExecError struct {
	ExitCode int
	Stderr   string
	Script   string
}

func (e *ExecError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("osascript failed (exit=%d): %s", e.ExitCode, e.Stderr)
}

type ScriptRunner interface {
	Run(ctx context.Context, script string) (string, error)
}

type OsaScriptRunner struct {
	timeout time.Duration
}

func NewOsaScriptRunnerFromEnv() *OsaScriptRunner {
	return &OsaScriptRunner{timeout: parseTimeout()}
}

func parseTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv(timeoutEnvVar))
	if raw == "" {
		return defaultTimeout
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultTimeout
}

func (r *OsaScriptRunner) Run(ctx context.Context, script string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := defaultTimeout
	if r != nil && r.timeout > 0 {
		timeout = r.timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", script)
	output, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(output)), nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("osascript timeout after %s", timeout)
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		execErr := &ExecError{
			ExitCode: exitCode(exitErr),
			Stderr:   strings.TrimSpace(string(exitErr.Stderr)),
			Script:   script,
		}
		if execErr.Stderr == "" {
			execErr.Stderr = err.Error()
		}
		return "", execErr
	}
	return "", fmt.Errorf("run osascript: %w", err)
}

func exitCode(err *exec.ExitError) int {
	if err == nil {
		return 1
	}
	if status, ok := err.Sys().(syscall.WaitStatus); ok {
		return status.ExitStatus()
	}
	return 1
}
