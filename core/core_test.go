package core

import (
	"fmt"
	"testing"
	"time"
)

type TaskManagerMock struct {
	c chan TaskManagerEvent
}

type TaskRunnerMock struct {
}

func (receiver *TaskRunnerMock) Run() error {
	fmt.Println("Runner Mock Run")
	return nil
}

func (receiver *TaskRunnerMock) Stop() error {
	fmt.Println("Runner Mock Stop")
	return nil
}

func (receiver *TaskRunnerMock) LogChannel() <-chan string {
	return nil
}

func NewTaskManagerMock() *TaskManagerMock {

	c := make(chan TaskManagerEvent, 1024)

	go func() {
		c <- TaskManagerEvent{
			Event: "new",
			Task: Task{
				Name:   "hello-world",
				Type:   "manual",
				Cron:   TaskCron{},
				Daemon: TaskDaemon{},
				Manual: TaskManual{
					Timeout: 3 * time.Second,
				},
			},
		}
		close(c)
	}()

	return &TaskManagerMock{
		c: c,
	}
}

func (receiver *TaskManagerMock) EventChan() <-chan TaskManagerEvent {
	return receiver.c
}

func (receiver *TaskManagerMock) Add(task Task) error {
	return nil
}

func (receiver *TaskManagerMock) Remove(task Task) error {
	return nil
}

func (receiver *TaskManagerMock) Update(task Task) error {
	return nil
}

func TestNewCore(t *testing.T) {
	taskManager := NewTaskManagerMock()
	createTaskRunnerFunc := func(task Task) TaskRunner {
		fmt.Println(task.Name)
		return &TaskRunnerMock{}
	}
	core, err := NewCore(taskManager, createTaskRunnerFunc)
	if err != nil {
		t.Error(err)
	}
	if err := core.Run(); err != nil {
		t.Error(err)
	}
}
