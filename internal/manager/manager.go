package manager

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ismdeep/log"
	"go.uber.org/zap"

	"github.com/ismdeep/sys-task-manager/internal/auth"
	"github.com/ismdeep/sys-task-manager/internal/scheduler"
	"github.com/ismdeep/sys-task-manager/internal/store"
	"github.com/ismdeep/sys-task-manager/internal/task"
)

var ErrTaskAlreadyRunning = errors.New("task is already running")

type Manager struct {
	auth       *auth.Authorizer
	runner     task.Runner
	runLogger  store.RunLogger
	cron       *scheduler.Scheduler
	maxRunning chan struct{}

	mu       sync.RWMutex
	tasks    map[string]task.Task
	entries  map[string]scheduler.EntryID
	running  map[string]struct{}
	cancels  map[string]context.CancelFunc
	done     map[string]chan struct{}
	statuses map[string]task.RunResult
	started  bool
	startCtx context.Context
}

func New(
	authz *auth.Authorizer,
	runner task.Runner,
	runLogger store.RunLogger,
	maxConcurrency int,
) *Manager {
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}

	return &Manager{
		auth:       authz,
		runner:     runner,
		runLogger:  runLogger,
		cron:       scheduler.New(),
		maxRunning: make(chan struct{}, maxConcurrency),
		tasks:      make(map[string]task.Task),
		entries:    make(map[string]scheduler.EntryID),
		running:    make(map[string]struct{}),
		cancels:    make(map[string]context.CancelFunc),
		done:       make(map[string]chan struct{}),
		statuses:   make(map[string]task.RunResult),
	}
}

