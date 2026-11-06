# Nexus 技术设计（Technical Design）

> 版本：v1.0 · 日期：2026-11-06 · 状态：已评审定稿
> 关联文档：[架构设计](architecture.md)

## 1. 技术栈清单

| 项 | 选型 | 说明 |
|---|---|---|
| 语言 | Go 1.24+ | 模块名 `github.com/dddpeter/agent-nexus` |
| 路由 | `go-chi/chi/v5` | 轻量 REST |
| 前端 | htmx + Alpine.js（嵌入 `web/` 静态资源，`embed` 打包） | 单二进制；API 先行，UI 可后换 Vue |
| 编排 | Temporal Go SDK；`docker compose` 起 local dev server | 降级：内置轻量调度器 |
| 存储 | `modernc.org/sqlite`（纯 Go 免 CGO，WAL） | repository 层收敛 SQL |
| 迁移 | `pressly/goose` | 嵌入式 SQL 迁移 |
| 配置 | YAML（`gopkg.in/yaml.v3`）+ 环境变量覆盖 | `config.local.yaml` 不入库 |
| 日志 | `log/slog`（JSON handler） | 请求 ID 贯穿 |
| 实时推送 | SSE（`/api/stream`） | 消息、任务进度、agent 状态 |

## 2. 目录结构

```
agent-nexus/
├── cmd/nexus/              # 入口：serve / agent / task / group 子命令
├── internal/
│   ├── api/                # REST handler、SSE、鉴权中间件
│   ├── bus/                # 事件总线：channel 扇出 + event_log 异步落库
│   ├── session/            # 私聊路由、群聊协调器、话题状态机
│   ├── arbiter/            # 发言仲裁器（纯函数核心 + 策略配置注入）
│   ├── orchestr/           # Temporal workflows & activities；内置降级调度器
│   ├── adapter/            # AgentAdapter 接口 + 各实现
│   │   ├── adapter.go      #   接口、AgentInfo、能力结构
│   │   ├── echo/           #   测试用假 agent
│   │   ├── hermes/         #   HTTP 型范式
│   │   ├── picoclaw/       #   CLI 型范式
│   │   └── opencode/ ...
│   ├── registry/           # 注册、心跳、健康、MaxConcurrency 闸门
│   ├── repo/               # SQLite repository（唯一 SQL 所在地）
│   └── config/
├── migrations/             # goose SQL 迁移
├── web/                    # htmx 前端静态资源
├── deploy/docker-compose.yml  # nexus + temporal
└── docs/
```

## 3. 数据模型（DDL 要点）

```sql
CREATE TABLE agents (
  id TEXT PRIMARY KEY,            -- nanoid
  name TEXT UNIQUE NOT NULL,
  adapter TEXT NOT NULL,          -- 'hermes' | 'picoclaw' | ...
  config_json TEXT NOT NULL,      -- adapter 专有配置（endpoint/命令行模板等）
  capabilities_json TEXT NOT NULL,-- CanGroupChat/CanStream/MaxConcurrency/...
  status TEXT NOT NULL DEFAULT 'offline',  -- online|offline|busy
  last_heartbeat_at DATETIME
);

CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  agent_id TEXT REFERENCES agents(id),
  kind TEXT NOT NULL,             -- private|group|task
  external_ref TEXT,              -- agent 侧会话标识（--session id / HTTP session）
  topic_id TEXT,
  created_at DATETIME, closed_at DATETIME
);

CREATE TABLE messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  sender_type TEXT NOT NULL,      -- human|agent|system
  sender_id TEXT NOT NULL,
  content TEXT NOT NULL,
  reply_to TEXT,
  created_at DATETIME NOT NULL
);
CREATE INDEX idx_messages_session ON messages(session_id, created_at);

CREATE TABLE event_log (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  type TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  created_at DATETIME NOT NULL
);

CREATE TABLE groups (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  members_json TEXT NOT NULL,     -- [{id,type,trigger,budget,priority}]
  arbiter_config_json TEXT NOT NULL
);

CREATE TABLE tasks (
  id TEXT PRIMARY KEY,
  workflow_id TEXT,
  agent_id TEXT,
  input TEXT, status TEXT,        -- queued|running|done|failed|cancelled
  result TEXT, error TEXT,
  timeout_seconds INT, retries INT,
  created_at DATETIME, finished_at DATETIME
);
```

