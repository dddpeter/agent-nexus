# Nexus 详细设计（Feature-by-Feature）

> 版本：v1.0 · 日期：2026-11-06 · 状态：定稿
> 上游文档：[架构设计](architecture.md) · [技术设计](technical-design.md) · [测试验证计划](test-plan.md)
> Feature 总览：F1 Agent 纳管注册 / F2 私聊会话 / F3 群聊仲裁 / F4 任务编排 / F5 接入层 / F6 事件总线与存储 / F7 进程治理与健康

---

## F1 Agent 纳管与注册（Registry）

### F1.1 目标

统一登记所有被纳管 agent 的身份、驱动方式、能力与健康状态，为上层（会话/编排）提供「可用 agent 目录 + 并发闸门 + 状态查询」。

### F1.2 功能需求

- FR1.1 注册：三种途径——**Web UI 注册向导**、REST API、配置文件（启动时批量载入）；三者最终收敛到同一 `Register(spec)` 入口，语义一致
- FR1.1a **页面注册向导**：选 adapter 类型 → 动态渲染该 adapter 的配置表单（adapter 声明自己的 config schema）→ 填写能力参数（CanGroupChat/MaxConcurrency 等）→ 在线测试连通性（探活/试发一条消息）→ 提交注册
- FR1.1b **页面配置管理**：已注册 agent 的查看/编辑配置/停用/软删；配置变更需通过连通性测试才允许保存生效；密钥字段（api_key 等）页面回显脱敏，留空表示不修改
- FR1.2 能力声明：`CanGroupChat / CanStream / MaxConcurrency / TypicalLatency / InterruptLevel`
- FR1.3 状态机：`offline → online → busy → online`（busy 由并发闸门自动推导，不可手工设置）
- FR1.4 心跳：周期探活，连续 N 次失败自动 offline；恢复自动 online
- FR1.5 并发闸门：per-agent 信号量，容量 = MaxConcurrency；满时调用方进入排队
- FR1.6 注销：软删（status=removed），历史会话/任务保留可查

### F1.3 详细设计

**模块**：`internal/registry`

```go
type Registry interface {
    Register(ctx context.Context, spec AgentSpec) (Agent, error)
    Get(ctx context.Context, id string) (Agent, error)
    List(ctx context.Context, f StatusFilter) ([]Agent, error)
    Acquire(ctx context.Context, id string) (release func(), err error) // 并发闸门
    Heartbeat(ctx context.Context, id string) error
}
```

- Registry 内聚合 **AdapterFactory**：`adapter.New(name, config) (AgentAdapter, error)`，注册即实例化 adapter；配置校验失败直接拒绝注册
- **ConfigSchema 声明式表单**：每种 adapter 实现暴露 `ConfigSchema() Schema`（字段名/类型/必填/默认值/是否密钥），Web UI 注册向导据此动态渲染表单——新增 adapter 无需写任何前端页面
- **连通性测试**（`registry.TestConnection(spec)`）：用待保存的配置临时实例化 adapter，执行探活 + 可选试发消息，返回耗时与错误详情；页面「保存」与「测试」分离，测试通过才允许启用
- 心跳器：每 agent 独立 ticker（间隔可配，默认 15s）。CLI 型执行探活命令（如 `picoclaw --version`，超时 5s）；HTTP 型 GET health。结果写 `agents.last_heartbeat_at / status`
- 状态变更发布 `agent.status` 事件（含 from/to/reason）
- busy 推导：`inFlight >= MaxConcurrency` 时 API 层展示 busy，信号量本身用 `golang.org/x/sync/semaphore`
- 配置中的密钥类字段（api_key）：载入时解析 `${ENV_VAR}` 引用，日志输出时统一脱敏为 `***`

### F1.4 错误处理

| 场景 | 行为 |
|---|---|
| adapter 专有配置校验失败 | 注册拒绝，400 返回具体字段错误 |
| 页面连通性测试失败 | 保留表单，展示错误详情（连接 refused / 探活命令非零 / 超时）；允许保存为 disabled 状态 |
| 页面配置变更后探活失败 | 变更不生效回滚旧配置，提示失败原因；旧配置期间 agent 状态不变 |
| 探活超时/非零退出 | 计失败数，达阈值 → offline + 事件 |
| offline 期间收到 Acquire | 返回 `ErrAgentOffline`，编排层转可重试错误 |
| 注册重名 | 409，提示先注销或改名 |