func (m *Manager) Register(t task.Task) error {
	if err := t.Validate(); err != nil {
		return err
	}

	if _, err := m.auth.ValidateRunAs(t.RunAsUser); err != nil {
		return fmt.Errorf("register task %q: %w", t.Name, err)
	}

	m.mu.Lock()

	if _, exists := m.tasks[t.Name]; exists {
		m.mu.Unlock()
		return fmt.Errorf("task %q already exists", t.Name)
	}

	m.tasks[t.Name] = t
	m.statuses[t.Name] = task.RunResult{
		TaskName:  t.Name,
		TaskType:  t.Type,
		Trigger:   task.TriggerMode(""),
		RunAsUser: t.RunAsUser,
		Status:    task.StatusPending,
	}

	if t.Enabled && t.Type == task.TypeCron {
		entryID, err := m.cron.AddFunc(t.CronExpr, func() {
			if _, err := m.runTask(context.Background(), t, task.TriggerScheduler, "", false); err != nil {
				log.WithContext(context.Background()).Error("cron task failed", zap.String("task", t.Name), zap.Error(err))
			}
		})
		if err != nil {
			delete(m.tasks, t.Name)
			delete(m.statuses, t.Name)
			m.mu.Unlock()
			return fmt.Errorf("register cron %q: %w", t.Name, err)
		}

		m.entries[t.Name] = entryID
	}

	started := m.started
	startCtx := m.startCtx
	m.mu.Unlock()

	if started && t.Enabled && t.Type == task.TypeDaemon {
		if _, err := m.runTask(startCtx, t, task.TriggerDaemon, "", true); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) Start(ctx context.Context) error {
	m.cron.Start()

	m.mu.Lock()
	m.started = true
	m.startCtx = ctx
	m.mu.Unlock()

	m.mu.RLock()
	var daemons []task.Task
	for _, t := range m.tasks {
		if t.Type == task.TypeDaemon && t.Enabled {
			daemons = append(daemons, t)
		}
	}
	m.mu.RUnlock()

	for _, t := range daemons {
		if _, err := m.runTask(ctx, t, task.TriggerDaemon, "", true); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) Shutdown(ctx context.Context) error {
	stoppedCtx := m.cron.Stop()

	select {
	case <-stoppedCtx.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) RunManual(ctx context.Context, name string, overrideAs string, async bool) (task.RunResult, error) {
	t, err := m.task(name)
	if err != nil {
		return task.RunResult{}, err
	}

	if !t.Enabled {
		return task.RunResult{}, fmt.Errorf("task %q is disabled", name)
	}

	if t.Type != task.TypeManual && t.Type != task.TypeCron {
		return task.RunResult{}, fmt.Errorf("task %q does not support manual trigger", name)
	}

	return m.runTask(ctx, t, task.TriggerManual, overrideAs, async)
}

func (m *Manager) Status(name string) (task.RunResult, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result, ok := m.statuses[name]
	return result, ok
}

func (m *Manager) ListTasks() []task.Task {
	m.mu.RLock()
	defer m.mu.RUnlock()

	items := make([]task.Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		items = append(items, t)
	}
	return items
}

func (m *Manager) Replace(t task.Task) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if _, err := m.auth.ValidateRunAs(t.RunAsUser); err != nil {
		return fmt.Errorf("replace task %q: %w", t.Name, err)
	}

	if err := m.stopTaskRunIfNeeded(t); err != nil {
		return err
	}

	m.mu.Lock()
	old, exists := m.tasks[t.Name]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("task %q not found", t.Name)
	}

	if entryID, ok := m.entries[t.Name]; ok {
		m.cron.Remove(entryID)
		delete(m.entries, t.Name)
	}

	m.tasks[t.Name] = t
	m.statuses[t.Name] = task.RunResult{
		TaskName:  t.Name,
		TaskType:  t.Type,
		Trigger:   task.TriggerMode(""),
		RunAsUser: t.RunAsUser,
		Status:    task.StatusPending,
	}

	if t.Enabled && t.Type == task.TypeCron {
		entryID, err := m.cron.AddFunc(t.CronExpr, func() {
			if _, err := m.runTask(context.Background(), t, task.TriggerScheduler, "", false); err != nil {
				log.WithContext(context.Background()).Error("cron task failed", zap.String("task", t.Name), zap.Error(err))
			}
		})
		if err != nil {
			m.tasks[t.Name] = old
			m.mu.Unlock()
			return fmt.Errorf("replace cron %q: %w", t.Name, err)
		}
		m.entries[t.Name] = entryID
	}

	started := m.started
	startCtx := m.startCtx
	m.mu.Unlock()

	if started && t.Enabled && t.Type == task.TypeDaemon && (old.Type != task.TypeDaemon || !old.Enabled) {
		if _, err := m.runTask(startCtx, t, task.TriggerDaemon, "", true); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) stopTaskRunIfNeeded(next task.Task) error {
	for {
		m.mu.Lock()
		current, exists := m.tasks[next.Name]
		if !exists {
			m.mu.Unlock()
			return fmt.Errorf("task %q not found", next.Name)
		}

		shouldStop := current.Type == task.TypeDaemon && current.Enabled && (!next.Enabled || next.Type != task.TypeDaemon)
		done := m.done[next.Name]
		cancel := m.cancels[next.Name]
		if !shouldStop || done == nil || cancel == nil {
			m.mu.Unlock()
			return nil
		}

		m.mu.Unlock()
		cancel()
		<-done
	}
}

func (m *Manager) Remove(name string) (task.Task, error) {
	for {
		m.mu.Lock()
		current, exists := m.tasks[name]
		if !exists {
			m.mu.Unlock()
			return task.Task{}, fmt.Errorf("task %q not found", name)
		}

		done := m.done[name]
		cancel := m.cancels[name]
		if done != nil && cancel != nil {
			m.mu.Unlock()
			cancel()
			<-done
			continue
		}

		if entryID, ok := m.entries[name]; ok {
			m.cron.Remove(entryID)
			delete(m.entries, name)
		}

		delete(m.tasks, name)
		delete(m.statuses, name)

		m.mu.Unlock()
		return current, nil
	}
}

func (m *Manager) task(name string) (task.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	t, ok := m.tasks[name]
	if !ok {
		return task.Task{}, fmt.Errorf("task %q not found", name)
	}

	return t, nil
}

func (m *Manager) runTask(
	ctx context.Context,
	t task.Task,
	trigger task.TriggerMode,
	overrideAs string,
	async bool,
) (task.RunResult, error) {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	if err := m.reserve(t.Name, cancel, done); err != nil {
		cancel()
		return task.RunResult{}, err
	}

	run := func() (task.RunResult, error) {
		defer close(done)
		acquiredSlot := false
		defer func() {
			if acquiredSlot {
				<-m.maxRunning
			}
			m.release(t.Name)
		}()

		select {
		case m.maxRunning <- struct{}{}:
			acquiredSlot = true
		case <-runCtx.Done():
			result := task.RunResult{
				TaskName:  t.Name,
				TaskType:  t.Type,
				Trigger:   trigger,
				RunAsUser: pickRunAs(t.RunAsUser, overrideAs),
				Status:    task.StatusFailed,
				Err:       fmt.Errorf("task canceled: %w", runCtx.Err()),
			}
			m.setStatus(t.Name, result)
			m.logResult(result, result.Err)
			return result, result.Err
		}

		m.setStatus(t.Name, task.RunResult{
			TaskName:  t.Name,
			TaskType:  t.Type,
			Trigger:   trigger,
			RunAsUser: pickRunAs(t.RunAsUser, overrideAs),
			Status:    task.StatusRunning,
		})

		result, err := m.runner.Run(runCtx, task.RunRequest{
			Task:       t,
			Trigger:    trigger,
			OverrideAs: overrideAs,
		})
		if err != nil && result.TaskName == "" {
			result = task.RunResult{
				TaskName:  t.Name,
				TaskType:  t.Type,
				Trigger:   trigger,
				RunAsUser: pickRunAs(t.RunAsUser, overrideAs),
				Status:    task.StatusFailed,
				Err:       err,
			}
		}
		result.Trigger = trigger

		m.setStatus(t.Name, result)
		m.logResult(result, err)
		return result, err
	}

	if !async {
		return run()
	}

	go func() {
		if _, err := run(); err != nil {
			log.WithContext(runCtx).Warn("async task failed", zap.String("task", t.Name), zap.Error(err))
		}
	}()

	return task.RunResult{
		TaskName:  t.Name,
		TaskType:  t.Type,
		Trigger:   trigger,
		RunAsUser: pickRunAs(t.RunAsUser, overrideAs),
		Status:    task.StatusRunning,
	}, nil
}

func (m *Manager) reserve(name string, cancel context.CancelFunc, done chan struct{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.running[name]; exists {
		return fmt.Errorf("%w: %s", ErrTaskAlreadyRunning, name)
	}

	m.running[name] = struct{}{}
	m.cancels[name] = cancel
	m.done[name] = done
	return nil
}

func (m *Manager) release(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, name)
	delete(m.cancels, name)
	delete(m.done, name)
}

func (m *Manager) setStatus(name string, result task.RunResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses[name] = result
}

func (m *Manager) logResult(result task.RunResult, err error) {
	fields := []zap.Field{
		zap.String("task", result.TaskName),
		zap.String("type", string(result.TaskType)),
		zap.String("run_as", result.RunAsUser),
		zap.String("status", string(result.Status)),
		zap.Int("attempt", result.Attempt),
		zap.String("duration", result.Duration().String()),
	}
	if err != nil {
		fields = append(fields, zap.Error(err))
	}

	log.WithContext(context.Background()).Info(
		"task finished",
		fields...,
	)

	if m.runLogger != nil {
		if writeErr := m.runLogger.Write(result); writeErr != nil {
			log.WithContext(context.Background()).Error("persist task log failed", zap.String("task", result.TaskName), zap.Error(writeErr))
		}
	}
}

func pickRunAs(taskRunAs, overrideAs string) string {
	if overrideAs != "" {
		return overrideAs
	}
	return taskRunAs
}