## 4. AgentAdapter 接口与两个范式实现

### 4.1 接口与数据结构

```go
type AgentInfo struct {
    ID, Name, Adapter string
    Capabilities Capabilities
}
type Capabilities struct {
    CanGroupChat, CanStream bool
    MaxConcurrency int
    TypicalLatency time.Duration
    InterruptLevel int   // 0=禁止插话 1=可申请 2=可直接插
}
type Message struct { Content string; Attachments []Attachment; ContextDigest string }
type Reply struct { Content string; SessionRef string; Done bool }

type AgentAdapter interface {
    Info() AgentInfo
    CreateSession(ctx context.Context, opts SessionOpts) (SessionID, error)
    Send(ctx context.Context, sid SessionID, msg Message) (Reply, error)
    Stream(ctx context.Context, sid SessionID, msg Message) (<-chan Event, func(), error)
    Close(ctx context.Context, sid SessionID) error
}
```

### 4.2 HTTP 型范式（以 hermes 为例）

- `CreateSession`：POST gateway 建 session，保存 `external_ref`
- `Send`：POST 消息 → 轮询/长连接等结果 → `Reply`
- `Stream`：订阅 gateway 流式接口，转译为统一 `Event`（`delta` / `final` / `error`）
- 健康：GET /health，间隔 15s，3 连败 offline
- 三个 profile（default/coder/research）注册为三个逻辑 agent

### 4.3 CLI 型范式（以 picoclaw 为例）

- 会话映射：`external_ref` → 复用 `--session <id>` 参数续话；无会话机制的 agent 则每次新进程 + 平台侧拼上下文
- 进程管理：
  ```go
  cmd := exec.CommandContext(ctx, bin, args...)
  cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 进程组
  // 超时：ctx + 平台 child_timeout 双保险；取消时 kill(-pgid)
  ```
- 输出解析：优先 `--output json`；否则行协议 wrapper（adapter 自带模板脚本）；解析失败重试一次后报 error
- 并发闸门：per-agent semaphore（容量 = `MaxConcurrency`），CLI 型默认 1

## 5. 事件总线实现

```go
type Bus interface {
    Publish(ctx context.Context, e Event) error      // 内存扇出 + 异步写 event_log
    Subscribe(topics ...string) (<-chan Event, func()) // 返回 unsubscribe
}
```

- 每订阅者独立 buffered channel（默认 256），慢消费者丢弃计数并告警，不阻塞总线
- event_log 落库走单独 goroutine + 批量 flush（100 条/500ms）
- 事件 ID 用 `event_log.seq`，消费位点按需支持（一期不做 replay，schema 已留 seq）

## 6. 仲裁器实现

```go
type Decision struct { Allow bool; Reason string; Deferred []Message }
func (a *Arbiter) Evaluate(ctx context.Context, in Input) Decision
```

- **纯函数核心**：输入（新消息、群状态、成员配置）→ 决策，无 IO，全量单测
- 群状态（每群一份，内存）：当前回合占用者、各 agent 本话题已发言次数、agent↔agent 链式计数、静默轮数
- 决策落 `arbiter.decision` 事件（审计为何 allowed/dropped）
- 阈值全部来自 `groups.arbiter_config_json`，API 可热更新
- 冻结逻辑：静默轮数 ≥ N → 冻结、生成讨论摘要（取最近 K 条消息）、@主持人

## 7. 编排层（Temporal）

### Workflows

- `TaskChainWorkflow`：DAG 执行——节点 = {agentID, inputTemplate, timeout, retries}，前节点 `Reply` 填入后节点 `inputTemplate` 占位符 `{{prev}}`
- `ScheduledReportWorkflow`：cron 触发 → 派任务 → 结果回填消息/存档
- `AsyncReplyWorkflow`：私聊长任务 → 挂起 → 完成或超时回报