### F1.5 边界与约定

- 一期不做 agent 侧主动注册（pull 模式，平台单向纳管）；二期 Sidecar 为 push 模式，接口预留 `Register(spec)` 幂等（同 name 覆盖更新）
- MaxConcurrency=0 视为非法配置（注册时拒绝），避免 0 语义歧义

---

## F2 私聊会话（Session Router）

### F2.1 目标

人与单个 agent 的 1v1 对话：会话创建/续话、同步回复、流式回复、长任务异步回报。

### F2.2 功能需求

- FR2.1 创建私聊会话（指定 agent），持久化会话映射
- FR2.2 发消息：同步模式（等 Reply，带超时）与流式模式（SSE 增量）
- FR2.3 会话续话：同一 session 的消息带上下文发给 agent（CLI 型用 `--session`，无会话机制的 agent 平台拼上下文窗口）
- FR2.4 异步回报：消息内容被判定为长任务（显式标记或 agent 回复声明"在做了"）→ 转 Temporal 挂起，完成后主动回填结果到会话
- FR2.5 会话关闭/超时回收（默认闲置 24h 关闭）

### F2.3 详细设计

**模块**：`internal/session/private`

```
SendMessage(ctx, sessionID, msg, mode) 流程：
1. repo 读 session → registry.Acquire(agent)
2. adapter.Send / Stream
3. 回复写 messages 表（异步）+ bus 发布 msg.agent 事件
4. mode=async → 创建 AsyncReplyWorkflow，立即返回 "已受理" 回执
```

- **上下文策略**（无会话机制 agent）：保留最近 K 轮（默认 10）+ 摘要压缩（超 20 轮触发，摘要用任意可用 agent 生成，失败则截断）
- **续话映射**：`sessions.external_ref` 存 agent 侧会话 ID；首次回复从 `Reply.SessionRef` 回填
- **异步回执协议**：`{type:"accepted", task_id, eta_hint}`；结果回填为 `sender_type=system, reply_to=原消息` 的消息
- 超时：同步模式默认 300s 可配；流式 60s 无 delta 判死，发 error 事件

### F2.4 错误处理

| 场景 | 行为 |
|---|---|
| agent offline | 409 提示，不排队（私聊即时性优先） |
| Send 超时 | 回复超时占位消息 + 事件；session 保留可重发 |
| agent 侧会话丢失（external_ref 失效） | 自动重建会话 + 注入上下文摘要，标记 system 提示 |
| 回填时 session 已关闭 | 结果只落库，事件标记 orphaned |

---

## F3 群聊与发言仲裁（Group Coordinator + Arbiter）

### F3.1 目标

人机混合群中多 agent 受控发言：@路由、回合制、发言预算、防死循环、插话通道。

### F3.2 功能需求

- FR3.1 建群：成员（human + agents）、触发方式、预算、优先级、仲裁参数
- FR3.2 人发言路由：@指定 → 唯一响应者；无 @ → 优先级×相关性选择
- FR3.3 agent 发言必须经仲裁；未获准的候选消息存档（Deferred）
- FR3.4 预算与话题：`max_turns_per_topic`（默认 2）；话题切换重置（显式 `/topic` 或静默 30min 自动切）
- FR3.5 防死循环：agent 回复不得直接触发其他 agent；人连续 N 轮（默认 4）未发言 → 冻结 + @人 + 摘要
- FR3.6 插话：`InterruptLevel=2` 直接放行；=1 需仲裁批准；=0 转站内信
- FR3.7 仲裁配置热更新（API 修改即时生效）

### F3.3 详细设计

**模块**：`internal/arbiter`（纯函数核心）+ `internal/session/group`（状态机壳）

```go
type Input struct {
    Group     GroupState     // 成员、预算、冻结标志
    Recent    []Message      // 本话题最近消息（相关性计算用）
    Incoming  Incoming       // 新消息：human 或 agent 候选
}
type Decision struct {
    Allow    bool
    Target   string          // 获准接话的 agent（人发言时）
    Deferred []Message       // 存档候选
    Actions  []Action        // Freeze / NotifyOwner / ResetTopic ...
}
```

**处理流水线**（订阅 bus 的 `msg.*` 事件）：

