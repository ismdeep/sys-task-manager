package task

import (
	"context"
	"errors"
	"time"
)

type Type string

const (
	TypeDaemon Type = "daemon"
	TypeCron   Type = "cron"
	TypeManual Type = "manual"
)

type Status string

const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusSuccess Status = "success"
	StatusFailed  Status = "failed"
)

type CommandSpec struct {
	Command string
	Args    []string
	WorkDir string
	Env     []string
}

type Task struct {
	ID          int64
	Name        string
	Description string
	CreatedAt   time.Time
	Type        Type
	RunAsUser   string
	CronExpr    string
	Timeout     time.Duration
	Retry       int
	Enabled     bool
	Command     CommandSpec
}

type RunResult struct {
	TaskName    string
	TaskType    Type
	Trigger     TriggerMode
	RunAsUser   string
	Status      Status
	StartedAt   time.Time
	FinishedAt  time.Time
	Attempt     int
	Output      string
	TempLogPath string
	LogPath     string
	Err         error
}

func (r RunResult) Duration() time.Duration {
	if r.FinishedAt.IsZero() || r.StartedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

type TriggerMode string

const (
	TriggerScheduler TriggerMode = "scheduler"
	TriggerManual    TriggerMode = "manual"
	TriggerDaemon    TriggerMode = "daemon"
)

type RunRequest struct {
	Task       Task
	Trigger    TriggerMode
	OverrideAs string
}

type Runner interface {
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

func (t Task) Validate() error {
	switch {
	case t.Name == "":
		return errors.New("task name is required")
	case t.Type != TypeDaemon && t.Type != TypeCron && t.Type != TypeManual:
		return errors.New("task type must be one of daemon, cron, manual")
	case t.Command.Command == "":
		return errors.New("task command is required")
	case t.Type == TypeCron && t.CronExpr == "":
		return errors.New("cron task requires cron expression")
	case t.Type != TypeCron && t.CronExpr != "":
		return errors.New("only cron task can define cron expression")
	case t.Retry < 0:
		return errors.New("retry must be greater than or equal to 0")
	}

	if !t.Enabled {
		return nil
	}

	return nil
}
