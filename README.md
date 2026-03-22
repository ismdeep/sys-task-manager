# System Task Manager

一个面向 Linux 场景的 Go 任务管理器，统一支持：

- 常驻任务（Daemon Task）
- 定时任务（Cron Task）
- 手动任务（Manual Task）

系统采用 Web 浏览器登录式管理，服务端统一调度任务；任务元数据、运行状态和登录会话存储到 `sqlite3`，任务执行日志和标准输出内容持久化到本地目录。当前核心实现已经覆盖 `RunAsUser` 权限模型、基于子进程的安全执行机制、超时与重试、重复执行保护、最大并发限制、任务状态跟踪，以及任务运行日志持久化。

## 补充技术方案

### 1. 管理方式

- 管理端只提供 Web 方式，不提供 CLI 管理入口。
- 用户通过浏览器访问管理后台并登录。
- 登录成功后可进行：
  - 任务新增、编辑、启停
  - 手动触发 `manual` / `cron` 任务
  - 查看任务运行状态
  - 查看最近执行记录
  - 查看本地日志文件索引与下载入口

### 2. 存储分层

- `sqlite3`
  - 存储任务定义
  - 存储任务状态
  - 存储任务执行历史元数据
  - 存储管理员账户、密码摘要、会话信息
- 本地目录日志
  - 存储每次任务执行产生的 stdout/stderr 原始文本
  - 存储运行日志文件索引，便于按任务、日期、执行批次归档

推荐目录结构：

```text
data/
├── task_manager.db
└── logs/
    ├── daemon-heartbeat/
    │   └── 20260321-144247.log
    ├── cron-whoami/
    │   └── 20260321-145250.log
    └── manual-self-check/
        └── 20260321-144247.log
```

### 3. 为什么使用 sqlite3

- 单机部署简单，适合任务调度器这类本地系统服务
- 无需额外数据库进程，降低运维复杂度
- 支持事务，适合任务注册、状态更新、执行记录落库
- 后续如果要升级到 MySQL/PostgreSQL，可以保持 repository 抽象不变

### 4. 日志为什么放本地目录

- 任务输出通常是大文本，不适合完整存入 sqlite BLOB/TEXT
- 文件方式更适合流式写入、归档、压缩和按日期清理
- sqlite 中只保存日志路径、大小、执行批次等索引信息即可

### 5. 推荐的管理链路

1. 浏览器访问 `/login`
2. 服务端校验用户名和密码摘要
3. 写入 session/cookie
4. 登录后访问 `/tasks`
5. 页面通过 HTTP API 调用任务管理服务
6. 服务端把任务定义和状态写入 sqlite3
7. 任务执行时把元数据写 sqlite3，把原始输出写本地日志目录

## 架构设计

### 1. 核心模块

- `internal/task`
  - 定义统一任务模型、任务类型、执行结果、任务状态。
- `internal/auth`
  - 负责解析当前进程用户。
  - 校验 `RunAsUser` 是否存在。
  - 根据“当前用户是否 root”执行权限判断。
- `internal/web`
  - 提供浏览器登录、会话校验、任务管理页面和 HTTP API。
- `internal/repository`
  - 基于 `sqlite3` 持久化任务定义、任务状态、执行历史、管理员账号和登录会话。
- `internal/executor`
  - 使用 `exec.CommandContext` 创建子进程。
  - 使用 `SysProcAttr.Credential{Uid, Gid}` 按指定用户身份执行。
  - 支持 `context cancel`、超时、重试、标准输出/错误收集。
- `internal/manager`
  - 统一管理任务注册、状态维护、运行控制。
  - 管理 daemon 启动、cron 注册，以及 `manual` / `cron` 的手动触发。
  - 防止同名任务重复执行，并通过信号量限制最大并发数。
- `internal/scheduler`
  - 提供轻量级 cron 调度能力。
  - 当前支持 5 位或 6 位 cron 表达式，以及 `* /n range list` 等常见写法。
- `internal/store`
  - 将任务原始输出落到本地目录。
  - 将日志路径、文件大小、执行批次号回写 sqlite3。

### 2. 任务执行链路

1. 注册任务时先做结构校验。
2. `auth.Authorizer` 对 `RunAsUser` 做权限校验。
3. Web/API 层接收请求并调用 `manager.Manager`。
4. `repository` 将任务定义、状态和执行记录写入 sqlite3。
5. 执行任务时构造 `context + timeout`。
6. `executor.Executor` 启动子进程，并通过 `SysProcAttr.Credential` 设置 UID/GID。
7. 执行输出写入本地日志目录，摘要与索引写回 sqlite3。
8. 页面查询 sqlite3 中的任务状态和执行记录用于展示。

