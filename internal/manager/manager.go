package manager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ismdeep/log"
	"go.uber.org/zap"

	"github.com/ismdeep/sys-task-manager/core"
	"github.com/ismdeep/sys-task-manager/internal/auth"
	"github.com/ismdeep/sys-task-manager/internal/scheduler"
	"github.com/ismdeep/sys-task-manager/internal/store"
	"github.com/ismdeep/sys-task-manager/internal/task"
)

var ErrTaskAlreadyRunning = errors.New("task is already running")

type Manager struct {
	auth      *auth.Authorizer
	runner    task.Runner
	runLogger store.RunLogger

	core       *core.Core
	coreEvents *coreEventSource

	maxRunning chan struct{}

	mu          sync.RWMutex
	tasks       map[string]task.Task
	runners     map[string]*managedTaskRunner
	running     map[string]struct{}
	cancels     map[string]context.CancelFunc
	done        map[string]chan struct{}
	statuses    map[string]task.RunResult
	started     bool
	startCtx    context.Context
	startNotify chan struct{}
}

type coreEventSource struct {
	ch chan core.TaskManagerEvent
}

func newCoreEventSource() *coreEventSource {
	return &coreEventSource{ch: make(chan core.TaskManagerEvent, 1024)}
}

func (s *coreEventSource) EventChan() <-chan core.TaskManagerEvent { return s.ch }
func (s *coreEventSource) Add(t core.Task) error {
	s.ch <- core.TaskManagerEvent{Event: "new", Task: t}
	return nil
}
func (s *coreEventSource) Remove(t core.Task) error {
	s.ch <- core.TaskManagerEvent{Event: "remove", Task: t}
	return nil
}
func (s *coreEventSource) Update(t core.Task) error {
	s.ch <- core.TaskManagerEvent{Event: "change", Task: t}
	return nil
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

	m := &Manager{
		auth:        authz,
		runner:      runner,
		runLogger:   runLogger,
		coreEvents:  newCoreEventSource(),
		maxRunning:  make(chan struct{}, maxConcurrency),
		tasks:       make(map[string]task.Task),
		runners:     make(map[string]*managedTaskRunner),
		running:     make(map[string]struct{}),
		cancels:     make(map[string]context.CancelFunc),
		done:        make(map[string]chan struct{}),
		statuses:    make(map[string]task.RunResult),
		startNotify: make(chan struct{}),
	}
	engine, err := core.NewCore(m.coreEvents, m.newManagedRunner)
	if err != nil {
		panic(err)
	}
	m.core = engine
	return m
}

func (m *Manager) Register(t task.Task) error {
	if err := m.validateTask(t, "register"); err != nil {
		return err
	}

	m.mu.Lock()
	if _, exists := m.tasks[t.Name]; exists {
		m.mu.Unlock()
		return fmt.Errorf("task %q already exists", t.Name)
	}
	m.tasks[t.Name] = t
	m.statuses[t.Name] = pendingResult(t)
	m.mu.Unlock()

	if err := m.core.ProcessTaskManagerEvent(core.TaskManagerEvent{Event: "new", Task: toCoreTask(t)}); err != nil {
		m.mu.Lock()
		delete(m.tasks, t.Name)
		delete(m.statuses, t.Name)
		m.mu.Unlock()
		return err
	}
	return nil
}

func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if !m.started {
		m.started = true
		m.startCtx = ctx
		close(m.startNotify)
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) Shutdown(ctx context.Context) error {
	names := m.taskNames()
	for _, name := range names {
		err := m.core.ProcessTaskManagerEvent(core.TaskManagerEvent{
			Event: "remove",
			Task:  core.Task{Name: name},
		})
		if err != nil {
			return err
		}
	}

	for _, name := range names {
		if err := m.waitForTaskStopped(ctx, name); err != nil {
			return err
		}
	}
	return nil
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
	if err := m.validateTask(t, "replace"); err != nil {
		return err
	}

	m.mu.Lock()
	old, exists := m.tasks[t.Name]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("task %q not found", t.Name)
	}
	m.tasks[t.Name] = t
	m.statuses[t.Name] = pendingResult(t)
	m.mu.Unlock()

	if err := m.core.ProcessTaskManagerEvent(core.TaskManagerEvent{Event: "change", Task: toCoreTask(t)}); err != nil {
		m.mu.Lock()
		m.tasks[t.Name] = old
		m.statuses[t.Name] = pendingResult(old)
		m.mu.Unlock()
		return err
	}
	return nil
}

func (m *Manager) Remove(name string) (task.Task, error) {
	current, err := m.task(name)
	if err != nil {
		return task.Task{}, err
	}

	if err := m.core.ProcessTaskManagerEvent(core.TaskManagerEvent{Event: "remove", Task: toCoreTask(current)}); err != nil {
		return task.Task{}, err
	}

	m.mu.Lock()
	delete(m.tasks, name)
	delete(m.statuses, name)
	m.mu.Unlock()
	return current, nil
}

func (m *Manager) validateTask(t task.Task, action string) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if t.Enabled && t.Type == task.TypeCron {
		if _, err := scheduler.New().AddFunc(t.CronExpr, func() {}); err != nil {
			return fmt.Errorf("%s cron %q: %w", action, t.Name, err)
		}
	}
	if _, err := m.auth.ValidateRunAs(t.RunAsUser); err != nil {
		return fmt.Errorf("%s task %q: %w", action, t.Name, err)
	}
	return nil
}

