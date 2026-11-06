# Nexus 架构设计（Architecture）

> 版本：v1.0 · 日期：2026-11-06 · 状态：已评审定稿
> 关联文档：[技术设计](technical-design.md) · [设计评审记录](plans/2026-11-06-nexus-agent-hub-design.md)

## 1. 系统定位

Nexus 是一个**异构 AI Agent 纳管与编排平台**：将 OpenClaw、Hermes、PicoClaw、OpenCode、pi agent、zcode、DSH 等具备 CLI 非交互模式或 HTTP API 的 agent 统一注册纳管，提供：

- **指挥官能力**：任务派发、协作链编排、定时调度、超时重试
- **总机能力**：私聊/群聊消息路由，人机混合群中 agent 受控发言
- **协作能力**：agent ↔ agent 互聊（群聊即时讨论）与流水线协作（受控 DAG）双通道

### 需求基线（已确认）

| 决策点 | 结论 |
|---|---|
| 平台角色 | 指挥官 + 总机，agent 可互聊 |
| 部署形态 | 本机起步，接口预留分布式（A2A HTTP 可扩展） |
| 群聊形态 | 人机混合群，agent 之间能互聊 |
| 建设方式 | 全新自研 + Temporal 编排引擎 |
| 技术栈 | 全 Go |
| 接入渠道 | 一期 Web UI + CLI；IM 通道二期 |

### 非目标（Non-Goals，一期）

- 不做 IM（飞书/Telegram）通道接入
- 不做远程 agent / 跨机器纳管
- 不做 agent 自身的模型配置管理（各家自管）
- 不做多租户

## 2. 总体架构

```
┌─────────────────────────────────────────────────────┐
│  接入层    Web UI (chi + SSE)   CLI (nexus 命令)      │
├─────────────────────────────────────────────────────┤
│  API 层    REST API · 鉴权 · 请求校验                │
├─────────────────────────────────────────────────────┤
│  会话层    群聊协调器 ─ 发言仲裁器(Arbiter)            │
│            私聊路由器 · 话题状态机                    │
├─────────────────────────────────────────────────────┤
│  编排层    Temporal Workflows（任务DAG/定时/重试）     │
│            └ 降级实现：内置轻量调度器（无Temporal场景） │
├─────────────────────────────────────────────────────┤
│  适配层    AgentAdapter 接口                          │
│            ├ hermes(HTTP) ├ picoclaw(CLI)            │
│            ├ openclaw      ├ opencode                │
│            └ pi/zcode/DSH …（有接口才接）             │
├─────────────────────────────────────────────────────┤
│  注册层    Agent Registry（能力声明/心跳/健康/并发闸门）│
├─────────────────────────────────────────────────────┤
│  基础层    事件总线(In-Mem+EventLog) · 存储(SQLite)   │
└─────────────────────────────────────────────────────┘
```

### 2.1 分层职责与依赖规则

- **依赖单向向下**：上层只能调用下层公开接口；下层不得感知上层。
- **横向解耦**：会话层与编排层互不依赖，通过事件总线协作（传话走总线，等结果走 Temporal）。
- **适配层是唯一变异点**：新增 agent 只增加 adapter 实现，其余层零改动。

## 3. 核心抽象

### 3.1 AgentAdapter（系统的心脏）

```go
type AgentAdapter interface {
    Info() AgentInfo
    CreateSession(ctx context.Context, opts SessionOpts) (SessionID, error)
    Send(ctx context.Context, sid SessionID, msg Message) (Reply, error)
    Stream(ctx context.Context, sid SessionID, msg Message) (<-chan Event, func(), error)
    Close(ctx context.Context, sid SessionID) error
}
```

- **不假设 agent 在本机**：一期为本地进程驱动实现；二期新增 A2A HTTP 实现即获得远程 agent 能力，上层无感知。
- **能力协商**：注册时声明 `CanGroupChat / CanStream / MaxConcurrency / TypicalLatency / InterruptLevel`。群聊协调器只拉 `CanStream` 的 agent 进群；`MaxConcurrency` 由注册层做闸门。

