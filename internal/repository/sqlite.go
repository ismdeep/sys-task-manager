package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/ismdeep/sys-task-manager/internal/task"
)

var ErrNotFound = errors.New("record not found")

type User struct {
	ID           int64
	Username     string
	PasswordHash string
}

type Session struct {
	UserID    int64
	Username  string
	Token     string
	ExpiresAt time.Time
}

type RunRecord struct {
	ID         int64
	TaskName   string
	TaskType   task.Type
	Trigger    task.TriggerMode
	RunAsUser  string
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
	Key   string
	Value string
}

type SQLiteRepository struct {
	db *sql.DB
}

func NewSQLiteRepository(path string) (*SQLiteRepository, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	dsn := fmt.Sprintf("%s?_busy_timeout=5000&_foreign_keys=on", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	repo := &SQLiteRepository{db: db}
	if err := repo.db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	return repo, nil
}

func (r *SQLiteRepository) Close() error {
	return r.db.Close()
}

func (r *SQLiteRepository) Init(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			token TEXT NOT NULL UNIQUE,
			expired_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			description TEXT NOT NULL DEFAULT '',
			type TEXT NOT NULL,
			run_as_user TEXT NOT NULL,
			cron_expr TEXT NOT NULL DEFAULT '',
			timeout_seconds INTEGER NOT NULL DEFAULT 0,
			retry INTEGER NOT NULL DEFAULT 0,
			command TEXT NOT NULL,
			args_json TEXT NOT NULL DEFAULT '[]',
			work_dir TEXT NOT NULL DEFAULT '',
			env_json TEXT NOT NULL DEFAULT '[]',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS task_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id INTEGER NOT NULL,
			trigger_mode TEXT NOT NULL,
			run_as_user TEXT NOT NULL,
			status TEXT NOT NULL,
			attempt INTEGER NOT NULL,
			started_at DATETIME NOT NULL,
			finished_at DATETIME NOT NULL,
			duration_ms INTEGER NOT NULL,
			error_message TEXT NOT NULL DEFAULT '',
			log_path TEXT NOT NULL DEFAULT '',
			log_content TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_task_runs_task_id_started_at ON task_runs(task_id, started_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_token ON sessions(token)`,
	}

	for _, stmt := range stmts {
		if _, err := r.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("init sqlite schema: %w", err)
		}
	}

	if err := r.ensureTaskRunColumn(ctx, "log_content", `ALTER TABLE task_runs ADD COLUMN log_content TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := r.ensureTaskColumn(ctx, "description", `ALTER TABLE tasks ADD COLUMN description TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}

	return nil
}

func (r *SQLiteRepository) GetSetting(ctx context.Context, key string) (Setting, error) {
	var item Setting
	err := r.db.QueryRowContext(
		ctx,
		`SELECT key, value FROM settings WHERE key = ?`,
		key,
	).Scan(&item.Key, &item.Value)
	if errors.Is(err, sql.ErrNoRows) {
		return Setting{}, ErrNotFound
	}
	if err != nil {
		return Setting{}, fmt.Errorf("query setting: %w", err)
	}
	return item, nil
}

func (r *SQLiteRepository) SetSetting(ctx context.Context, key, value string) error {
	_, err := r.db.ExecContext(
		ctx,
		`INSERT INTO settings (key, value)
		 VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		key,
		value,
	)
	if err != nil {
		return fmt.Errorf("upsert setting: %w", err)
	}
	return nil
}

func (r *SQLiteRepository) EnsureAdmin(ctx context.Context, username, passwordHash string) error {
	var existing int64
	err := r.db.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, username).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check admin user: %w", err)
	}

	_, err = r.db.ExecContext(
		ctx,
		`INSERT INTO users (username, password_hash) VALUES (?, ?)`,
		username,
		passwordHash,
	)
	if err != nil {
		return fmt.Errorf("insert admin user: %w", err)
	}
	return nil
}

func (r *SQLiteRepository) GetUserByUsername(ctx context.Context, username string) (User, error) {
	var u User
	err := r.db.QueryRowContext(
		ctx,
		`SELECT id, username, password_hash FROM users WHERE username = ?`,
		username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("query user: %w", err)
	}
	return u, nil
}

