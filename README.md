# Nexus

异构 AI Agent 纳管与编排平台 —— 把 OpenClaw、Hermes、PicoClaw、OpenCode、pi agent 等各类有接口的 agent 统一注册、编排、调度，并支持人机混合群聊与私聊。

## 特性（规划）

- **异构纳管**：统一 `AgentAdapter` 接口，CLI headless 与 HTTP API 两种驱动范式，新接一个 agent ≈ 200-300 行 adapter
- **任务编排**：基于 Temporal 的任务 DAG、协作链（A→B→汇总）、定时任务、超时重试
- **群聊协调**：人机混合群、@路由、发言预算、防死循环仲裁器
- **私聊路由**：会话续话、长任务异步回报
- **单二进制部署**：Go + SQLite(WAL)，一条 docker-compose 起全家

## 快速开始

```bash
git clone git@github.com:dddpeter/agent-nexus.git
cd agent-nexus
# M0 之后补充：make build / docker-compose up
```

## 文档

- [设计文档](docs/plans/2026-11-06-nexus-agent-hub-design.md) —— 架构、适配层、群聊仲裁、存储与调度、里程碑与风险（§1–§5）

## 里程碑

| 阶段 | 内容 | 状态 |
|---|---|---|
| M0 | 骨架：事件总线 + SQLite + Adapter 接口 + Registry | 🚧 未开始 |
| M1 | 接入 Hermes（HTTP）+ PicoClaw（CLI） | 🚧 未开始 |
| M2 | 私聊 + Web UI（SSE 实时回复） | 🚧 未开始 |
| M3 | 群聊 + 发言仲裁 | 🚧 未开始 |
| M4 | 编排 DAG + 定时任务 + 补接 OpenCode/OpenClaw | 🚧 未开始 |

## 路线图（M4 之后）

- 飞书 / Telegram 等 IM 通道接入
- A2A 协议支持（远程 agent，分布式部署）
- Agent Sidecar（跨机器注册）

## 许可证

[MIT](LICENSE)
