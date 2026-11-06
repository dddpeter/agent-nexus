# Contributing to Nexus

感谢关注 Nexus！目前项目处于设计定稿、M0 启动前的阶段。

## 开发流程

1. Fork 或建分支：`git checkout -b feat/xxx`
2. 提交遵循 [Conventional Commits](https://www.conventionalcommits.org/zh-hans/)：`feat:` / `fix:` / `docs:` / `refactor:` / `test:`
3. PR 前确保 `go build ./...` 和 `go test ./...` 通过

## 代码规范

- Go 1.24+，`gofmt` / `go vet` 必须干净
- 核心抽象是 `AgentAdapter` 接口（见设计文档 §2），新增 agent 支持请实现该接口，不要绕过
- 仲裁器（§3）阈值参数全部可配置，禁止硬编码

## 新增 Agent 适配指南（M1 后有效）

接入门槛：目标 agent 必须具备 **CLI 非交互模式** 或 **HTTP API**（二选一）。

1. 在 `internal/adapter/<name>/` 实现 `AgentAdapter` 接口
2. 在配置中声明能力：`CanGroupChat / CanStream / MaxConcurrency / TypicalLatency`
3. 提供探活方式（CLI 探活命令或 HTTP 心跳）
4. 补充集成测试（用 fake 模式，不依赖真实 agent 进程）

## 讨论与问题

- Issue 优先，标注类型：`bug` / `feature` / `question`
- 设计变更请先在 Issue 讨论，更新设计文档后再动代码
