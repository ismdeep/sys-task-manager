package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ismdeep/log"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/ismdeep/sys-task-manager/internal/manager"
	"github.com/ismdeep/sys-task-manager/internal/repository"
	"github.com/ismdeep/sys-task-manager/internal/task"
)

const sessionCookieName = "task_manager_session"
const flashCookieName = "task_manager_flash"

type Server struct {
	repo      *repository.Repository
	manager   *manager.Manager
	logger    *log.Logger
	templates *template.Template
	i18n      translations
}

type loginPageData struct {
	pageMeta
	Error string
}

type noticePageData struct {
	pageMeta
	Title       string
	Message     string
	RedirectURL string
	RedirectIn  int
}

type dashboardPageData struct {
	pageMeta
	Username string
	Stats    dashboardStats
	Tasks    []dashboardTaskView
}

type dashboardStats struct {
	Total    int
	Enabled  int
	Disabled int
	Running  int
	Failed   int
	Manual   int
	Cron     int
	Daemon   int
	Runs     int
}

type dashboardTaskView struct {
	Task          task.Task
	CurrentStatus string
	RunCount      int
	LastRun       *repository.RunRecord
}

type tasksPageData struct {
	pageMeta
	Username         string
	Message          string
	Error            string
	Tasks            []taskListItemView
	SelectedTask     *taskDetailView
	SelectedTaskName string
}

type taskFormPageData struct {
	pageMeta
	Username  string
	Message   string
	Error     string
	Mode      string
	Task      task.Task
	ArgsText  string
	EnvText   string
	RunScript string
}

type runsPageData struct {
	pageMeta
	Username string
	TaskName string
	Runs     []repository.RunRecord
}

type taskView struct {
	Task   task.Task
	Status task.RunResult
}

type taskListItemView struct {
	Task       task.Task
	Status     task.RunResult
	IsSelected bool
}

type taskDetailView struct {
	Task            task.Task
	Status          task.RunResult
	DescriptionText string
	CurrentOutput   string
	TimeoutText     string
	CurrentStatus   string
}

type runTaskAPIRequest struct {
	OverrideAs string `json:"override_as"`
	Async      bool   `json:"async"`
}

type taskEnabledAPIRequest struct {
	Enabled bool `json:"enabled"`
}

type runTaskAPIResponse struct {
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

type taskEnabledAPIResponse struct {
	Message       string `json:"message,omitempty"`
	Error         string `json:"error,omitempty"`
	Enabled       bool   `json:"enabled"`
	CurrentStatus string `json:"current_status,omitempty"`
}

func NewServer(repo *repository.Repository, mgr *manager.Manager) (*Server, error) {
	i18n, err := loadTranslations()
	if err != nil {
		return nil, fmt.Errorf("load translations: %w", err)
	}

	var srv *Server
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"formatTime": func(v time.Time) string {
			if v.IsZero() {
				return "-"
			}
			return v.Local().Format("2006-01-02 15:04:05")
		},
		"formatDurationMS": func(ms int64) string {
			if ms <= 0 {
				return "-"
			}
			return (time.Duration(ms) * time.Millisecond).String()
		},
		"statusClass": func(status string) string {
			switch strings.ToLower(status) {
			case string(task.StatusSuccess):
				return "success"
			case string(task.StatusFailed):
				return "failed"
			case string(task.StatusRunning):
				return "running"
			case "disabled":
				return "disabled"
			default:
				return "pending"
			}
		},
		"joinLines": func(items []string) string {
			if len(items) == 0 {
				return "-"
			}
			return strings.Join(items, "\n")
		},
		"initial": func(value string) string {
			value = strings.TrimSpace(value)
			if value == "" {
				return "U"
			}
			return strings.ToUpper(string([]rune(value)[0]))
		},
		"t": func(locale, key string) string {
			if srv == nil {
				return key
			}
			return srv.translate(locale, key)
		},
		"tf": func(locale, key string, args ...any) string {
			if srv == nil {
				return key
			}
			return srv.translatef(locale, key, args...)
		},
		"withLang":  withLang,
		"withParam": withParam,
		"htmlLang":  htmlLang,
		"statusLabel": func(locale, status string) string {
			if srv == nil {
				return status
			}
			return srv.statusLabel(locale, status)
		},
	}).ParseFS(webFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse web templates: %w", err)
	}

	srv = &Server{
		repo:      repo,
		manager:   mgr,
		templates: tmpl,
		i18n:      i18n,
	}
	return srv, nil
}

