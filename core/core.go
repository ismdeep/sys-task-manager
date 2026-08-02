package core

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ismdeep/log"
	"go.uber.org/zap"
)

type Core struct {
	TaskManager          TaskManager
	tasks                map[string]Task
	runners              map[string]TaskRunner
	CreateTaskRunnerFunc func(Task) TaskRunner
	wg                   sync.WaitGroup
}

type Task struct {
	Name   string     `json:"name"`
	Type   string     `json:"type"` // e.g. cron, daemon, manual
	Cron   TaskCron   `json:"cron"`
	Daemon TaskDaemon `json:"daemon"`
	Manual TaskManual `json:"manual"`
}

type TaskCron struct {
	Timeout   time.Duration `json:"timeout"`
	Schedules []string      `json:"schedules"`
}

type TaskDaemon struct {
	IsAutostart bool   `json:"is_autostart"`
	Restart     string `json:"restart"`
}

type TaskManual struct {
	Timeout time.Duration `json:"timeout"`
}

func NewCore(taskManager TaskManager, createTaskRunnerFunc func(Task) TaskRunner) (*Core, error) {
	return &Core{
		TaskManager:          taskManager,
		tasks:                make(map[string]Task),
		runners:              make(map[string]TaskRunner),
		CreateTaskRunnerFunc: createTaskRunnerFunc,
		wg:                   sync.WaitGroup{},
	}, nil
}

func (receiver *Core) Run() error {
	ctx := context.Background()

	events := receiver.TaskManager.EventChan()
	for event := range events {
		if err := receiver.ProcessTaskManagerEvent(event); err != nil {
			log.WithContext(ctx).Error("failed to process event", zap.Error(err))
		}
	}
	receiver.wg.Wait()
	return nil
}

func (receiver *Core) ProcessTaskManagerEvent(event TaskManagerEvent) error {
	ctx := context.Background()
	receiver.wg.Add(1)

	switch event.Event {
	case "new":
		taskName := event.Task.Name
		if _, ok := receiver.tasks[taskName]; ok {
			receiver.wg.Done()
			return errors.New("task already exists")
		}
		receiver.tasks[taskName] = event.Task
		receiver.runners[taskName] = receiver.CreateTaskRunnerFunc(event.Task)

		go func(taskName string) {
			defer receiver.wg.Done()
			if err := receiver.runners[taskName].Run(); err != nil {
				log.WithContext(ctx).Error("failed to run task", zap.String("task", taskName), zap.Error(err))
			}
		}(taskName)
	case "change":
		taskName := event.Task.Name
		if _, ok := receiver.tasks[taskName]; !ok {
			receiver.wg.Done()
			return errors.New("task not exists")
		}
		if err := receiver.runners[taskName].Stop(); err != nil {
			log.WithContext(ctx).Error("failed to stop task", zap.String("task", taskName), zap.Error(err))
		}
		receiver.tasks[taskName] = event.Task
		receiver.runners[taskName] = receiver.CreateTaskRunnerFunc(event.Task)
		go func(taskName string) {
			defer receiver.wg.Done()
			if err := receiver.runners[taskName].Run(); err != nil {
				log.WithContext(ctx).Error("failed to run task", zap.String("task", taskName), zap.Error(err))
			}
		}(taskName)
	case "remove":
		taskName := event.Task.Name
		if _, ok := receiver.tasks[taskName]; !ok {
			receiver.wg.Done()
			return errors.New("task not exists")
		}
		if err := receiver.runners[taskName].Stop(); err != nil {
			log.WithContext(ctx).Error("failed to stop task", zap.String("task", taskName), zap.Error(err))
		}
		delete(receiver.runners, taskName)
		delete(receiver.tasks, taskName)
		receiver.wg.Done()
	default:
		log.WithContext(ctx).Error("unknown event", zap.String("event", event.Task.Type))
		receiver.wg.Done()
	}

	return nil
}
