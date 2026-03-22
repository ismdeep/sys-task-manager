package manager

import (
	"context"
	"testing"
	"time"

	"github.com/ismdeep/sys-task-manager/internal/auth"
	"github.com/ismdeep/sys-task-manager/internal/task"
)

type stubRunner struct {
	last task.RunRequest
}

func (r *stubRunner) Run(_ context.Context, req task.RunRequest) (task.RunResult, error) {
	r.last = req
	now := time.Now()
	runAs := req.Task.RunAsUser
	if req.OverrideAs != "" {
		runAs = req.OverrideAs
	}

	return task.RunResult{
		TaskName:   req.Task.Name,
		TaskType:   req.Task.Type,
		Trigger:    req.Trigger,
		RunAsUser:  runAs,
		Status:     task.StatusSuccess,
		StartedAt:  now,
		FinishedAt: now,
		Attempt:    1,
	}, nil
}

type stubRunLogger struct {
	last task.RunResult
}

func (l *stubRunLogger) Write(result task.RunResult) error {
	l.last = result
	return nil
}

type blockingRunner struct {
	started chan task.RunRequest
	stopped chan struct{}
}

func (r *blockingRunner) Run(ctx context.Context, req task.RunRequest) (task.RunResult, error) {
	if r.started != nil {
		select {
		case r.started <- req:
		default:
		}
	}

	<-ctx.Done()

	if r.stopped != nil {
		select {
		case r.stopped <- struct{}{}:
		default:
		}
	}

	return task.RunResult{}, ctx.Err()
}

func newTestManager(t *testing.T, runner task.Runner, runLogger *stubRunLogger) *Manager {
	t.Helper()

	authz, err := auth.NewAuthorizer()
	if err != nil {
		t.Fatalf("create authorizer: %v", err)
	}

	return New(authz, runner, runLogger, 1)
}

func TestRunManualSupportsCronTask(t *testing.T) {
	runner := &stubRunner{}
	runLogger := &stubRunLogger{}
	mgr := newTestManager(t, runner, runLogger)

	item := task.Task{
		Name:     "cron-task",
		Type:     task.TypeCron,
		CronExpr: "*/10 * * * * *",
		Enabled:  true,
		Command: task.CommandSpec{
			Command: "echo",
			Args:    []string{"hi"},
		},
	}
	if err := mgr.Register(item); err != nil {
		t.Fatalf("register task: %v", err)
	}

	result, err := mgr.RunManual(context.Background(), item.Name, "", false)
	if err != nil {
		t.Fatalf("run manual: %v", err)
	}

	if runner.last.Trigger != task.TriggerManual {
		t.Fatalf("runner trigger = %q, want %q", runner.last.Trigger, task.TriggerManual)
	}
	if result.Trigger != task.TriggerManual {
		t.Fatalf("result trigger = %q, want %q", result.Trigger, task.TriggerManual)
	}
	if result.TaskType != task.TypeCron {
		t.Fatalf("result task type = %q, want %q", result.TaskType, task.TypeCron)
	}
	if runLogger.last.TaskName != item.Name {
		t.Fatalf("logged task = %q, want %q", runLogger.last.TaskName, item.Name)
	}
}

func TestRunManualRejectsDaemonTask(t *testing.T) {
	runner := &stubRunner{}
	runLogger := &stubRunLogger{}
	mgr := newTestManager(t, runner, runLogger)

	item := task.Task{
		Name:    "daemon-task",
		Type:    task.TypeDaemon,
		Enabled: true,
		Command: task.CommandSpec{
			Command: "sleep",
			Args:    []string{"1"},
		},
	}
	if err := mgr.Register(item); err != nil {
		t.Fatalf("register task: %v", err)
	}

	if _, err := mgr.RunManual(context.Background(), item.Name, "", false); err == nil {
		t.Fatal("expected daemon task to reject manual trigger")
	}
}

func TestReplaceStartsDaemonWhenEnabled(t *testing.T) {
	runner := &blockingRunner{
		started: make(chan task.RunRequest, 1),
		stopped: make(chan struct{}, 1),
	}
	runLogger := &stubRunLogger{}
	mgr := newTestManager(t, runner, runLogger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	item := task.Task{
		Name:    "daemon-enable",
		Type:    task.TypeDaemon,
		Enabled: false,
		Command: task.CommandSpec{Command: "sleep", Args: []string{"1"}},
	}
	if err := mgr.Register(item); err != nil {
		t.Fatalf("register task: %v", err)
	}
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("start manager: %v", err)
	}

	item.Enabled = true
	if err := mgr.Replace(item); err != nil {
		t.Fatalf("replace task: %v", err)
	}

	select {
	case req := <-runner.started:
		if req.Trigger != task.TriggerDaemon {
			t.Fatalf("trigger = %q, want %q", req.Trigger, task.TriggerDaemon)
		}
	case <-time.After(time.Second):
		t.Fatal("expected enabled daemon to auto start")
	}
}

func TestReplaceStopsDaemonWhenDisabled(t *testing.T) {
	runner := &blockingRunner{
		started: make(chan task.RunRequest, 1),
		stopped: make(chan struct{}, 1),
	}
	runLogger := &stubRunLogger{}
	mgr := newTestManager(t, runner, runLogger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	item := task.Task{
		Name:    "daemon-disable",
		Type:    task.TypeDaemon,
		Enabled: true,
		Command: task.CommandSpec{Command: "sleep", Args: []string{"1"}},
	}
	if err := mgr.Register(item); err != nil {
		t.Fatalf("register task: %v", err)
	}
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("start manager: %v", err)
	}

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("expected daemon to start")
	}

	item.Enabled = false
	if err := mgr.Replace(item); err != nil {
		t.Fatalf("replace task: %v", err)
	}

	select {
	case <-runner.stopped:
	case <-time.After(time.Second):
		t.Fatal("expected daemon to stop after disabling")
	}

	if _, err := mgr.RunManual(context.Background(), item.Name, "", false); err == nil {
		t.Fatal("expected disabled daemon to reject manual trigger")
	}
}