func (s *Server) Routes() http.Handler {
	gin.SetMode(gin.ReleaseMode)

	router := gin.New()
	router.Use(gin.Recovery())
	router.GET("/login", s.ginHandler(s.handleLoginPage))
	router.POST("/login", s.ginHandler(s.handleLogin))
	router.POST("/logout", s.ginHandler(s.requireAuth(s.handleLogout)))
	router.GET("/", s.ginHandler(s.requireAuth(s.handleDashboardPage)))
	router.GET("/tasks", s.ginHandler(s.requireAuth(s.handleTasksPage)))
	router.GET("/tasks/new", s.ginHandler(s.requireAuth(s.handleTaskCreatePage)))
	router.GET("/tasks/:name/edit", s.ginHandler(s.requireAuth(s.handleTaskEditPage)))
	router.POST("/tasks", s.ginHandler(s.requireAuth(s.handleCreateTask)))
	router.POST("/tasks/:name/edit", s.ginHandler(s.requireAuth(s.handleUpdateTask)))
	router.POST("/tasks/:name/delete", s.ginHandler(s.requireAuth(s.handleDeleteTask)))
	router.POST("/tasks/:name/run", s.ginHandler(s.requireAuth(s.handleRunTask)))
	router.POST("/api/tasks/:name/run", s.ginHandler(s.requireAuth(s.handleRunTaskAPI)))
	router.GET("/api/tasks/:name/output/stream", s.ginHandler(s.requireAuth(s.handleTaskOutputStream)))
	router.PATCH("/api/tasks/:name/enabled", s.ginHandler(s.requireAuth(s.handleTaskEnabledAPI)))
	router.POST("/settings/password", s.ginHandler(s.requireAuth(s.handleUpdatePassword)))
	router.GET("/tasks/:name/runs", s.ginHandler(s.requireAuth(s.handleRunsPage)))
	router.GET("/runs/:id/log", s.ginHandler(s.requireAuth(s.handleRunLog)))
	return router
}

func (s *Server) ginHandler(next http.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, param := range c.Params {
			c.Request.SetPathValue(param.Key, param.Value)
		}
		next(c.Writer, c.Request)
	}
}

func PasswordHash(password string) (string, error) {
	raw, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("generate password hash: %w", err)
	}
	return string(raw), nil
}

