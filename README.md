# System Task Manager

一个面向 Linux 场景的 Go 任务管理器，统一支持：

- 常驻任务（Daemon Task）
- 定时任务（Cron Task）
- 手动任务（Manual Task）

系统采用 Web 浏览器登录式管理，服务端统一调度任务；任务定义通过当前目录下的 `workspaces/<task-name>/config.json` 和 `run.sh` 管理，任务 stdout/stderr 和运行结果直接输出到 `sys-task-manager` 程序自身日志中。当前核心实现已经覆盖 `RunAsUser` 权限模型、基于子进程的安全执行机制、超时与重试、重复执行保护、最大并发限制、任务状态跟踪，以及进程内任务运行记录。

## 补充技术方案

### 1. 管理方式

- 管理端只提供 Web 方式，不提供 CLI 管理入口。
- 用户通过浏览器访问管理后台并登录。
- 登录成功后可进行：
  - 任务新增、编辑、启停
  - 手动触发 `manual` / `cron` 任务
  - 查看任务运行状态
  - 查看最近执行记录
  - 查看最近运行输出

### 2. 存储分层

- `workspaces/<task-name>/`
  - `run.sh` 是任务实际执行脚本。
  - `config.json` 描述任务类型、cron spec、超时、重试、执行用户和环境变量。
- `data/`
  - `users.json` 存储管理员账户和密码摘要。
  - `settings.json` 存储本地设置。
- 登录会话
  - 保存在程序内存中，仅当前进程生命周期内有效，程序重启后需要重新登录。
- 运行记录
  - 保存在程序内存中，仅当前进程生命周期内可查，程序重启后清空。

推荐目录结构：

```text
data/
├── settings.json
└── users.json
workspaces/
└── hello-world/
    ├── config.json
    └── run.sh
```

示例 `config.json`：

```json
{
  "type": "cron",
  "run_as_user": "ismdeep",
  "cron": {
    "spec": "*/15 * * * * *"
  },
  "timeout_seconds": 30,
  "retry": 0,
  "enabled": true
}
```

daemon 任务只需要设置 `type` 为 `daemon`。程序启动后会自动启动该任务；任务退出后会自动重启，直到任务被禁用、删除或服务关闭。

```json
{
  "type": "daemon",
  "enabled": true
}
```

### 3. 日志策略

- 任务 stdout/stderr 按行写入 `sys-task-manager` 程序自身日志。
- 任务完成后，最终状态、耗时、执行用户、错误和保留输出摘要也写入程序自身日志。
- 程序运行时不会生成每次运行的 `.log`、`.result` 或 `runs.json` 文件。
- Web 页面上的运行记录来自当前进程内存，服务重启后不保留。

### 4. 推荐的管理链路

1. 浏览器访问 `/login`
2. 服务端校验用户名和密码摘要
3. 写入 session/cookie
4. 登录后访问 `/tasks`
5. 页面通过 HTTP API 调用任务管理服务
6. 服务端把任务定义写入 `workspaces/<name>/config.json` 和 `run.sh`
7. 任务执行时把 stdout/stderr 和运行结果写入 `sys-task-manager` 程序自身日志，运行记录摘要保存在当前进程内存中

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
  - 基于本地文件持久化任务定义、管理员账号和本地设置。
  - 登录会话和运行记录仅保存在进程内存中。
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
  - 整理任务输出摘要。
  - 将运行记录交给 repository 的内存记录区保存。

### 2. 任务执行链路

1. 注册任务时先做结构校验。
2. `auth.Authorizer` 对 `RunAsUser` 做权限校验。
3. Web/API 层接收请求并调用 `manager.Manager`。
4. `repository` 将任务定义写入 workspace，将执行记录放入当前进程内存。
5. 执行任务时构造 `context + timeout`。
6. `executor.Executor` 启动子进程，并通过 `SysProcAttr.Credential` 设置 UID/GID。
7. 执行输出按行写入 `sys-task-manager` 程序自身日志，最终结果也写入程序自身日志。
8. 页面查询 workspace 和当前进程内存中的运行记录用于展示。

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
│   ├── settings.json
│   └── users.json
├── workspaces
│   └── hello-world
│       ├── config.json
│       └── run.sh
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
│   │   └── file.go
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

### Workspace 配置字段

`workspaces/<name>/config.json`

- `type`: `cron`、`daemon` 或 `manual`
- `description`
- `run_as_user`
- `enabled`
- `timeout_seconds`: 仅对 `cron` / `manual` 生效，`daemon` 不使用 timeout
- `retry`
- `env`
- `cron.spec`: cron 任务的表达式
- `type = daemon` 时任务会随服务启动并持续运行，退出后自动重启

`workspaces/<name>/run.sh`

- 服务端会在该 workspace 目录中执行 `bash run.sh`
- Web 新建任务时会创建默认脚本，之后可以直接编辑该文件

任务运行输出

- 每次任务运行的 stdout 和 stderr 会写入 `sys-task-manager` 程序自身日志
- 首页显示当前正在执行任务的状态
- 历史运行记录仍可通过单独的 runs 页面查看

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

执行命令固定映射为 workspace 脚本：

- `Command.Command = "bash"`
- `Command.Args = ["run.sh"]`
- `Command.WorkDir = "workspaces/<name>"`
- `Command.Env` 来自 `config.json` 的 `env`

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

- HTTP 服务监听 `:46808`
- 默认管理员账号：`admin`
- 默认管理员密码：`admin123`
- 示例任务会自动写入 `workspaces/`
- 登录会话仅保存在当前进程内存中，重启后需要重新登录
- 运行记录仅保存在当前进程内存中，重启后清空

可通过环境变量覆盖：

- `TASK_MANAGER_ADDR`
- `TASK_MANAGER_ADMIN_USER`
- `TASK_MANAGER_ADMIN_PASSWORD`

## 工程实践亮点

- 使用 `context` 管理生命周期与 graceful shutdown
- 使用 goroutine + channel 做并发控制
- 提供内置 cron scheduler，离线环境也可直接编译运行
- 推荐以 Web 登录后台作为唯一管理入口
- 推荐以 workspace 目录作为任务定义来源
- 推荐以 `data/*.json` 保存用户/设置，会话和运行记录只保存在进程内存中
- 任务状态：`pending / running / success / failed`
- 任务日志输出到 `sys-task-manager` 程序自身日志，并在运行记录中保留尾部输出摘要
