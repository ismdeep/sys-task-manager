package repository

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ismdeep/sys-task-manager/internal/task"
)

func TestRepositoryLoadsWorkspaceTask(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspaces", "hello-world")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "run.sh"), []byte("#!/bin/sh\necho hello\n"), 0o755); err != nil {
		t.Fatalf("write run.sh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "config.json"), []byte(`{
  "type": "cron",
  "run_as_user": "tester",
  "cron": {"spec": "*/15 * * * * *"},
  "timeout_seconds": 5,
  "retry": 1,
  "enabled": true,
  "env": ["HELLO=world"]
}`), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	repo, err := NewRepository(filepath.Join(root, "data"), filepath.Join(root, "workspaces"))
	if err != nil {
		t.Fatalf("new repository: %v", err)
	}
	if err := repo.Init(context.Background()); err != nil {
		t.Fatalf("init repository: %v", err)
	}

	item, err := repo.GetTaskByName(context.Background(), "hello-world")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if item.Name != "hello-world" || item.Type != task.TypeCron || item.CronExpr != "*/15 * * * * *" {
		t.Fatalf("unexpected task: %#v", item)
	}
	if item.Command.Command != "bash" || len(item.Command.Args) != 1 || item.Command.Args[0] != "run.sh" || item.Command.WorkDir != workspace {
		t.Fatalf("unexpected command spec: %#v", item.Command)
	}
	if item.Command.Script != "#!/bin/sh\necho hello\n" {
		t.Fatalf("unexpected run script: %q", item.Command.Script)
	}
}

func TestRepositoryCreateTaskWritesWorkspaceFiles(t *testing.T) {
	root := t.TempDir()
	repo, err := NewRepository(filepath.Join(root, "data"), filepath.Join(root, "workspaces"))
	if err != nil {
		t.Fatalf("new repository: %v", err)
	}
	if err := repo.Init(context.Background()); err != nil {
		t.Fatalf("init repository: %v", err)
	}

	created, err := repo.CreateTask(context.Background(), task.Task{
		Name:      "daemon-worker",
		Type:      task.TypeDaemon,
		RunAsUser: os.Getenv("USER"),
		Timeout:   10 * time.Second,
		Enabled:   true,
		Command: task.CommandSpec{
			Command: "bash",
			Args:    []string{"run.sh"},
			Script:  "#!/usr/bin/env bash\r\necho daemon\r\n",
		},
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	configPath := filepath.Join(root, "workspaces", "daemon-worker", "config.json")
	runPath := filepath.Join(root, "workspaces", "daemon-worker", "run.sh")
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("stat config.json: %v", err)
	}
	rawRun, err := os.ReadFile(runPath)
	if err != nil {
		t.Fatalf("read run.sh: %v", err)
	}
	if string(rawRun) != "#!/usr/bin/env bash\necho daemon\n" {
		t.Fatalf("unexpected run.sh: %q", string(rawRun))
	}
	if !created.Daemon.Autostart || created.Daemon.Restart != "always" {
		t.Fatalf("unexpected daemon config: %#v", created.Daemon)
	}
	if created.Timeout != 0 {
		t.Fatalf("daemon timeout = %s, want 0", created.Timeout)
	}
}

func TestRepositorySaveRunStoresRunWithoutWorkspaceLogFiles(t *testing.T) {
	root := t.TempDir()
	repo, err := NewRepository(filepath.Join(root, "data"), filepath.Join(root, "workspaces"))
	if err != nil {
		t.Fatalf("new repository: %v", err)
	}
	if err := repo.Init(context.Background()); err != nil {
		t.Fatalf("init repository: %v", err)
	}

	created, err := repo.CreateTask(context.Background(), task.Task{
		Name:      "manual-result",
		Type:      task.TypeManual,
		RunAsUser: os.Getenv("USER"),
		Enabled:   true,
		Command: task.CommandSpec{
			Command: "bash",
			Args:    []string{"run.sh"},
			Script:  "#!/usr/bin/env bash\necho result\n",
		},
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	startedAt := time.Now().UTC()
	if err := repo.SaveRun(context.Background(), task.RunResult{
		TaskName:   created.Name,
		TaskType:   created.Type,
		Trigger:    task.TriggerManual,
		RunAsUser:  created.RunAsUser,
		RunID:      "12345",
		Status:     task.StatusSuccess,
		StartedAt:  startedAt,
		FinishedAt: startedAt.Add(time.Second),
		Attempt:    1,
		Output:     "hello\n",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	resultPath := filepath.Join(created.Command.WorkDir, "log", "12345.result")
	if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
		t.Fatalf("result file exists or stat failed: %v", err)
	}
	logPath := filepath.Join(created.Command.WorkDir, "log", "12345.log")
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("log file exists or stat failed: %v", err)
	}
	runsPath := filepath.Join(root, "data", "runs.json")
	if _, err := os.Stat(runsPath); !os.IsNotExist(err) {
		t.Fatalf("runs file exists or stat failed: %v", err)
	}

	runs, err := repo.ListRunsByTask(context.Background(), created.Name, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("run count = %d, want 1", len(runs))
	}
	if runs[0].ID != 12345 || runs[0].LogPath != "" || runs[0].LogContent != "hello\n" {
		t.Fatalf("unexpected run record: %#v", runs[0])
	}
}

func TestRepositoryStoresSessionsInMemory(t *testing.T) {
	root := t.TempDir()
	repo, err := NewRepository(filepath.Join(root, "data"), filepath.Join(root, "workspaces"))
	if err != nil {
		t.Fatalf("new repository: %v", err)
	}
	if err := repo.Init(context.Background()); err != nil {
		t.Fatalf("init repository: %v", err)
	}
	if err := repo.EnsureAdmin(context.Background(), "admin", "hash"); err != nil {
		t.Fatalf("ensure admin: %v", err)
	}
	user, err := repo.GetUserByUsername(context.Background(), "admin")
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}

	if err := repo.CreateSession(context.Background(), user.ID, "active-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create active session: %v", err)
	}
	if err := repo.CreateSession(context.Background(), user.ID, "expired-token", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("create expired session: %v", err)
	}

	session, err := repo.GetSession(context.Background(), "active-token")
	if err != nil {
		t.Fatalf("get active session: %v", err)
	}
	if session.Username != "admin" {
		t.Fatalf("session username = %q, want admin", session.Username)
	}

	sessionsPath := filepath.Join(root, "data", "sessions.json")
	if _, err := os.Stat(sessionsPath); !os.IsNotExist(err) {
		t.Fatalf("sessions file exists or stat failed: %v", err)
	}

	if err := repo.CleanupExpiredSessions(context.Background()); err != nil {
		t.Fatalf("cleanup expired sessions: %v", err)
	}
	if _, err := repo.GetSession(context.Background(), "expired-token"); err != ErrNotFound {
		t.Fatalf("expired session err = %v, want ErrNotFound", err)
	}

	if err := repo.DeleteSession(context.Background(), "active-token"); err != nil {
		t.Fatalf("delete active session: %v", err)
	}
	if _, err := repo.GetSession(context.Background(), "active-token"); err != ErrNotFound {
		t.Fatalf("active session err = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(sessionsPath); !os.IsNotExist(err) {
		t.Fatalf("sessions file exists or stat failed after updates: %v", err)
	}
}