func CheckPassword(hash, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, err := s.currentSession(r.Context(), r); err == nil {
		http.Redirect(w, r, "/tasks", http.StatusSeeOther)
		return
	}
	_, errorMessage := popFlash(w, r)
	s.render(w, r, "login.html", loginPageData{
		pageMeta: s.newPageMeta(w, r),
		Error:    errorMessage,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		setFlash(w, "error", s.translateForRequest(r, "flash.invalid_form"))
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	user, err := s.repo.GetUserByUsername(r.Context(), strings.TrimSpace(r.FormValue("username")))
	if err != nil || CheckPassword(user.PasswordHash, r.FormValue("password")) != nil {
		setFlash(w, "error", s.translateForRequest(r, "flash.invalid_credentials"))
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	token, err := randomToken()
	if err != nil {
		http.Error(w, "cannot create session", http.StatusInternalServerError)
		return
	}

	expiresAt := time.Now().Add(24 * time.Hour)
	if err := s.repo.CreateSession(r.Context(), user.ID, token, expiresAt); err != nil {
		http.Error(w, "cannot persist session", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  expiresAt,
	})

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err == nil && cookie.Value != "" {
		_ = s.repo.DeleteSession(r.Context(), cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Path:     "/",
		Value:    "",
		MaxAge:   -1,
		HttpOnly: true,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleDashboardPage(w http.ResponseWriter, r *http.Request) {
	session, err := s.currentSession(r.Context(), r)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	tasks, err := s.repo.ListTasks(r.Context())
	if err != nil {
		http.Error(w, "cannot list tasks", http.StatusInternalServerError)
		return
	}
	summaries, err := s.repo.ListTaskRunSummaries(r.Context())
	if err != nil {
		http.Error(w, "cannot list task run summaries", http.StatusInternalServerError)
		return
	}

	stats := dashboardStats{Total: len(tasks)}
	views := make([]dashboardTaskView, 0, len(tasks))
	for _, item := range tasks {
		status, _ := s.manager.Status(item.Name)
		currentStatus := displayStatus(status.Status, item.Enabled)
		summary := summaries[item.Name]

		if item.Enabled {
			stats.Enabled++
		} else {
			stats.Disabled++
		}
		switch item.Type {
		case task.TypeManual:
			stats.Manual++
		case task.TypeCron:
			stats.Cron++
		case task.TypeDaemon:
			stats.Daemon++
		}
		switch currentStatus {
		case string(task.StatusRunning):
			stats.Running++
		case string(task.StatusFailed):
			stats.Failed++
		}
		stats.Runs += summary.RunCount

		views = append(views, dashboardTaskView{
			Task:          item,
			CurrentStatus: currentStatus,
			RunCount:      summary.RunCount,
			LastRun:       summary.LastRun,
		})
	}

	s.render(w, r, "dashboard.html", dashboardPageData{
		pageMeta: s.newPageMeta(w, r),
		Username: session.Username,
		Stats:    stats,
		Tasks:    views,
	})
}

func (s *Server) handleTasksPage(w http.ResponseWriter, r *http.Request) {
	session, err := s.currentSession(r.Context(), r)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	message, errorMessage := popFlash(w, r)

	tasks, err := s.repo.ListTasks(r.Context())
	if err != nil {
		http.Error(w, "cannot list tasks", http.StatusInternalServerError)
		return
	}
	selectedName := strings.TrimSpace(r.URL.Query().Get("task"))
	if selectedName == "" && len(tasks) > 0 {
		selectedName = tasks[0].Name
	}

	views := make([]taskListItemView, 0, len(tasks))
	for _, item := range tasks {
		status, _ := s.manager.Status(item.Name)
		views = append(views, taskListItemView{
			Task:       item,
			Status:     status,
			IsSelected: item.Name == selectedName,
		})
	}

	var selected *taskDetailView
	if selectedName != "" {
		for _, item := range views {
			if item.Task.Name != selectedName {
				continue
			}

			selected = &taskDetailView{
				Task:            item.Task,
				Status:          item.Status,
				DescriptionText: strings.TrimSpace(item.Task.Description),
				CurrentOutput:   currentOutput(item.Status),
				TimeoutText:     formatTaskTimeout(s.resolveLocale(r), item.Task.Timeout, s.translate),
				CurrentStatus:   displayStatus(item.Status.Status, item.Task.Enabled),
			}
			break
		}
	}

	data := tasksPageData{
		pageMeta:         s.newPageMeta(w, r),
		Username:         session.Username,
		Message:          message,
		Error:            errorMessage,
		Tasks:            views,
		SelectedTask:     selected,
		SelectedTaskName: selectedName,
	}
	s.render(w, r, "tasks.html", data)
}

func (s *Server) handleTaskCreatePage(w http.ResponseWriter, r *http.Request) {
	session, err := s.currentSession(r.Context(), r)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	message, errorMessage := popFlash(w, r)

	s.render(w, r, "task_form.html", taskFormPageData{
		pageMeta: s.newPageMeta(w, r),
		Username: session.Username,
		Message:  message,
		Error:    errorMessage,
		Mode:     "create",
		Task: task.Task{
			Type:      task.TypeManual,
			Enabled:   true,
			RunAsUser: os.Getenv("USER"),
			Timeout:   30 * time.Second,
			Command: task.CommandSpec{
				Command: "bash",
				Args:    []string{"run.sh"},
				Script:  "#!/usr/bin/env bash\nset -euo pipefail\n",
			},
		},
		RunScript: "#!/usr/bin/env bash\nset -euo pipefail\n",
	})
}

func (s *Server) handleTaskEditPage(w http.ResponseWriter, r *http.Request) {
	session, err := s.currentSession(r.Context(), r)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	message, errorMessage := popFlash(w, r)

	item, err := s.repo.GetTaskByName(r.Context(), r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	s.render(w, r, "task_form.html", taskFormPageData{
		pageMeta:  s.newPageMeta(w, r),
		Username:  session.Username,
		Message:   message,
		Error:     errorMessage,
		Mode:      "edit",
		Task:      item,
		ArgsText:  strings.Join(item.Command.Args, "\n"),
		EnvText:   strings.Join(item.Command.Env, "\n"),
		RunScript: item.Command.Script,
	})
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		setFlash(w, "error", s.translateForRequest(r, "flash.invalid_form"))
		http.Redirect(w, r, "/tasks", http.StatusSeeOther)
		return
	}

	newTask := buildTaskFromForm(r, true)

	if err := newTask.Validate(); err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, "/tasks", http.StatusSeeOther)
		return
	}

	created, err := s.repo.CreateTask(r.Context(), newTask)
	if err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, "/tasks", http.StatusSeeOther)
		return
	}
	if err := s.manager.Register(created); err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, "/tasks", http.StatusSeeOther)
		return
	}

	setFlash(w, "message", s.translateForRequest(r, "flash.task_created"))
	http.Redirect(w, r, "/tasks", http.StatusSeeOther)
}