func (m *Manager) newManagedRunner(t core.Task) core.TaskRunner {
	runner := &managedTaskRunner{
		manager: m,
		name:    t.Name,
		ready:   make(chan struct{}),
		done:    make(chan struct{}),
	}

	m.mu.Lock()
	m.runners[t.Name] = runner
	m.mu.Unlock()
	return runner
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

func (m *Manager) taskNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.tasks))
	for name := range m.tasks {
		names = append(names, name)
	}
	return names
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

		runID := newRunID()
		m.setStatus(t.Name, task.RunResult{
			TaskName:  t.Name,
			TaskType:  t.Type,
			Trigger:   trigger,
			RunAsUser: pickRunAs(t.RunAsUser, overrideAs),
			RunID:     runID,
			Status:    task.StatusRunning,
		})

		result, err := m.runner.Run(runCtx, task.RunRequest{
			Task:       t,
			Trigger:    trigger,
			OverrideAs: overrideAs,
			RunID:      runID,
			OnUpdate: func(result task.RunResult) {
				m.setStatus(t.Name, result)
			},
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

func (m *Manager) waitForTaskStopped(ctx context.Context, name string) error {
	for {
		m.mu.RLock()
		done := m.done[name]
		m.mu.RUnlock()
		if done == nil {
			return nil
		}

		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *Manager) cancelTaskRun(name string) {
	m.mu.RLock()
	cancel := m.cancels[name]
	m.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
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
	if result.Output != "" {
		fields = append(fields, zap.String("output", result.Output))
	}

	log.WithContext(context.Background()).Info("task finished", fields...)

	if m.runLogger != nil {
		if writeErr := m.runLogger.Write(result); writeErr != nil {
			log.WithContext(context.Background()).Error("persist task log failed", zap.String("task", result.TaskName), zap.Error(writeErr))
		}
	}
}

type managedTaskRunner struct {
	manager *Manager
	name    string

	mu     sync.Mutex
	cancel context.CancelFunc
	ready  chan struct{}
	done   chan struct{}
}

func (r *managedTaskRunner) Run() error {
	ctx, cancel := context.WithCancel(context.Background())

	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()
	close(r.ready)
	defer close(r.done)

	if err := r.waitForStart(ctx); err != nil {
		return err
	}

	t, err := r.manager.task(r.name)
	if err != nil {
		return err
	}
	if !t.Enabled {
		<-ctx.Done()
		return nil
	}

	switch t.Type {
	case task.TypeCron:
		return r.runCron(ctx, t)
	case task.TypeDaemon:
		return r.runDaemon(ctx, t)
	case task.TypeManual:
		<-ctx.Done()
		return nil
	default:
		return fmt.Errorf("unknown task type %q", t.Type)
	}
}

func (r *managedTaskRunner) Stop() error {
	r.mu.Lock()
	ready := r.ready
	done := r.done
	r.mu.Unlock()

	select {
	case <-ready:
	case <-done:
		return nil
	}

	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}

	r.manager.cancelTaskRun(r.name)
	return r.manager.waitForTaskStopped(context.Background(), r.name)
}

func (r *managedTaskRunner) LogChannel() <-chan string {
	return nil
}

func (r *managedTaskRunner) waitForStart(ctx context.Context) error {
	r.manager.mu.RLock()
	started := r.manager.started
	startNotify := r.manager.startNotify
	r.manager.mu.RUnlock()

	if started {
		return nil
	}

	select {
	case <-startNotify:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *managedTaskRunner) runCron(ctx context.Context, t task.Task) error {
	cron := scheduler.New()
	if _, err := cron.AddFunc(t.CronExpr, func() {
		if _, err := r.manager.runTask(ctx, t, task.TriggerScheduler, "", false); err != nil {
			log.WithContext(ctx).Error("cron task failed", zap.String("task", t.Name), zap.Error(err))
		}
	}); err != nil {
		return fmt.Errorf("register cron %q: %w", t.Name, err)
	}
	cron.Start()

	<-ctx.Done()
	stopped := cron.Stop()
	<-stopped.Done()
	return nil
}

func (r *managedTaskRunner) runDaemon(ctx context.Context, t task.Task) error {
	for {
		current, err := r.manager.task(t.Name)
		if err != nil {
			return nil
		}
		if !current.Enabled || current.Type != task.TypeDaemon {
			return nil
		}

		_, err = r.manager.runTask(ctx, current, task.TriggerDaemon, "", false)
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil
		}
		if err != nil {
			log.WithContext(ctx).Warn("daemon task failed", zap.String("task", t.Name), zap.Error(err))
		}

		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return nil
		}
	}
}

func pendingResult(t task.Task) task.RunResult {
	return task.RunResult{
		TaskName:  t.Name,
		TaskType:  t.Type,
		Trigger:   task.TriggerMode(""),
		RunAsUser: t.RunAsUser,
		Status:    task.StatusPending,
	}
}

func toCoreTask(t task.Task) core.Task {
	coreTask := core.Task{
		Name: t.Name,
		Type: string(t.Type),
		Cron: core.TaskCron{
			Timeout: t.Timeout,
		},
		Daemon: core.TaskDaemon{
			IsAutostart: t.Daemon.Autostart,
			Restart:     t.Daemon.Restart,
		},
		Manual: core.TaskManual{
			Timeout: t.Timeout,
		},
	}
	if t.CronExpr != "" {
		coreTask.Cron.Schedules = []string{t.CronExpr}
	}
	return coreTask
}

func newRunID() string {
	return fmt.Sprintf("%d", time.Now().UTC().UnixNano())
}

func pickRunAs(taskRunAs, overrideAs string) string {
	if overrideAs != "" {
		return overrideAs
	}
	return taskRunAs
}
