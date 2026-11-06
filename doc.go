// Package nexus 是异构 AI Agent 纳管与编排平台的核心模块集合。
//
// 分层（详见 docs/architecture.md）：
//
//	接入层 internal/api + cmd/nexus（CLI）
//	会话层 internal/session（私聊路由 / 群聊协调）
//	编排层 internal/orchestr（Scheduler 接口 + builtin 降级实现）
//	适配层 internal/adapter（AgentAdapter 接口 + 各 agent 实现）
//	注册层 internal/registry（能力声明 / 心跳 / 并发闸门）
//	基础层 internal/bus + internal/repo（事件总线 + SQLite 存储）
//
// 依赖规则：上层只准调用下层公开接口，下层不得感知上层。
package nexus