func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		setFlash(w, "error", s.translateForRequest(r, "flash.invalid_form"))
		http.Redirect(w, r, "/tasks/"+url.PathEscape(r.PathValue("name"))+"/edit", http.StatusSeeOther)
		return
	}

	existing, err := s.repo.GetTaskByName(r.Context(), r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	updated := buildTaskFromForm(r, existing.Enabled)
	updated.Name = existing.Name

	if err := updated.Validate(); err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, "/tasks/"+url.PathEscape(updated.Name)+"/edit", http.StatusSeeOther)
		return
	}

	item, err := s.repo.UpdateTaskByName(r.Context(), updated.Name, updated)
	if err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, "/tasks/"+url.PathEscape(updated.Name)+"/edit", http.StatusSeeOther)
		return
	}
	if err := s.manager.Replace(item); err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, "/tasks/"+url.PathEscape(updated.Name)+"/edit", http.StatusSeeOther)
		return
	}

	setFlash(w, "message", s.translateForRequest(r, "flash.task_updated"))
	http.Redirect(w, r, "/tasks/"+url.PathEscape(updated.Name)+"/edit", http.StatusSeeOther)
}

func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		setFlash(w, "error", s.translateForRequest(r, "flash.invalid_delete_request"))
		http.Redirect(w, r, sanitizedRedirectTarget(r), http.StatusSeeOther)
		return
	}

	name := r.PathValue("name")
	removed, err := s.manager.Remove(name)
	if err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, sanitizedRedirectTarget(r), http.StatusSeeOther)
		return
	}

	if err := s.repo.DeleteTaskByName(r.Context(), name); err != nil {
		if restoreErr := s.manager.Register(removed); restoreErr != nil {
			s.logger.WithContext(r.Context()).Error(
				"restore task after delete failure failed",
				zap.String("task", name),
				zap.Error(restoreErr),
			)
		}
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, sanitizedRedirectTarget(r), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/tasks", http.StatusSeeOther)
}

func (s *Server) handleRunTask(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		setFlash(w, "error", s.translateForRequest(r, "flash.invalid_run_request"))
		http.Redirect(w, r, sanitizedRedirectTarget(r), http.StatusSeeOther)
		return
	}

	_, err := s.manager.RunManual(
		r.Context(),
		r.PathValue("name"),
		strings.TrimSpace(r.FormValue("override_as")),
		r.FormValue("async") == "1",
	)
	if err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, sanitizedRedirectTarget(r), http.StatusSeeOther)
		return
	}

	setFlash(w, "message", s.translateForRequest(r, "flash.task_triggered"))
	http.Redirect(w, r, sanitizedRedirectTarget(r), http.StatusSeeOther)
}

func (s *Server) handleRunTaskAPI(w http.ResponseWriter, r *http.Request) {
	var req runTaskAPIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, runTaskAPIResponse{Error: s.translateForRequest(r, "flash.invalid_json_body")})
		return
	}

	_, err := s.manager.RunManual(
		r.Context(),
		r.PathValue("name"),
		strings.TrimSpace(req.OverrideAs),
		req.Async,
	)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, runTaskAPIResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, runTaskAPIResponse{Message: s.translateForRequest(r, "flash.task_triggered")})
}

func (s *Server) handleTaskEnabledAPI(w http.ResponseWriter, r *http.Request) {
	var req taskEnabledAPIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, taskEnabledAPIResponse{Error: s.translateForRequest(r, "flash.invalid_json_body")})
		return
	}

	item, err := s.repo.GetTaskByName(r.Context(), r.PathValue("name"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, taskEnabledAPIResponse{Error: err.Error()})
		return
	}

	item.Enabled = req.Enabled

	updated, err := s.repo.UpdateTaskByName(r.Context(), item.Name, item)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, taskEnabledAPIResponse{Error: err.Error()})
		return
	}
	if err := s.manager.Replace(updated); err != nil {
		writeJSON(w, http.StatusBadRequest, taskEnabledAPIResponse{Error: err.Error()})
		return
	}

	messageKey := "flash.task_disabled"
	if updated.Enabled {
		messageKey = "flash.task_enabled"
	}

	status, _ := s.manager.Status(updated.Name)
	writeJSON(w, http.StatusOK, taskEnabledAPIResponse{
		Message:       s.translateForRequest(r, messageKey),
		Enabled:       updated.Enabled,
		CurrentStatus: displayStatus(status.Status, updated.Enabled),
	})
}

