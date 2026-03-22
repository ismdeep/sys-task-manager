package store

import (
	"context"
	"fmt"
	"io"
	"os"
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
	result.LogPath = ""
	if l.repo != nil {
		if err := l.repo.SaveRun(context.Background(), result); err != nil {
			return fmt.Errorf("persist task run: %w", err)
		}
	}

	if result.TempLogPath != "" {
		if err := os.Remove(result.TempLogPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove temp log file: %w", err)
		}
	}

	return nil
}

func (l *FileRunLogger) readRetainedLog(result task.RunResult) (string, error) {
	content := result.Output
	if result.TempLogPath != "" {
		raw, err := readLastBytes(result.TempLogPath, l.maxContentSize)
		if err != nil {
			return "", fmt.Errorf("read temp task log: %w", err)
		}
		content = string(raw)
	}

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

func readLastBytes(path string, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	size := info.Size()
	start := int64(0)
	if size > limit {
		start = size - limit
	}

	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	return data, nil
}
