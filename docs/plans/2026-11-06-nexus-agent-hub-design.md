# Nexus — 异构 Agent 纳管与编排平台 设计文档

> 状态：§1–§4 已确认，§5 待出
> 日期：2026-11-06
> 需求对齐结论：C（指挥官+总机，agent 可互聊）/ C（本机起步，接口预留分布式）/ B（人机混合群）/ C（全新自研 + Temporal）/ A（全 Go）

## §1 总体架构（已确认）

```
┌─────────────────────────────────────────────┐
│  接入层   Web UI / CLI   （二期再加飞书等IM） │
├─────────────────────────────────────────────┤
│  会话层   群聊协调器 / 私聊路由 / 发言仲裁     │
├─────────────────────────────────────────────┤
│  编排层   Temporal Workflows                 │
│          （任务DAG、定时任务、超时重试）       │
├─────────────────────────────────────────────┤
│  适配层   AgentAdapter 接口                  │
│          ├ picoclaw ├ hermes ├ openclaw      │
│          ├ opencode ├ pi/zcode/DSH ...       │
├─────────────────────────────────────────────┤
│  注册层   Agent Registry（心跳/能力/健康）    │
├─────────────────────────────────────────────┤
│  存储层   SQLite(WAL) → Postgres 可切换       │
└─────────────────────────────────────────────┘
```

- 唯一核心抽象：`AgentAdapter` 接口。新接 agent = 一个 adapter 文件，平台零改动。
- 所有消息走统一内部事件总线（Go channel + 追加写事件日志）。
- 分布式预留：Adapter 接口不假设 agent 在本机，一期本地进程驱动，二期加 A2A HTTP 实现即远程 agent。

## §2 适配层（已确认）

```go
type AgentAdapter interface {
    Info() AgentInfo
    CreateSession(ctx, SessionOpts) (SessionID, error)
    Send(ctx, SessionID, Message) (Reply, error)
    Stream(ctx, SessionID, Message) (<-chan Event, func(), error)
    Close(ctx, SessionID) error
}
```

接入方式：

| Agent | 驱动方式 |
|---|---|
| PicoClaw | CLI headless 或直连 gateway HTTP API |
| Hermes | Gateway HTTP API（default/coder/research 三 profile） |
| OpenClaw | CLI headless / gateway |
| OpenCode | CLI `opencode run` 非交互，`--session` 续话 |
| pi / zcode / DSH | 有 CLI headless 或 HTTP API 才接 |

- 接入门槛：必须有非交互模式或 HTTP API。
- 能力协商：`CanGroupChat / CanStream / MaxConcurrency / TypicalLatency`。
- 健康检查：CLI 型探活命令，HTTP 型心跳。
- 超时取消：adapter 统一管子进程生命周期，context 取消 kill 进程组。
- 新接入成本：~200-300 行 adapter + 配置，半天内。

## §3 群聊协调与发言仲裁（已确认）

- 群成员带配置：触发方式（@/关键词/任意）、发言预算、优先级。
- 仲裁器（Arbiter）规则：
  1. 人 @ 某 agent → 只有它能接话，其余候选静默丢弃。
  2. 人无 @ → 按优先级×相关性选一个 agent 接话。
  3. 回合制：本回合已有人接话，其余闭嘴，候选存档。
  4. 发言预算：`max_turns_per_topic`（默认 2），超预算降权。
  5. 防死循环：agent 回复不得直接触发另一 agent；人 N 轮（默认 4）未插话强制冻结并 @ 人附摘要。
  6. 插话通道：agent 按 `interrupt_level` 权限申请插话，否则转站内信。
- 话题隔离：`topic_state` 分段，预算与上下文按话题重置。
- 私聊：无仲裁，长任务可异步回报（Temporal workflow 到点回填）。
- Agent↔Agent 协作链走编排层 DAG，不走群聊仲裁。群聊=即时讨论，编排=受控流水线。

## §4 存储与任务调度（已确认）

- SQLite（WAL）起步，SQL 抽象层便于切 Postgres；不引入 Redis。
- 核心表：`agents / sessions / messages / event_log / groups / tasks / workflows`。
- `messages` 只做存档检索，实时流转走内存总线，落库异步。
- 分界线：**「等结果」的事走 Temporal，「传话」的事走总线。**
  - 进 Temporal：任务 DAG、异步回报、定时任务、超时重试（activity 粒度）。
  - 不进：群聊消息路由、仲裁决策（内存实时）。
- 派发前检查 agent `status` 与 `MaxConcurrency`，忙碌排队。

## §5 技术栈汇总、里程碑与风险清单（已确认）

### 技术栈

| 层 | 选型 | 理由 |
|---|---|---|
| 语言 | Go 1.24+ | 生态一致、Temporal 一等 SDK、进程管理顺手 |
| Web 框架 | chi + htmx/Alpine 轻前端 | 单二进制、少依赖；接口先行，UI 复杂再上 Vue |
| 编排 | Temporal（docker compose local dev） | 超时/重试/DAG 开箱即用 |
| 存储 | SQLite WAL（modernc.org/sqlite 纯 Go 免 CGO） | 单二进制 |
| 实时推送 | SSE | 简单够用 |
| 配置 | YAML + 环境变量 | 与 hermes/picoclaw 习惯一致 |
| 部署 | 单二进制 + docker-compose（含 Temporal） | 一条命令起全家 |

### 里程碑（约 9-10 周全职，业余 ×2.5）

- M0（1 周）：骨架——事件总线 + SQLite + AgentAdapter 接口 + Registry；echo adapter 收发回显。
- M1（2 周）：接 Hermes（HTTP 型）+ PicoClaw（CLI 型），覆盖两种驱动范式；CLI 派任务拿回真实回复。
- M2（2 周）：私聊 + Web UI——路由、续话、SSE 实时回复、任务列表；浏览器双向聊天。
- M3（2-3 周）：群聊 + 仲裁——@路由、发言预算、防死循环；人+2 agent 讨论不失控。
- M4（2 周）：编排——协作链 DAG、定时任务、异步回报、补接 OpenCode/OpenClaw；流水线+cron 日报跑通。

### 风险清单

1. （高）CLI 型 agent 输出不可控 → 优先 HTTP API；CLI 加 `--output json` 或 wrapper。
2. （中）仲裁体验两难 → 阈值全部可热调，M3 小范围试用先行。
3. （中）Temporal 运维偏重 → 编排层抽象，留「内置轻量调度器」降级实现。
4. （中）群聊与任务流水线上下文割裂 → session 关联 topic_id，派任务附带群聊摘要。
5. （高）范围膨胀 → 铁律：M4 前不加新特性；IM 接入与远程 agent 只进 roadmap。

