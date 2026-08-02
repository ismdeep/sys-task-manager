package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ismdeep/log"
	"go.uber.org/zap"

	"github.com/ismdeep/sys-task-manager/internal/auth"
	"github.com/ismdeep/sys-task-manager/internal/task"
)

type Executor struct {
	auth *auth.Authorizer
}

func New(authz *auth.Authorizer, _ string) *Executor {
	return &Executor{auth: authz}
}

func (e *Executor) Run(ctx context.Context, req task.RunRequest) (task.RunResult, error) {
	resolved, err := e.auth.ValidateRunAs(pickRunAs(req))
	if err != nil {
		return task.RunResult{}, err
	}

	runCtx := ctx
	cancel := func() {}
	if req.Task.Type != task.TypeDaemon && req.Task.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, req.Task.Timeout)
	}
	defer cancel()

	var lastResult task.RunResult
	var lastErr error

	for attempt := 1; attempt <= req.Task.Retry+1; attempt++ {
		lastResult, lastErr = e.runOnce(runCtx, req.Task, req.Trigger, req.RunID, resolved, attempt, req.OnUpdate)
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
	trigger task.TriggerMode,
	runID string,
	resolved auth.ResolvedUser,
	attempt int,
	onUpdate func(task.RunResult),
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

	if runID == "" {
		runID = fmt.Sprintf("%d", startedAt.UnixNano())
	}

	output := newTaskOutputCollector(t.Name, runID)
	stdout := output.writer("stdout")
	stderr := output.writer("stderr")
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	running := task.RunResult{
		TaskName:  t.Name,
		TaskType:  t.Type,
		Trigger:   trigger,
		RunAsUser: resolved.Username,
		RunID:     runID,
		StartedAt: startedAt,
		Attempt:   attempt,
		Status:    task.StatusRunning,
	}
	if onUpdate != nil {
		onUpdate(running)
	}

	err := cmd.Run()
	stdout.flush()
	stderr.flush()
	finishedAt := time.Now()
	outputText := output.String()

	result := task.RunResult{
		TaskName:   t.Name,
		TaskType:   t.Type,
		Trigger:    trigger,
		RunAsUser:  resolved.Username,
		RunID:      runID,
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		Attempt:    attempt,
		Output:     outputText,
		Status:     task.StatusSuccess,
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

const maxTaskOutputSize = 1 << 20

type taskOutputCollector struct {
	taskName string
	runID    string
	maxSize  int

	mu  sync.Mutex
	buf []byte
}

type taskOutputWriter struct {
	collector *taskOutputCollector
	stream    string
	partial   []byte
}

func newTaskOutputCollector(taskName, runID string) *taskOutputCollector {
	return &taskOutputCollector{
		taskName: taskName,
		runID:    runID,
		maxSize:  maxTaskOutputSize,
	}
}

func (c *taskOutputCollector) writer(stream string) *taskOutputWriter {
	return &taskOutputWriter{collector: c, stream: stream}
}

func (w *taskOutputWriter) Write(p []byte) (int, error) {
	w.collector.append(p)
	w.emit(p)
	return len(p), nil
}

func (w *taskOutputWriter) flush() {
	if len(w.partial) == 0 {
		return
	}
	w.collector.logLine(w.stream, string(w.partial))
	w.partial = nil
}

func (w *taskOutputWriter) emit(p []byte) {
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			w.partial = append(w.partial, p...)
			return
		}
		line := append(w.partial, p[:idx]...)
		w.partial = nil
		w.collector.logLine(w.stream, string(line))
		p = p[idx+1:]
	}
}

func (c *taskOutputCollector) append(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.buf = append(c.buf, p...)
	if len(c.buf) > c.maxSize {
		c.buf = append([]byte(nil), c.buf[len(c.buf)-c.maxSize:]...)
	}
}

func (c *taskOutputCollector) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

func (c *taskOutputCollector) logLine(stream, line string) {
	line = strings.TrimRight(line, "\r")
	if line == "" {
		return
	}
	log.WithContext(context.Background()).Info(
		"task output",
		zap.String("task", c.taskName),
		zap.String("run_id", c.runID),
		zap.String("stream", stream),
		zap.String("output", line),
	)
}
