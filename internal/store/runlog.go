package store

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/ismdeep/sys-task-manager/internal/task"
)

type RunLogger interface {
	Write(task.RunResult) error
}

type RunRepository interface {
	SaveRun(context.Context, task.RunResult) error
}

type FileRunLogger struct {
	repo           RunRepository
	maxContentSize int64
	mu             sync.Mutex
}

func NewFileRunLogger(_ string, repo RunRepository) (*FileRunLogger, error) {
	return &FileRunLogger{
		repo:           repo,
		maxContentSize: 1 << 20,
	}, nil
}

func (l *FileRunLogger) Write(result task.RunResult) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	content, err := l.readRetainedLog(result)
	if err != nil {
		return err
	}

	result.Output = content
	if l.repo != nil {
		if err := l.repo.SaveRun(context.Background(), result); err != nil {
			return fmt.Errorf("persist task run: %w", err)
		}
	}

	return nil
}

func (l *FileRunLogger) readRetainedLog(result task.RunResult) (string, error) {
	content := result.Output

	if result.Err != nil && !strings.Contains(content, result.Err.Error()) {
		if content != "" {
			content += "\n"
		}
		content += "ERROR: " + result.Err.Error()
	}

	if int64(len(content)) > l.maxContentSize {
		content = content[len(content)-int(l.maxContentSize):]
	}

	return content, nil
}