```
人发言 → 相关性选择（关键词重叠 + priority 加权，并列取最近未发言者）
       → 路由给 Target，触发其 Stream
agent 候选 → 查回合占用：本回合已有接话者？→ Deferred
           → 查预算：超 max_turns_per_topic？→ Deferred + 降权
           → 查链式计数：上一条发言者是 agent 且本轮无人发言？→ 拒绝（防连环）
           → 通过 → 发布 msg.agent
静默计数：每轮结束检查 human silent rounds ≥ N → Freeze + 生成摘要 + @主持人
摘要生成：调用群内 CanStream 且预算未超的 agent（失败降级为截断最近消息）
```

- 状态持久化：群状态存内存，快照每 30s 异步落库（重启恢复，丢失容忍）
- 相关性算法一期用词面重叠（纯 Go 分词 [`gse`](https://github.com/go-ego/gse) + Jaccard，避免 CGO），接口抽象 `RelevanceScorer`，可替换为向量方案

### F3.4 错误处理

| 场景 | 行为 |
|---|---|
| 被路由 agent 超时/失败 | 本回合释放，按优先级顺延给次优 agent |
| 获准发言的 agent 产出空内容 | 记 system 事件，不占用其预算 |
| 冻结后仅主持人可解冻（`/unfreeze` 或 API） | 其余发言一律 Deferred |
| 所有 agent 均 offline | 群自动进入"仅人可见"模式，消息正常存档 |

### F3.5 设计取向

第一版仲裁宁严勿松：所有阈值保守取值，全部热可调；仲裁决策全量落 `arbiter.decision` 事件供回放调参。

---

## F4 任务编排（Orchestration）

### F4.1 目标

受控流水线：协作链 DAG、定时任务、超时重试、异步回报，基于 Temporal（含内置降级实现）。

### F4.2 功能需求

- FR4.1 协作链：`[{agentID, inputTemplate, timeout, retries}]` 顺序/并行执行，`{{prev}}` 占位符注入上节点输出
- FR4.2 fan-out/fan-in：并行分支 + 汇聚节点（等全部分支或任一分支）
- FR4.3 定时任务：cron 表达式，触发任意 chain 或单 agent 调用
- FR4.4 重试：activity 级 RetryPolicy，指数退避
- FR4.5 取消/恢复：运行中可 cancel；failed 可人工修参后 resume
- FR4.6 进度上报：activity 心跳 → Web UI 实时展示

### F4.3 详细设计

**模块**：`internal/orchestr`

```go
type ChainDef struct {
    Nodes []NodeDef
}
type NodeDef struct {
    AgentID   string
    Input     string            // 模板，支持 {{prev}} {{task.input}}
    Timeout   time.Duration
    Retries   int
    Parallel  []NodeDef         // 嵌套即 fan-out；兄弟节点默认汇聚等待
}
```

- **Temporal 侧**：一个 `TaskChainWorkflow`（单一 workflow，节点循环执行），每节点一个 activity；输入模板渲染在 activity 内做（保证 workflow 确定性）
- **降级 builtin**：goroutine 池 + 内存 DAG 调度，无恢复能力，重启后 running 任务标记 failed（文档明示差异）；接口 `orchestr.Scheduler` 与 Temporal 实现同签名
- 派发前置检查：registry 状态 + Acquire（闸门满则 activity 内阻塞等待，受节点 timeout 约束）
- **上下文注入**：`ContextDigest` 字段可携带群聊话题摘要（F2/F3 联动），渲染进 agent 输入
- 任务状态机：`queued → running → done | failed | cancelled`；节点级状态存 `tasks.nodes_json`（见技术设计 §3 DDL）

### F4.4 错误处理

| 场景 | 行为 |
|---|---|
| 节点失败重试耗尽 | 整链 failed，已完成节点结果保留，resume 从失败节点继续 |
| agent offline | activity 返回可重试错误；超重试次数 failed |
| 上节点输出超长（>64KB） | 截断 + 摘要注入 `{{prev}}`，原始输出存档 |
| cron 任务触发时 agent offline | 跳过本次并记录，不影响后续调度 |

---

## F5 接入层（Web UI + CLI）

### F5.1 目标

REST API + SSE 为底座，Web UI（Vue 3 SPA）与 CLI（`nexus` 命令）双入口。

### F5.2 功能需求

- FR5.1 API 全集（见技术设计 §8）+ Bearer token 鉴权
- FR5.2 SSE：`/api/stream`（全局）、`/api/groups/{id}/stream`、私聊消息流
- FR5.3 Web UI 页面：Agent 列表（状态/健康）、**Agent 注册向导**（选 adapter → ConfigSchema 动态表单 → 连通性测试 → 启用）、**Agent 详情/配置编辑页**（脱敏回显、变更测试、停用/软删）、私聊聊天页、群聊页（含仲裁可视化：谁被 Deferred 及原因）、任务列表与详情（DAG 进度）、配置页（阈值热调）
- FR5.4 CLI 子命令：`nexus serve / agent list / agent register / chat <agent> / group send / task submit / task status`；`chat` 支持交互式 REPL

### F5.3 详细设计

- API 层中间件链：recover → requestID → 鉴权 → 限流（简单窗口计数，60 req/min）→ 日志
- 鉴权双通道：API/CLI 用 `Authorization: Bearer`；浏览器用 `/api/auth/login` 换取的
  HttpOnly SameSite=Strict 会话 cookie（7 天，内存会话存储，重启后重新登录）。
  未配置 auth_token = 本地免登模式。SPA 静态 shell 不含数据，不参与鉴权；`/api` 全量鉴权
- SSE 实现：bus 订阅 → per-connection channel；断线用 `Last-Event-ID`（event_log.seq）补偿重放最近 500 条
- Web UI：Vue 3 + Vite + TypeScript 独立工程（`frontend/`），前后端分离——
  开发期 `vite dev`（HMR）+ `/api` 反代到 Go 后端；生产 `vite build` 产物 `web/dist`
  经 `go:embed` 打包单二进制，history fallback 由后端承担（非 `/api` GET → index.html）
- CLI：`spf13/cobra`；REPL 模式读 stdin 行，Ctrl-C 优雅取消当前请求（context cancel）

### F5.4 错误处理

统一错误结构 `{"error":{"code","message","request_id"}}`；SSE 心跳注释行每 15s 保活；CLI 网络错误重试 2 次（幂等 GET）。

---

## F6 事件总线与存储（Bus + Store）

### F6.1 目标

进程内事件扇出 + 追加写持久化；SQLite 单文件存储，repository 层收敛。

### F6.2 详细设计

- **Bus**：`Publish` = 顺序写内存 ring（序号）→ 异步批量落 event_log（100 条/500ms）→ 逐订阅者非阻塞投递（channel 满则丢弃 + 计数暴露 metrics）
- 订阅者慢消费不阻塞发布者（背压策略：丢弃而非阻塞，群聊时效优先）
- **repo**：接口按聚合划分（`AgentRepo/SessionRepo/MessageRepo/GroupRepo/TaskRepo/EventRepo`），SQLite 实现 + 测试用内存实现
- 迁移：goose 嵌入 `migrations/`，启动时自动 upgrade
- WAL 模式 + `busy_timeout=5s`；`messages` 索引 `(session_id, created_at)`；event_log 按 seq 顺序消费
- 备份：`VACUUM INTO` 定时（每日）导出快照到 `data/backups/`，保留 7 份

---

## F7 进程治理与健康（Runtime Governance）

### F7.1 目标

CLI 型 adapter 的子进程全生命周期管控——杜绝悬挂进程、僵尸进程、超时失控（本需求的直接动机之一）。

### F7.2 详细设计

- **spawn 规范**：`Setpgid` 独立进程组；stdin/stdout 管道；env 显式白名单继承
- **超时三层**：调用 ctx（请求级）→ node/task timeout（编排级）→ `default_child_timeout`（平台兜底，默认 1800s，0 = 不限但必须有编排级超时存在，启动时校验告警）
- **取消语义**：`context.Done` → `kill(-pgid, SIGTERM)` → 5s 宽限 → SIGKILL；进程退出码与输出尾部记入事件
- **资源上限**：单 agent 并发 1（默认）；平台总子进程上限 32，满则排队
- **收尸**：`cmd.Wait()` 必在 goroutine 中成对调用；启动时清理上次残留（按记录的 pgid 扫描）

---

## Feature 与里程碑对照

| Feature | M0 | M1 | M2 | M3 | M4 |
|---|---|---|---|---|---|
| F1 纳管注册 | ✔核心 | ✔两 agent | | | |
| F2 私聊 | | | ✔ | | |
| F3 群聊仲裁 | | | | ✔ | |
| F4 编排 | | | | | ✔ |
| F5 接入层 | ✔CLI 骨架 | | ✔Web UI | | |
| F6 总线存储 | ✔ | | | | |
| F7 进程治理 | ✔规范 | ✔落地 | | | |