func (r *SQLiteRepository) UpdateUserPasswordHash(ctx context.Context, userID int64, passwordHash string) error {
	result, err := r.db.ExecContext(
		ctx,
		`UPDATE users
		 SET password_hash = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`,
		passwordHash,
		userID,
	)
	if err != nil {
		return fmt.Errorf("update user password: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read affected rows: %w", err)
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *SQLiteRepository) CreateSession(ctx context.Context, userID int64, token string, expiresAt time.Time) error {
	_, err := r.db.ExecContext(
		ctx,
		`INSERT INTO sessions (user_id, token, expired_at) VALUES (?, ?, ?)`,
		userID,
		token,
		expiresAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (r *SQLiteRepository) GetSession(ctx context.Context, token string) (Session, error) {
	var s Session
	err := r.db.QueryRowContext(
		ctx,
		`SELECT s.user_id, u.username, s.token, s.expired_at
		 FROM sessions s
		 JOIN users u ON u.id = s.user_id
		 WHERE s.token = ?`,
		token,
	).Scan(&s.UserID, &s.Username, &s.Token, &s.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("query session: %w", err)
	}
	return s, nil
}

func (r *SQLiteRepository) DeleteSession(ctx context.Context, token string) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (r *SQLiteRepository) DeleteSessionsByUserID(ctx context.Context, userID int64) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("delete user sessions: %w", err)
	}
	return nil
}

func (r *SQLiteRepository) CleanupExpiredSessions(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE expired_at <= ?`, time.Now().UTC()); err != nil {
		return fmt.Errorf("cleanup expired sessions: %w", err)
	}
	return nil
}

func (r *SQLiteRepository) EnsureTask(ctx context.Context, t task.Task) (task.Task, error) {
	existing, err := r.GetTaskByName(ctx, t.Name)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return task.Task{}, err
	}
	return r.CreateTask(ctx, t)
}

func (r *SQLiteRepository) CreateTask(ctx context.Context, t task.Task) (task.Task, error) {
	argsJSON, err := json.Marshal(t.Command.Args)
	if err != nil {
		return task.Task{}, fmt.Errorf("marshal task args: %w", err)
	}
	envJSON, err := json.Marshal(t.Command.Env)
	if err != nil {
		return task.Task{}, fmt.Errorf("marshal task env: %w", err)
	}

	res, err := r.db.ExecContext(
		ctx,
		`INSERT INTO tasks
		(name, description, type, run_as_user, cron_expr, timeout_seconds, retry, command, args_json, work_dir, env_json, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Name,
		t.Description,
		string(t.Type),
		t.RunAsUser,
		t.CronExpr,
		int64(t.Timeout.Seconds()),
		t.Retry,
		t.Command.Command,
		string(argsJSON),
		t.Command.WorkDir,
		string(envJSON),
		boolToInt(t.Enabled),
	)
	if err != nil {
		return task.Task{}, fmt.Errorf("create task: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return task.Task{}, fmt.Errorf("fetch task id: %w", err)
	}
	t.ID = id
	return r.GetTaskByName(ctx, t.Name)
}

func (r *SQLiteRepository) UpdateTaskByName(ctx context.Context, name string, t task.Task) (task.Task, error) {
	argsJSON, err := json.Marshal(t.Command.Args)
	if err != nil {
		return task.Task{}, fmt.Errorf("marshal task args: %w", err)
	}
	envJSON, err := json.Marshal(t.Command.Env)
	if err != nil {
		return task.Task{}, fmt.Errorf("marshal task env: %w", err)
	}

	res, err := r.db.ExecContext(
		ctx,
		`UPDATE tasks
		 SET description = ?, type = ?, run_as_user = ?, cron_expr = ?, timeout_seconds = ?, retry = ?, command = ?, args_json = ?, work_dir = ?, env_json = ?, enabled = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE name = ?`,
		t.Description,
		string(t.Type),
		t.RunAsUser,
		t.CronExpr,
		int64(t.Timeout.Seconds()),
		t.Retry,
		t.Command.Command,
		string(argsJSON),
		t.Command.WorkDir,
		string(envJSON),
		boolToInt(t.Enabled),
		name,
	)
	if err != nil {
		return task.Task{}, fmt.Errorf("update task: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return task.Task{}, fmt.Errorf("fetch affected rows: %w", err)
	}
	if affected == 0 {
		return task.Task{}, ErrNotFound
	}

	return r.GetTaskByName(ctx, name)
}

func (r *SQLiteRepository) ListTasks(ctx context.Context) ([]task.Task, error) {
	rows, err := r.db.QueryContext(
		ctx,
		`SELECT id, name, description, created_at, type, run_as_user, cron_expr, timeout_seconds, retry, command, args_json, work_dir, env_json, enabled
		 FROM tasks
		 ORDER BY name ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []task.Task
	for rows.Next() {
		item, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, item)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	return tasks, nil
}

func (r *SQLiteRepository) GetTaskByName(ctx context.Context, name string) (task.Task, error) {
	row := r.db.QueryRowContext(
		ctx,
		`SELECT id, name, description, created_at, type, run_as_user, cron_expr, timeout_seconds, retry, command, args_json, work_dir, env_json, enabled
		 FROM tasks
		 WHERE name = ?`,
		name,
	)

	item, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, ErrNotFound
	}
	if err != nil {
		return task.Task{}, err
	}
	return item, nil
}

func (r *SQLiteRepository) DeleteTaskByName(ctx context.Context, name string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM tasks WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete task: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("fetch affected rows: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}

	return nil
}

func (r *SQLiteRepository) ListTaskRunSummaries(ctx context.Context) (map[string]TaskRunSummary, error) {
	rows, err := r.db.QueryContext(
		ctx,
		`SELECT
			t.name,
			COUNT(tr.id) AS run_count,
			lr.id,
			lr.trigger_mode,
			lr.run_as_user,
			lr.status,
			lr.attempt,
			lr.started_at,
			lr.finished_at,
			lr.duration_ms,
			lr.error_message,
			lr.log_path,
			lr.log_content
		FROM tasks t
		LEFT JOIN task_runs tr ON tr.task_id = t.id
		LEFT JOIN task_runs lr ON lr.id = (
			SELECT tr2.id
			FROM task_runs tr2
			WHERE tr2.task_id = t.id
			ORDER BY tr2.started_at DESC, tr2.id DESC
			LIMIT 1
		)
		GROUP BY
			t.id,
			t.name,
			lr.id,
			lr.trigger_mode,
			lr.run_as_user,
			lr.status,
			lr.attempt,
			lr.started_at,
			lr.finished_at,
			lr.duration_ms,
			lr.error_message,
			lr.log_path,
			lr.log_content
		ORDER BY t.name ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list task run summaries: %w", err)
	}
	defer rows.Close()

	summaries := make(map[string]TaskRunSummary)
	for rows.Next() {
		var (
			summary          TaskRunSummary
			lastID           sql.NullInt64
			lastTrigger      sql.NullString
			lastRunAs        sql.NullString
			lastStatus       sql.NullString
			lastAttempt      sql.NullInt64
			lastStartedAt    sql.NullTime
			lastFinishedAt   sql.NullTime
			lastDurationMS   sql.NullInt64
			lastErrorMessage sql.NullString
			lastLogPath      sql.NullString
			lastLogContent   sql.NullString
		)

		if err := rows.Scan(
			&summary.TaskName,
			&summary.RunCount,
			&lastID,
			&lastTrigger,
			&lastRunAs,
			&lastStatus,
			&lastAttempt,
			&lastStartedAt,
			&lastFinishedAt,
			&lastDurationMS,
			&lastErrorMessage,
			&lastLogPath,
			&lastLogContent,
		); err != nil {
			return nil, fmt.Errorf("scan task run summary: %w", err)
		}

		if lastID.Valid {
			summary.LastRun = &RunRecord{
				ID:         lastID.Int64,
				TaskName:   summary.TaskName,
				Trigger:    task.TriggerMode(lastTrigger.String),
				RunAsUser:  lastRunAs.String,
				Status:     task.Status(lastStatus.String),
				Attempt:    int(lastAttempt.Int64),
				StartedAt:  lastStartedAt.Time,
				FinishedAt: lastFinishedAt.Time,
				DurationMS: lastDurationMS.Int64,
				ErrorMsg:   lastErrorMessage.String,
				LogPath:    lastLogPath.String,
				LogContent: lastLogContent.String,
			}
		}

		summaries[summary.TaskName] = summary
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task run summaries: %w", err)
	}

	return summaries, nil
}

func (r *SQLiteRepository) SaveRun(ctx context.Context, result task.RunResult) error {
	if result.TaskName == "" {
		return errors.New("task name is required to save run")
	}

	var taskID int64
	if err := r.db.QueryRowContext(ctx, `SELECT id FROM tasks WHERE name = ?`, result.TaskName).Scan(&taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("lookup task id: %w", err)
	}

	errMsg := ""
	if result.Err != nil {
		errMsg = result.Err.Error()
	}

	_, err := r.db.ExecContext(
		ctx,
		`INSERT INTO task_runs
		(task_id, trigger_mode, run_as_user, status, attempt, started_at, finished_at, duration_ms, error_message, log_path, log_content)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		taskID,
		string(result.Trigger),
		result.RunAsUser,
		string(result.Status),
		result.Attempt,
		result.StartedAt.UTC(),
		result.FinishedAt.UTC(),
		result.Duration().Milliseconds(),
		errMsg,
		result.LogPath,
		result.Output,
	)
	if err != nil {
		return fmt.Errorf("insert task run: %w", err)
	}
	return nil
}

func (r *SQLiteRepository) ListRunsByTask(ctx context.Context, taskName string, limit int) ([]RunRecord, error) {
	if limit <= 0 {
		limit = 20
	}

	rows, err := r.db.QueryContext(
		ctx,
		`SELECT tr.id, t.name, t.type, tr.trigger_mode, tr.run_as_user, tr.status, tr.attempt, tr.started_at, tr.finished_at, tr.duration_ms, tr.error_message, tr.log_path, tr.log_content
		 FROM task_runs tr
		 JOIN tasks t ON t.id = tr.task_id
		 WHERE t.name = ?
		 ORDER BY tr.started_at DESC
		 LIMIT ?`,
		taskName,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var runs []RunRecord
	for rows.Next() {
		var rec RunRecord
		if err := rows.Scan(
			&rec.ID,
			&rec.TaskName,
			&rec.TaskType,
			&rec.Trigger,
			&rec.RunAsUser,
			&rec.Status,
			&rec.Attempt,
			&rec.StartedAt,
			&rec.FinishedAt,
			&rec.DurationMS,
			&rec.ErrorMsg,
			&rec.LogPath,
			&rec.LogContent,
		); err != nil {
			return nil, fmt.Errorf("scan run record: %w", err)
		}
		runs = append(runs, rec)
	}
	return runs, rows.Err()
}

func (r *SQLiteRepository) GetRunByID(ctx context.Context, id int64) (RunRecord, error) {
	var rec RunRecord
	err := r.db.QueryRowContext(
		ctx,
		`SELECT tr.id, t.name, t.type, tr.trigger_mode, tr.run_as_user, tr.status, tr.attempt, tr.started_at, tr.finished_at, tr.duration_ms, tr.error_message, tr.log_path, tr.log_content
		 FROM task_runs tr
		 JOIN tasks t ON t.id = tr.task_id
		 WHERE tr.id = ?`,
		id,
	).Scan(
		&rec.ID,
		&rec.TaskName,
		&rec.TaskType,
		&rec.Trigger,
		&rec.RunAsUser,
		&rec.Status,
		&rec.Attempt,
		&rec.StartedAt,
		&rec.FinishedAt,
		&rec.DurationMS,
		&rec.ErrorMsg,
		&rec.LogPath,
		&rec.LogContent,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return RunRecord{}, ErrNotFound
	}
	if err != nil {
		return RunRecord{}, fmt.Errorf("query run by id: %w", err)
	}
	return rec, nil
}

func (r *SQLiteRepository) ensureTaskRunColumn(ctx context.Context, name, ddl string) error {
	return r.ensureTableColumn(ctx, "task_runs", name, ddl)
}

func (r *SQLiteRepository) ensureTaskColumn(ctx context.Context, name, ddl string) error {
	return r.ensureTableColumn(ctx, "tasks", name, ddl)
}

func (r *SQLiteRepository) ensureTableColumn(ctx context.Context, tableName, name, ddl string) error {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", tableName))
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", tableName, err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var columnName string
		var columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return fmt.Errorf("scan %s schema: %w", tableName, err)
		}
		if columnName == name {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate %s schema: %w", tableName, err)
	}

	if _, err := r.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("alter %s add %s: %w", tableName, name, err)
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTask(row scanner) (task.Task, error) {
	var item task.Task
	var rawType string
	var argsJSON string
	var envJSON string
	var enabled int
	var timeoutSeconds int64

	err := row.Scan(
		&item.ID,
		&item.Name,
		&item.Description,
		&item.CreatedAt,
		&rawType,
		&item.RunAsUser,
		&item.CronExpr,
		&timeoutSeconds,
		&item.Retry,
		&item.Command.Command,
		&argsJSON,
		&item.Command.WorkDir,
		&envJSON,
		&enabled,
	)
	if err != nil {
		return task.Task{}, err
	}

	item.Type = task.Type(rawType)
	item.Timeout = time.Duration(timeoutSeconds) * time.Second
	item.Enabled = enabled == 1

	if err := json.Unmarshal([]byte(argsJSON), &item.Command.Args); err != nil {
		return task.Task{}, fmt.Errorf("unmarshal task args: %w", err)
	}
	if err := json.Unmarshal([]byte(envJSON), &item.Command.Env); err != nil {
		return task.Task{}, fmt.Errorf("unmarshal task env: %w", err)
	}

	return item, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