### Activities 与重试

- 每个 agent 调用 = 一个 activity，`RetryPolicy{InitialInterval: 2s, BackoffCoefficient: 2, MaximumAttempts: 按任务}`
- activity 内先查 registry：offline → 直接返回可重试错误；并发闸门满 → Temporal 队列天然排队

### 降级实现

`orchestr.Scheduler` 接口：`Submit(ctx, wfDef) (workflowID, error)` / `Cancel` / `Status`。Temporal 实现 + 内置 goroutine 池实现（仅串行/并行 DAG，无持久化恢复），配置 `orchestration.engine: temporal|builtin` 切换。

## 8. API 设计（REST 一期）

```
POST   /api/agents                  注册 agent（adapter+config+capabilities）
GET    /api/agents                  列表与健康状态
POST   /api/sessions                建私聊会话 {agent_id}
POST   /api/sessions/{id}/messages  发消息（同步等回复 or SSE 订阅）
POST   /api/groups                  建群（成员+仲裁配置）
POST   /api/groups/{id}/messages    群发言（human）
GET    /api/groups/{id}/stream      SSE：群消息实时流
POST   /api/tasks                   提交任务/DAG {chain:[...]} / {cron:...}
GET    /api/tasks/{id}              状态与结果
POST   /api/tasks/{id}/cancel
GET    /api/stream                  全局 SSE（agent 状态、任务生命周期）
```

鉴权：单用户 token（`Authorization: Bearer`），一期不做多用户。

## 9. 配置样例

```yaml
server:
  listen: :8080
  auth_token: ${NEXUS_TOKEN}
store:
  driver: sqlite
  dsn: file:data/nexus.db?_journal=WAL
orchestration:
  engine: temporal        # builtin 为降级
  temporal:
    host: localhost:7233
    namespace: nexus
registry:
  heartbeat_interval: 15s
  offline_threshold: 3
  default_child_timeout: 1800s
agents:
  - name: hermes-coder
    adapter: hermes
    config: { profile: coder, base_url: http://localhost:8600 }
  - name: picoclaw-main
    adapter: picoclaw
    config:
      command: ["picoclaw", "agent", "-p", "{{input}}", "--session", "{{session}}", "--output", "json"]
```

## 10. 测试策略

- **单测**：仲裁器纯函数全分支、事件总线扇出/背压、repository CRUD
- **adapter 集成测试**：echo fake agent（可脚本化输出/延迟/故障注入）覆盖 CLI 与 HTTP 两范式；真实 agent 测试打 `//go:build e2e` tag
- **故障注入**：进程 kill、网关 500、输出畸形 JSON → 验证重试与 offline 判定
- **仲裁回归**：把「防死循环/抢话」场景做成表驱动用例，改阈值必跑

## 11. 部署

```yaml
# deploy/docker-compose.yml（要点）
services:
  nexus:      { build: ., ports: ["8080:8080"], volumes: ["./data:/app/data"] }
  temporal:   { image: temporalio/auto-setup, environment: [DB=sqlite] }
```

单机一条 `docker compose up -d`；`engine: builtin` 模式可裸跑单二进制不依赖 Temporal。

## 12. 里程碑交付对照

| 里程碑 | 交付物 | 验收标准 |
|---|---|---|
| M0 | bus/repo/registry/adapter 接口 + echo agent | CLI 注册 echo，发消息收回显 |
| M1 | hermes + picoclaw adapter | CLI 派任务两者返回真实回复 |
| M2 | 私聊 + Web UI + SSE | 浏览器双向聊天、任务列表 |
| M3 | 群聊协调器 + 仲裁器 | 人+2 agent 讨论不失控，阈值热调生效 |
| M4 | TaskChain/Scheduled workflow + opencode/openclaw | 协作链流水线 + cron 日报跑通 |