### 3.2 事件总线

- 内存 Go channel 扇出 + 追加写 `event_log` 持久化（异步落库，不阻塞消息路径）。
- 订阅者：群聊仲裁器、会话路由、Web SSE 推送、存档器。
- 事件类型：`msg.human / msg.agent / msg.system / agent.status / task.lifecycle / arbiter.decision`。

## 4. 关键机制设计

### 4.1 双通道协作模型（本系统的根本分界）

| | 群聊通道（会话层） | 编排通道（Temporal） |
|---|---|---|
| 性质 | 即时讨论，仲裁调控 | 受控流水线，确定性执行 |
| 触发 | 人发言/@ | 任务提交/cron |
| agent↔agent | 受仲裁约束（回复不得直接触发他者） | 显式 DAG 声明（A 输出→B 输入） |
| 持久化 | 消息存档（检索/回放） | workflow 历史 + tasks 表 |
| 时效 | 毫秒级 | 秒~分钟级 |

两通道通过 `topic_id` 关联会话上下文：派任务时可附带群聊上下文摘要，消除视角割裂。

### 4.2 发言仲裁器（Arbiter）

所有 agent 发言必须经仲裁批准才发出：

1. 人 @ 某 agent → 仅其可接话，其余候选静默丢弃
2. 人未 @ → 按优先级×相关性选一个接话
3. 回合制：本回合已有人接话，其余闭嘴（候选存档供参考）
4. 发言预算：`max_turns_per_topic`（默认 2），超预算降权
5. 防死循环：agent 回复**不得直接触发**另一 agent；人 N 轮（默认 4）未插话 → 冻结群聊并 @ 人附摘要
6. 插话通道：agent 按 `InterruptLevel` 申请，否则转站内信给人

设计取向：**第一版宁可不说话，不可抢话**；全部阈值热可调。

### 4.3 生命周期与故障处理

- Agent 健康：CLI 型探活命令 / HTTP 型心跳，连续 N 次失败标记 offline；离线 agent 不接新任务，进行中任务由编排层按重试策略处理。
- 进程治理：adapter 统一管理子进程（进程组、`context` 取消即 kill、平台级 `child_timeout` 兜底）——规避自管进程的超时坑。
- 优雅停机：先停消息入口 → 等仲裁队列清空 → 通知 Temporal 取消未完成 activity → 落盘状态。

## 5. 存储架构

SQLite（WAL 模式，`modernc.org/sqlite` 纯 Go 免 CGO）起步，SQL 访问收敛在 repository 层，切 Postgres 只改连接与迁移脚本。

核心实体：

```
agents    注册与能力声明、状态、心跳
sessions  会话（private|group|task），外部引用
messages  全量消息存档（审计/回放/上下文重建），异步写入
event_log 追加写事件总线持久层
groups    群配置、成员、仲裁参数
tasks     任务实例（状态/结果/重试）
workflows Temporal workflow 元数据映射
```

分界：`messages` 只服务检索，实时流转全在内存总线。

## 6. 演进路线

- **一期（M0–M4）**：本机纳管、私聊/群聊、Temporal 编排、Web UI + CLI
- **二期**：IM 通道（飞书 Bot 等）、A2A 协议远程 agent、Agent Sidecar 跨机注册
- **架构预留**：Adapter 网络化（3.1）、事件总线可外移（NATS）、存储可换 Postgres

## 7. 架构风险与对策

| 风险 | 等级 | 对策 |
|---|---|---|
| CLI 型 agent 输出格式不可控 | 高 | 优先 HTTP API 接入；CLI 型要求 json 输出或 wrapper |
| 仲裁体验两难（太死/打架） | 中 | 阈值全部热可调，M3 小范围试用先行 |
| Temporal 对个人工具偏重 | 中 | 编排层接口抽象，内置轻量调度器降级实现 |
| 群聊与流水线上下文割裂 | 中 | topic_id 关联 + 派任务附摘要 |
| 范围膨胀 | 高 | 铁律：M4 前不加新特性 |