func (s *Server) handleTaskOutputStream(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.manager.Status(r.PathValue("name")); !ok {
		http.NotFound(w, r)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	lastPayload := ""
	for {
		status, ok := s.manager.Status(r.PathValue("name"))
		payload, err := json.Marshal(struct {
			Running bool   `json:"running"`
			Output  string `json:"output"`
		}{
			Running: ok && status.Status == task.StatusRunning,
			Output:  currentOutput(status),
		})
		if err != nil {
			return
		}

		if string(payload) != lastPayload {
			if _, err := fmt.Fprintf(w, "event: output\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
			lastPayload = string(payload)
		}

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) handleRunsPage(w http.ResponseWriter, r *http.Request) {
	session, err := s.currentSession(r.Context(), r)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	runs, err := s.repo.ListRunsByTask(r.Context(), r.PathValue("name"), 50)
	if err != nil {
		http.Error(w, "cannot list runs", http.StatusInternalServerError)
		return
	}

	s.render(w, r, "runs.html", runsPageData{
		pageMeta: s.newPageMeta(w, r),
		Username: session.Username,
		TaskName: r.PathValue("name"),
		Runs:     runs,
	})
}

func (s *Server) handleUpdatePassword(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		setFlash(w, "error", s.translateForRequest(r, "flash.invalid_form"))
		http.Redirect(w, r, sanitizedRedirectTarget(r), http.StatusSeeOther)
		return
	}

	session, err := s.currentSession(r.Context(), r)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	user, err := s.repo.GetUserByUsername(r.Context(), session.Username)
	if err != nil {
		setFlash(w, "error", s.translateForRequest(r, "settings.password_user_not_found"))
		http.Redirect(w, r, sanitizedRedirectTarget(r), http.StatusSeeOther)
		return
	}

	currentPassword := r.FormValue("current_password")
	newPassword := r.FormValue("new_password")
	confirmPassword := r.FormValue("confirm_password")

	switch {
	case CheckPassword(user.PasswordHash, currentPassword) != nil:
		setFlash(w, "error", s.translateForRequest(r, "settings.password_current_invalid"))
		http.Redirect(w, r, sanitizedRedirectTarget(r), http.StatusSeeOther)
		return
	case len(newPassword) < 6:
		setFlash(w, "error", s.translateForRequest(r, "settings.password_too_short"))
		http.Redirect(w, r, sanitizedRedirectTarget(r), http.StatusSeeOther)
		return
	case newPassword != confirmPassword:
		setFlash(w, "error", s.translateForRequest(r, "settings.password_confirm_mismatch"))
		http.Redirect(w, r, sanitizedRedirectTarget(r), http.StatusSeeOther)
		return
	}

	passwordHash, err := PasswordHash(newPassword)
	if err != nil {
		http.Error(w, "cannot hash password", http.StatusInternalServerError)
		return
	}

	if err := s.repo.UpdateUserPasswordHash(r.Context(), user.ID, passwordHash); err != nil {
		http.Error(w, "cannot update password", http.StatusInternalServerError)
		return
	}

	if err := s.repo.DeleteSessionsByUserID(r.Context(), user.ID); err != nil {
		http.Error(w, "cannot clear sessions", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Path:     "/",
		Value:    "",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	s.render(w, r, "notice.html", noticePageData{
		pageMeta:    s.newPageMeta(w, r),
		Title:       s.translateForRequest(r, "settings.password_updated_title"),
		Message:     s.translateForRequest(r, "settings.password_updated"),
		RedirectURL: "/login",
		RedirectIn:  3,
	})
}

func (s *Server) handleRunLog(w http.ResponseWriter, r *http.Request) {
	runID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	rec, err := s.repo.GetRunByID(r.Context(), runID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "cannot load run", http.StatusInternalServerError)
		return
	}

	if rec.LogContent == "" {
		if rec.LogPath == "" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, rec.LogPath)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(rec.LogContent))
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.currentSession(r.Context(), r); err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (s *Server) currentSession(ctx context.Context, r *http.Request) (repository.Session, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return repository.Session{}, repository.ErrNotFound
	}

	session, err := s.repo.GetSession(ctx, cookie.Value)
	if err != nil {
		return repository.Session{}, err
	}
	if session.ExpiresAt.Before(time.Now()) {
		_ = s.repo.DeleteSession(ctx, cookie.Value)
		return repository.Session{}, repository.ErrNotFound
	}
	return session, nil
}

func (s *Server) render(w http.ResponseWriter, _ *http.Request, name string, data any) {
	if err := s.templates.ExecuteTemplate(w, name, data); err != nil {
		log.WithContext(context.Background()).Error("render template failed", zap.String("template", name), zap.Error(err))
		http.Error(w, "render page failed", http.StatusInternalServerError)
	}
}

func splitLines(raw string) []string {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		item := strings.TrimSpace(line)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func buildTaskFromForm(r *http.Request, enabled bool) task.Task {
	timeoutSeconds, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("timeout_seconds")))
	retry, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("retry")))

	item := task.Task{
		Name:        strings.TrimSpace(r.FormValue("name")),
		Description: strings.TrimSpace(r.FormValue("description")),
		Type:        task.Type(strings.TrimSpace(r.FormValue("type"))),
		RunAsUser:   strings.TrimSpace(r.FormValue("run_as_user")),
		CronExpr:    strings.TrimSpace(r.FormValue("cron_expr")),
		Timeout:     time.Duration(timeoutSeconds) * time.Second,
		Retry:       retry,
		Enabled:     enabled,
		Command: task.CommandSpec{
			Command: "bash",
			Args:    []string{"run.sh"},
			Env:     splitLines(r.FormValue("env")),
			Script:  r.FormValue("run_script"),
		},
	}

	if item.RunAsUser == "" {
		item.RunAsUser = os.Getenv("USER")
	}
	if item.Type == task.TypeDaemon {
		item.Timeout = 0
		item.Daemon = task.DaemonConfig{Autostart: true, Restart: "always"}
	}
	if item.Command.Command == "" {
		item.Command.Command = "bash"
		item.Command.Args = []string{"run.sh"}
	}
	return item
}

