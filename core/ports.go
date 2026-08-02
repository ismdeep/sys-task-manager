package core

type TaskManager interface {
	EventChan() <-chan TaskManagerEvent
	Add(task Task) error
	Remove(task Task) error
	Update(task Task) error
}

type TaskManagerEvent struct {
	Event string
	Task  Task
}

type TaskRunner interface {
	Run() error
	Stop() error
	LogChannel() <-chan string
}
