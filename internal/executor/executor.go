package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ismdeep/sys-task-manager/internal/auth"
	"github.com/ismdeep/sys-task-manager/internal/task"
)

type Executor struct {
	auth   *auth.Authorizer
	logDir string
}

func New(authz *auth.Authorizer, logDir string) *Executor {
	return &Executor{auth: authz, logDir: logDir}
}

func (e *Executor) Run(ctx context.Context, req task.RunRequest) (task.RunResult, error) {
	resolved, err := e.auth.ValidateRunAs(pickRunAs(req))
	if err != nil {
		return task.RunResult{}, err
	}

	runCtx := ctx
	cancel := func() {}
	if req.Task.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, req.Task.Timeout)
	}
	defer cancel()

	var lastResult task.RunResult
	var lastErr error

	for attempt := 1; attempt <= req.Task.Retry+1; attempt++ {
		lastResult, lastErr = e.runOnce(runCtx, req.Task, resolved, attempt)
		if lastErr == nil {
			return lastResult, nil
		}

		if errors.Is(runCtx.Err(), context.Canceled) || errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return lastResult, lastErr
		}
	}

	return lastResult, lastErr
}

func pickRunAs(req task.RunRequest) string {
	if req.OverrideAs != "" {
		return req.OverrideAs
	}
	return req.Task.RunAsUser
}

func (e *Executor) runOnce(
	ctx context.Context,
	t task.Task,
	resolved auth.ResolvedUser,
	attempt int,
) (task.RunResult, error) {
	current := e.auth.Current()
	startedAt := time.Now()
	cmd := exec.CommandContext(ctx, t.Command.Command, t.Command.Args...)
	cmd.Dir = t.Command.WorkDir
	if len(t.Command.Env) > 0 {
		cmd.Env = append(os.Environ(), t.Command.Env...)
	}
	if resolved.UID != current.UID || resolved.GID != current.GID {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid: resolved.UID,
				Gid: resolved.GID,
			},
		}
	}

	if err := os.MkdirAll(e.logDir, 0o755); err != nil {
		return task.RunResult{}, fmt.Errorf("create temp log directory: %w", err)
	}

	logFile, err := os.CreateTemp(e.logDir, sanitizeFileName(t.Name)+"-attempt-*.log")
	if err != nil {
		return task.RunResult{}, fmt.Errorf("create temp log file: %w", err)
	}
	defer logFile.Close()

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	err = cmd.Run()
	finishedAt := time.Now()

	result := task.RunResult{
		TaskName:    t.Name,
		TaskType:    t.Type,
		Trigger:     task.TriggerMode(""),
		RunAsUser:   resolved.Username,
		StartedAt:   startedAt,
		FinishedAt:  finishedAt,
		Attempt:     attempt,
		TempLogPath: logFile.Name(),
		Status:      task.StatusSuccess,
	}

	if err == nil {
		return result, nil
	}

	result.Status = task.StatusFailed
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Err = fmt.Errorf("task timeout after %s: %w", t.Timeout, ctx.Err())
		return result, result.Err
	}

	if errors.Is(ctx.Err(), context.Canceled) {
		result.Err = fmt.Errorf("task canceled: %w", ctx.Err())
		return result, result.Err
	}

	result.Err = fmt.Errorf("execute command %q: %w", t.Command.Command, err)
	return result, result.Err
}

func sanitizeFileName(name string) string {
	if name == "" {
		return "task"
	}

	cleaned := filepath.Clean(name)
	if cleaned == "." {
		return "task"
	}

	return filepath.Base(cleaned)
}