### 3. 权限模型

- 默认执行用户：当前进程用户。
- 当前进程是 `root`：
  - 允许指定任意存在的 `RunAsUser`。
- 当前进程不是 `root`：
  - 只允许以当前用户执行。
  - 如果指定其他用户，直接返回错误。

### 4. 为什么不在当前进程 `setuid`

当前实现严格避免在主进程内直接调用 `setuid/setgid`，因为那会影响整个服务进程。执行器始终通过新建子进程并设置 `SysProcAttr.Credential` 的方式切换身份，符合服务化场景的安全实践。

## 项目结构

```text
.
├── data
│   ├── task_manager.db
│   └── logs
├── go.mod
├── go.sum
├── main.go
├── internal
│   ├── auth
│   │   └── auth.go
│   ├── executor
│   │   └── executor.go
│   ├── manager
│   │   └── manager.go
│   ├── repository
│   │   └── sqlite.go
│   ├── scheduler
│   │   └── scheduler.go
│   ├── store
│   │   └── runlog.go
│   ├── task
│   │   └── task.go
│   └── web
│       ├── server.go
│       └── templates
│           ├── login.html
│           ├── runs.html
│           └── tasks.html
```

### 建议的 sqlite3 表设计

`users`

- `id`
- `username`
- `password_hash`
- `created_at`
- `updated_at`

`sessions`

- `id`
- `user_id`
- `token`
- `expired_at`
- `created_at`

`tasks`

- `id`
- `name`
- `description`
- `type`
- `run_as_user`
- `cron_expr`
- `timeout_seconds`
- `retry`
- `command`
- `args_json`
- `work_dir`
- `env_json`
- `enabled`
- `created_at`
- `updated_at`

`task_runs`

- `id`
- `task_id`
- `trigger_mode`
- `run_as_user`
- `status`
- `attempt`
- `started_at`
- `finished_at`
- `duration_ms`
- `error_message`
- `log_path`
- `created_at`

### 当前已实现的 Web 管理接口

- `GET /login`
- `POST /login`
- `POST /logout`
- `GET /tasks`
- `POST /tasks`
- `POST /tasks/{name}/run`
- `GET /tasks/{name}/runs`
- `GET /runs/{id}/log`

## 任务模型

每个任务至少包含以下字段：

- `Name`
- `Description`
- `Type`
- `RunAsUser`
- `CronExpr`
- `Timeout`
- `Retry`

同时补充了统一命令执行结构：

- `Command.Command`
- `Command.Args`
- `Command.WorkDir`
- `Command.Env`

## 示例任务

### root 执行任务

当进程用户是 `root` 时，`main.go` 会注册：

- `manual-root-task`：以 `root` 身份执行
- `manual-run-as-normal-user`：以普通用户（优先 `nobody/daemon/ubuntu`）执行

### 指定普通用户执行任务

`manual-run-as-normal-user` 会通过：

- `RunAsUser: "nobody"`（或其他存在的普通用户）
- `SysProcAttr.Credential{Uid, Gid}`

以指定普通用户身份启动子进程。

### 非 root 用户尝试指定其他用户

如果当前进程不是 root，则以下场景会报错：

- 注册任务时指定 `RunAsUser: "root"`
- 手动触发时覆盖 `overrideAs = "root"`

## 运行方式

```bash
go run .
```

默认启动后：

- HTTP 服务监听 `:8080`
- 默认管理员账号：`admin`
- 默认管理员密码：`admin123`
- 示例任务会自动写入 `sqlite3`
- 任务执行日志会写入 `data/logs/`

可通过环境变量覆盖：

- `TASK_MANAGER_ADDR`
- `TASK_MANAGER_ADMIN_USER`
- `TASK_MANAGER_ADMIN_PASSWORD`

## 工程实践亮点

- 使用 `context` 管理生命周期与 graceful shutdown
- 使用 goroutine + channel 做并发控制
- 提供内置 cron scheduler，离线环境也可直接编译运行
- 推荐以 Web 登录后台作为唯一管理入口
- 推荐以 sqlite3 作为任务元数据和会话存储
- 推荐以本地目录作为任务输出日志存储
- 任务状态：`pending / running / success / failed`
- 任务日志采用“数据库存索引 + 文件系统存内容”的组合方案
