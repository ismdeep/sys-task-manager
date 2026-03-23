package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ismdeep/log"
	"go.uber.org/zap"

	"github.com/ismdeep/sys-task-manager/internal/auth"
	"github.com/ismdeep/sys-task-manager/internal/conf"
	"github.com/ismdeep/sys-task-manager/internal/executor"
	"github.com/ismdeep/sys-task-manager/internal/manager"
	"github.com/ismdeep/sys-task-manager/internal/repository"
	"github.com/ismdeep/sys-task-manager/internal/store"
	"github.com/ismdeep/sys-task-manager/internal/task"
	"github.com/ismdeep/sys-task-manager/internal/web"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Init("console://[stdout]?level=info&time_encoder=rfc3339&caller_encoder=none&trace_level=error")

	if err := validateRuntime(); err != nil {
		log.WithContext(context.Background()).Warn("runtime validation failed", zap.Error(err))
		os.Exit(1)
	}

	// 加载配置
	cfg, err := conf.Load()
	if err != nil {
		log.WithContext(ctx).Fatal("load configuration failed", zap.Error(err))
	}

	tempRunLogDir := filepath.Join(os.TempDir(), "sys-task-manager", "run-logs")
	if err := os.MkdirAll(tempRunLogDir, 0o755); err != nil {
		log.WithContext(ctx).Error("create temp log directory failed", zap.Error(err))
		os.Exit(1)
	}

	repo, err := repository.NewSQLiteRepository(filepath.Join("data", "task_manager.db"))
	if err != nil {
		log.WithContext(ctx).Error("init sqlite repository failed", zap.Error(err))
		os.Exit(1)
	}
	defer func() { _ = repo.Close() }()

	if err := repo.Init(ctx); err != nil {
		log.WithContext(ctx).Error("init database schema failed", zap.Error(err))
		os.Exit(1)
	}
	if err := repo.CleanupExpiredSessions(ctx); err != nil {
		log.WithContext(ctx).Error("cleanup expired sessions failed", zap.Error(err))
		os.Exit(1)
	}

	adminUser := "admin"
	adminPassword := "admin123"
	adminHash, err := web.PasswordHash(adminPassword)
	if err != nil {
		log.WithContext(ctx).Error("hash admin password failed", zap.Error(err))
		os.Exit(1)
	}
	if err := repo.EnsureAdmin(ctx, adminUser, adminHash); err != nil {
		log.WithContext(ctx).Error("seed admin user failed", zap.Error(err))
		os.Exit(1)
	}

	authorize, err := auth.NewAuthorizer()
	if err != nil {
		log.WithContext(ctx).Error("init run-as authorizer failed", zap.Error(err))
		os.Exit(1)
	}

	runLog, err := store.NewFileRunLogger(filepath.Join("data", "logs"), repo)
	if err != nil {
		log.WithContext(ctx).Error("init log store failed", zap.Error(err))
		os.Exit(1)
	}

	mgr := manager.New(authorize, executor.New(authorize, tempRunLogDir), runLog, 4)

	if err := seedExampleTasks(ctx, repo, authorize); err != nil {
		log.WithContext(ctx).Error("seed example tasks failed", zap.Error(err))
		os.Exit(1)
	}

	tasks, err := repo.ListTasks(ctx)
	if err != nil {
		log.WithContext(ctx).Error("load tasks from database failed", zap.Error(err))
		os.Exit(1)
	}
	for _, item := range tasks {
		if err := mgr.Register(item); err != nil {
			log.WithContext(ctx).Error("register task failed", zap.String("task", item.Name), zap.Error(err))
			os.Exit(1)
		}
	}

	if err := mgr.Start(ctx); err != nil {
		log.WithContext(ctx).Error("start manager failed", zap.Error(err))
		os.Exit(1)
	}

	webServer, err := web.NewServer(repo, mgr)
	if err != nil {
		log.WithContext(ctx).Error("init web server failed", zap.Error(err))
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:              cfg.ServerAddr,
		Handler:           webServer.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.WithContext(ctx).Info("task manager http server started", zap.String("addr", httpServer.Addr))
	serverErr := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case <-ctx.Done():
		log.WithContext(ctx).Info("shutdown signal received")
	case err := <-serverErr:
		if err != nil {
			log.WithContext(ctx).Error("http server failed", zap.Error(err))
			os.Exit(1)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.WithContext(shutdownCtx).Error("shutdown http server failed", zap.Error(err))
	}
	if err := mgr.Shutdown(shutdownCtx); err != nil {
		log.WithContext(shutdownCtx).Error("shutdown task manager failed", zap.Error(err))
	}

	log.WithContext(ctx).Info("task manager stopped")
}

