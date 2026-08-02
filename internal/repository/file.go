package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ismdeep/sys-task-manager/internal/task"
)

var ErrNotFound = errors.New("record not found")

type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Session struct {
	UserID    int64     `json:"user_id"`
	Username  string    `json:"username"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type RunRecord struct {
	ID         int64
	TaskName   string
	TaskType   task.Type
	Trigger    task.TriggerMode
	RunAsUser  string
	RunID      string
	Status     task.Status
	Attempt    int
	StartedAt  time.Time
	FinishedAt time.Time
	DurationMS int64
	ErrorMsg   string
	LogPath    string
	LogContent string
}

func (r RunRecord) DurationSeconds() float64 {
	return float64(r.DurationMS) / 1000
}

type TaskRunSummary struct {
	TaskName string
	RunCount int
	LastRun  *RunRecord
}

type Setting struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Repository struct {
	dataDir       string
	workspacesDir string
	sessions      []Session
	runs          []RunRecord
	mu            sync.Mutex
}

type taskConfig struct {
	Description    string       `json:"description,omitempty"`
	Type           task.Type    `json:"type"`
	RunAsUser      string       `json:"run_as_user,omitempty"`
	TimeoutSeconds int64        `json:"timeout_seconds,omitempty"`
	Retry          int          `json:"retry,omitempty"`
	Enabled        *bool        `json:"enabled,omitempty"`
	Cron           cronConfig   `json:"cron,omitempty"`
	Daemon         daemonConfig `json:"daemon,omitempty"`
	Env            []string     `json:"env,omitempty"`
}

type cronConfig struct {
	Spec string `json:"spec,omitempty"`
}

type daemonConfig struct {
	Autostart bool   `json:"autostart,omitempty"`
	Restart   string `json:"restart,omitempty"`
}

func NewRepository(dataDir, workspacesDir string) (*Repository, error) {
	repo := &Repository{
		dataDir:       dataDir,
		workspacesDir: workspacesDir,
	}
	return repo, nil
}

func (r *Repository) Close() error {
	return nil
}

func (r *Repository) Init(context.Context) error {
	if err := os.MkdirAll(r.dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if err := os.MkdirAll(r.workspacesDir, 0o755); err != nil {
		return fmt.Errorf("create workspaces dir: %w", err)
	}
	return nil
}

func (r *Repository) GetSetting(_ context.Context, key string) (Setting, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	settings, err := r.loadSettingsLocked()
	if err != nil {
		return Setting{}, err
	}
	value, ok := settings[key]
	if !ok {
		return Setting{}, ErrNotFound
	}
	return Setting{Key: key, Value: value}, nil
}

func (r *Repository) SetSetting(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	settings, err := r.loadSettingsLocked()
	if err != nil {
		return err
	}
	settings[key] = value
	return writeJSONFile(r.settingsPath(), settings)
}

func (r *Repository) EnsureAdmin(ctx context.Context, username, passwordHash string) error {
	if _, err := r.GetUserByUsername(ctx, username); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	users, err := r.loadUsersLocked()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	users = append(users, User{
		ID:           nextUserID(users),
		Username:     username,
		PasswordHash: passwordHash,
		CreatedAt:    now,
		UpdatedAt:    now,
	})
	return writeJSONFile(r.usersPath(), users)
}

func (r *Repository) GetUserByUsername(_ context.Context, username string) (User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	users, err := r.loadUsersLocked()
	if err != nil {
		return User{}, err
	}
	for _, u := range users {
		if u.Username == username {
			return u, nil
		}
	}
	return User{}, ErrNotFound
}

func (r *Repository) UpdateUserPasswordHash(_ context.Context, userID int64, passwordHash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	users, err := r.loadUsersLocked()
	if err != nil {
		return err
	}
	for i := range users {
		if users[i].ID == userID {
			users[i].PasswordHash = passwordHash
			users[i].UpdatedAt = time.Now().UTC()
			return writeJSONFile(r.usersPath(), users)
		}
	}
	return ErrNotFound
}

func (r *Repository) CreateSession(_ context.Context, userID int64, token string, expiresAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	users, err := r.loadUsersLocked()
	if err != nil {
		return err
	}
	username := ""
	for _, u := range users {
		if u.ID == userID {
			username = u.Username
			break
		}
	}
	if username == "" {
		return ErrNotFound
	}

	r.sessions = append(r.sessions, Session{
		UserID:    userID,
		Username:  username,
		Token:     token,
		ExpiresAt: expiresAt.UTC(),
	})
	return nil
}

func (r *Repository) GetSession(_ context.Context, token string) (Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, s := range r.sessions {
		if s.Token == token {
			return s, nil
		}
	}
	return Session{}, ErrNotFound
}

func (r *Repository) DeleteSession(_ context.Context, token string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.sessions = filterSessions(r.sessions, func(s Session) bool { return s.Token != token })
	return nil
}

func (r *Repository) DeleteSessionsByUserID(_ context.Context, userID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.sessions = filterSessions(r.sessions, func(s Session) bool { return s.UserID != userID })
	return nil
}

func (r *Repository) CleanupExpiredSessions(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	r.sessions = filterSessions(r.sessions, func(s Session) bool { return s.ExpiresAt.After(now) })
	return nil
}

func (r *Repository) EnsureTask(ctx context.Context, t task.Task) (task.Task, error) {
	existing, err := r.GetTaskByName(ctx, t.Name)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return task.Task{}, err
	}
	return r.CreateTask(ctx, t)
}

func (r *Repository) CreateTask(_ context.Context, t task.Task) (task.Task, error) {
	if _, err := os.Stat(r.taskDir(t.Name)); err == nil {
		return task.Task{}, fmt.Errorf("task %q already exists", t.Name)
	} else if !os.IsNotExist(err) {
		return task.Task{}, err
	}
	return r.writeTask(t)
}

func (r *Repository) UpdateTaskByName(_ context.Context, name string, t task.Task) (task.Task, error) {
	if name != t.Name {
		return task.Task{}, errors.New("renaming workspace tasks is not supported")
	}
	if _, err := os.Stat(r.taskDir(name)); os.IsNotExist(err) {
		return task.Task{}, ErrNotFound
	} else if err != nil {
		return task.Task{}, err
	}
	return r.writeTask(t)
}

func (r *Repository) ListTasks(context.Context) ([]task.Task, error) {
	entries, err := os.ReadDir(r.workspacesDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read workspaces: %w", err)
	}

	var tasks []task.Task
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		item, err := r.readTask(entry.Name())
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, item)
	}

	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Name < tasks[j].Name })
	return tasks, nil
}

func (r *Repository) GetTaskByName(_ context.Context, name string) (task.Task, error) {
	return r.readTask(name)
}

func (r *Repository) DeleteTaskByName(_ context.Context, name string) error {
	if err := os.RemoveAll(r.taskDir(name)); err != nil {
		return fmt.Errorf("delete workspace task: %w", err)
	}
	return nil
}

func (r *Repository) ListTaskRunSummaries(ctx context.Context) (map[string]TaskRunSummary, error) {
	tasks, err := r.ListTasks(ctx)
	if err != nil {
		return nil, err
	}

	summaries := make(map[string]TaskRunSummary)
	for _, t := range tasks {
		runs, err := r.listRunsByTaskName(t.Name)
		if err != nil {
			return nil, err
		}
		summary := TaskRunSummary{TaskName: t.Name, RunCount: len(runs)}
		if len(runs) > 0 {
			copy := runs[0]
			summary.LastRun = &copy
		}
		summaries[t.Name] = summary
	}
	return summaries, nil
}

func (r *Repository) SaveRun(_ context.Context, result task.RunResult) error {
	if result.TaskName == "" {
		return errors.New("task name is required to save run")
	}

	errMsg := ""
	if result.Err != nil {
		errMsg = result.Err.Error()
	}

	rec := RunRecord{
		ID:         runRecordID(result.RunID),
		TaskName:   result.TaskName,
		TaskType:   result.TaskType,
		Trigger:    result.Trigger,
		RunAsUser:  result.RunAsUser,
		RunID:      result.RunID,
		Status:     result.Status,
		Attempt:    result.Attempt,
		StartedAt:  result.StartedAt.UTC(),
		FinishedAt: result.FinishedAt.UTC(),
		DurationMS: result.Duration().Milliseconds(),
		ErrorMsg:   errMsg,
		LogPath:    result.LogPath,
		LogContent: result.Output,
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	replaced := false
	for i := range r.runs {
		if r.runs[i].ID == rec.ID {
			r.runs[i] = rec
			replaced = true
			break
		}
	}
	if !replaced {
		r.runs = append(r.runs, rec)
	}
	return nil
}

func (r *Repository) ListRunsByTask(_ context.Context, taskName string, limit int) ([]RunRecord, error) {
	if limit <= 0 {
		limit = 20
	}

	runs, err := r.listRunsByTaskName(taskName)
	if err != nil {
		return nil, err
	}
	if len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

func (r *Repository) GetRunByID(_ context.Context, id int64) (RunRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, rec := range r.runs {
		if rec.ID == id {
			return rec, nil
		}
	}
	return RunRecord{}, ErrNotFound
}

func (r *Repository) writeTask(t task.Task) (task.Task, error) {
	t = normalizeDaemonTask(t)
	if err := t.Validate(); err != nil {
		return task.Task{}, err
	}

	name, err := safeTaskName(t.Name)
	if err != nil {
		return task.Task{}, err
	}
	t.Name = name

	dir := r.taskDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return task.Task{}, fmt.Errorf("create task workspace: %w", err)
	}

	runPath := filepath.Join(dir, "run.sh")
	script := t.Command.Script
	if script == "" {
		if raw, err := os.ReadFile(runPath); err == nil {
			script = string(raw)
		} else if !os.IsNotExist(err) {
			return task.Task{}, fmt.Errorf("read run.sh: %w", err)
		}
	}
	if script == "" {
		script = defaultRunScript(t)
	}
	script = normalizeScriptLineEndings(script)
	if err := os.WriteFile(runPath, []byte(script), 0o755); err != nil {
		return task.Task{}, fmt.Errorf("write run.sh: %w", err)
	}

	enabled := t.Enabled
	cfg := taskConfig{
		Description:    t.Description,
		Type:           t.Type,
		RunAsUser:      t.RunAsUser,
		TimeoutSeconds: int64(t.Timeout.Seconds()),
		Retry:          t.Retry,
		Enabled:        &enabled,
		Cron:           cronConfig{Spec: t.CronExpr},
		Daemon: daemonConfig{
			Autostart: t.Daemon.Autostart,
			Restart:   t.Daemon.Restart,
		},
		Env: t.Command.Env,
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return task.Task{}, fmt.Errorf("marshal task config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), append(raw, '\n'), 0o644); err != nil {
		return task.Task{}, fmt.Errorf("write task config: %w", err)
	}

	return r.readTask(name)
}

func (r *Repository) readTask(name string) (task.Task, error) {
	name, err := safeTaskName(name)
	if err != nil {
		return task.Task{}, err
	}

	dir := r.taskDir(name)
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if os.IsNotExist(err) {
		return task.Task{}, ErrNotFound
	}
	if err != nil {
		return task.Task{}, fmt.Errorf("read task config %q: %w", name, err)
	}

	var cfg taskConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return task.Task{}, fmt.Errorf("parse task config %q: %w", name, err)
	}

	enabled := true
	if cfg.Enabled != nil {
		enabled = *cfg.Enabled
	}

	info, _ := os.Stat(filepath.Join(dir, "config.json"))
	createdAt := time.Time{}
	if info != nil {
		createdAt = info.ModTime()
	}

	script, err := os.ReadFile(filepath.Join(dir, "run.sh"))
	if os.IsNotExist(err) {
		return task.Task{}, fmt.Errorf("read run.sh for %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return task.Task{}, fmt.Errorf("read run.sh for %q: %w", name, err)
	}

	item := task.Task{
		Name:        name,
		Description: cfg.Description,
		CreatedAt:   createdAt,
		Type:        cfg.Type,
		RunAsUser:   cfg.RunAsUser,
		CronExpr:    cfg.Cron.Spec,
		Timeout:     time.Duration(cfg.TimeoutSeconds) * time.Second,
		Retry:       cfg.Retry,
		Enabled:     enabled,
		Daemon: task.DaemonConfig{
			Autostart: cfg.Daemon.Autostart,
			Restart:   cfg.Daemon.Restart,
		},
		Command: task.CommandSpec{
			Command: "bash",
			Args:    []string{"run.sh"},
			WorkDir: dir,
			Env:     cfg.Env,
			Script:  string(script),
		},
	}
	if item.RunAsUser == "" {
		item.RunAsUser = os.Getenv("USER")
	}
	item = normalizeDaemonTask(item)
	if err := item.Validate(); err != nil {
		return task.Task{}, fmt.Errorf("validate task config %q: %w", name, err)
	}
	return item, nil
}

func (r *Repository) taskDir(name string) string {
	return filepath.Join(r.workspacesDir, name)
}

func (r *Repository) usersPath() string {
	return filepath.Join(r.dataDir, "users.json")
}

func (r *Repository) settingsPath() string {
	return filepath.Join(r.dataDir, "settings.json")
}

func (r *Repository) loadUsersLocked() ([]User, error) {
	var users []User
	if err := readJSONFile(r.usersPath(), &users); err != nil {
		return nil, err
	}
	return users, nil
}

func (r *Repository) loadSettingsLocked() (map[string]string, error) {
	settings := make(map[string]string)
	if err := readJSONFile(r.settingsPath(), &settings); err != nil {
		return nil, err
	}
	return settings, nil
}

func (r *Repository) listRunsByTaskName(taskName string) ([]RunRecord, error) {
	taskName, err := safeTaskName(taskName)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var runs []RunRecord
	for _, rec := range r.runs {
		if rec.TaskName != taskName {
			continue
		}
		if rec.ID == 0 {
			rec.ID = runRecordID(rec.RunID)
		}
		runs = append(runs, rec)
	}

	sort.Slice(runs, func(i, j int) bool {
		if runs[i].StartedAt.Equal(runs[j].StartedAt) {
			return runs[i].ID > runs[j].ID
		}
		return runs[i].StartedAt.After(runs[j].StartedAt)
	})
	return runs, nil
}

func filterSessions(items []Session, keep func(Session) bool) []Session {
	out := items[:0]
	for _, item := range items {
		if keep(item) {
			out = append(out, item)
		}
	}
	return out
}

func readJSONFile(path string, dest any) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read json file %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("parse json file %s: %w", path, err)
	}
	return nil
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create json file dir: %w", err)
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json file %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("write json file %s: %w", path, err)
	}
	return nil
}

func nextUserID(users []User) int64 {
	var maxID int64
	for _, user := range users {
		if user.ID > maxID {
			maxID = user.ID
		}
	}
	return maxID + 1
}

func runRecordID(runID string) int64 {
	id, err := strconv.ParseInt(strings.TrimSpace(runID), 10, 64)
	if err == nil {
		return id
	}
	return time.Now().UTC().UnixNano()
}

func safeTaskName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("task name is required")
	}
	if name != filepath.Base(name) || name == "." || name == ".." {
		return "", errors.New("task name must be a workspace directory name")
	}
	return name, nil
}

func shellLine(command string, args []string) string {
	parts := append([]string{command}, args...)
	for i, part := range parts {
		parts[i] = "'" + strings.ReplaceAll(part, "'", "'\\''") + "'"
	}
	return strings.Join(parts, " ")
}

func defaultRunScript(t task.Task) string {
	if t.Command.Command != "" && t.Command.Command != "bash" {
		return "#!/usr/bin/env bash\nset -euo pipefail\n" + shellLine(t.Command.Command, t.Command.Args) + "\n"
	}
	return "#!/usr/bin/env bash\nset -euo pipefail\n"
}

func normalizeScriptLineEndings(script string) string {
	script = strings.ReplaceAll(script, "\r\n", "\n")
	return strings.ReplaceAll(script, "\r", "\n")
}

func normalizeDaemonTask(t task.Task) task.Task {
	if t.Type == task.TypeDaemon {
		t.Timeout = 0
		t.Daemon = task.DaemonConfig{Autostart: true, Restart: "always"}
	}
	return t
}