func displayStatus(status task.Status, enabled bool) string {
	if !enabled {
		return "disabled"
	}
	if status == "" {
		return string(task.StatusPending)
	}
	return string(status)
}

func currentOutput(result task.RunResult) string {
	if result.Status != task.StatusRunning {
		return ""
	}
	logPath := result.LogPath
	if logPath == "" {
		logPath = result.TempLogPath
	}
	if logPath == "" {
		return ""
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	const limit = 1 << 20
	if len(raw) > limit {
		raw = raw[len(raw)-limit:]
	}
	return string(raw)
}

func sanitizedRedirectTarget(r *http.Request) string {
	target := strings.TrimSpace(r.FormValue("redirect_to"))
	if target == "" {
		target = "/tasks?task=" + url.QueryEscape(r.PathValue("name"))
	}

	parsed, err := url.Parse(target)
	if err != nil || parsed.IsAbs() || !strings.HasPrefix(parsed.Path, "/") {
		parsed = &url.URL{Path: "/tasks"}
	}

	cleanPath := path.Clean(parsed.Path)
	if cleanPath == "." {
		cleanPath = "/tasks"
	}
	if !strings.HasPrefix(cleanPath, "/") {
		cleanPath = "/" + cleanPath
	}
	parsed.Path = cleanPath
	return parsed.String()
}

func setFlash(w http.ResponseWriter, kind, message string) {
	if strings.TrimSpace(message) == "" {
		return
	}

	payload := base64.RawURLEncoding.EncodeToString([]byte(kind + "\n" + message))
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookieName,
		Value:    payload,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func popFlash(w http.ResponseWriter, r *http.Request) (string, string) {
	cookie, err := r.Cookie(flashCookieName)
	if err != nil || cookie.Value == "" {
		return "", ""
	}

	clearFlash(w)

	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return "", ""
	}

	parts := strings.SplitN(string(raw), "\n", 2)
	if len(parts) != 2 {
		return "", ""
	}

	switch parts[0] {
	case "message":
		return parts[1], ""
	case "error":
		return "", parts[1]
	default:
		return "", ""
	}
}

func clearFlash(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookieName,
		Path:     "/",
		Value:    "",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