func seedExampleTasks(ctx context.Context, repo *repository.SQLiteRepository, authorize *auth.Authorizer) error {
	current := authorize.Current()
	defaultExamples := []task.Task{
		{
			Name:      "daemon-heartbeat",
			Type:      task.TypeDaemon,
			RunAsUser: current.Username,
			Timeout:   0,
			Retry:     0,
			Enabled:   true,
			Command: task.CommandSpec{
				Command: "sh",
				Args:    []string{"-c", "echo daemon-start:$USER; trap 'exit 0' TERM INT; while true; do sleep 60; done"},
			},
		},
		{
			Name:      "cron-whoami",
			Type:      task.TypeCron,
			RunAsUser: current.Username,
			CronExpr:  "*/15 * * * * *",
			Timeout:   5 * time.Second,
			Retry:     0,
			Enabled:   true,
			Command: task.CommandSpec{
				Command: "sh",
				Args:    []string{"-c", "echo cron:$USER:$(date +%T)"},
			},
		},
		{
			Name:      "manual-self-check",
			Type:      task.TypeManual,
			RunAsUser: current.Username,
			Timeout:   5 * time.Second,
			Retry:     1,
			Enabled:   true,
			Command: task.CommandSpec{
				Command: "sh",
				Args:    []string{"-c", "echo manual:$USER && id"},
			},
		},
	}

	if current.IsRoot {
		if normalUser, ok := firstAvailableUser([]string{"nobody", "daemon", "ubuntu"}); ok {
			defaultExamples = append(defaultExamples, task.Task{
				Name:      "manual-root-task",
				Type:      task.TypeManual,
				RunAsUser: "root",
				Timeout:   5 * time.Second,
				Enabled:   true,
				Command: task.CommandSpec{
					Command: "sh",
					Args:    []string{"-c", "echo root-task:$USER && id"},
				},
			})
			defaultExamples = append(defaultExamples, task.Task{
				Name:      "manual-run-as-normal-user",
				Type:      task.TypeManual,
				RunAsUser: normalUser,
				Timeout:   5 * time.Second,
				Enabled:   true,
				Command: task.CommandSpec{
					Command: "sh",
					Args:    []string{"-c", "echo normal-user-task:$USER && id"},
				},
			})
			log.WithContext(ctx).Info("root examples seeded", zap.String("normal_user", normalUser))
		}
	}

	for _, sample := range defaultExamples {
		settingKey := fmt.Sprintf("default_task_created.%v", sample.Name)
		setting, err := repo.GetSetting(ctx, settingKey)
		if err == nil && setting.Value == "1" {
			continue
		}
		if err != nil && !errors.Is(err, repository.ErrNotFound) {
			return err
		}

		if _, err := repo.EnsureTask(ctx, sample); err != nil {
			return err
		}
		if err := repo.SetSetting(ctx, settingKey, "1"); err != nil {
			return err
		}
	}

	return nil
}

func validateRuntime() error {
	for _, bin := range []string{"sh", "id"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("required binary %q not found: %w", bin, err)
		}
	}
	return nil
}

func firstAvailableUser(candidates []string) (string, bool) {
	rootView := auth.NewAuthorizerWithIdentity(auth.Identity{
		Username: "root",
		UID:      0,
		GID:      0,
		IsRoot:   true,
	})

	for _, candidate := range candidates {
		if _, err := rootView.ResolveRunAs(candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}
