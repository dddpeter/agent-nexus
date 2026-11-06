# Nexus 测试验证计划（Test & Verification Plan）

> 版本：v1.0 · 日期：2026-11-06 · 状态：定稿
> 关联文档：[详细设计](detailed-design.md)（F1–F7）
> 分层策略：单测 → 组件测试（fake agent）→ 系统验证（真实 agent）→ 专项验证（仲裁回归/故障注入/性能）

## 1. 测试环境与工具

| 项 | 说明 |
|---|---|
| 单测 | `go test` + `testify`；仓库层用内存实现，不碰真 SQLite；SQLite 路径用临时文件 |
| fake agent | `echod`：内置测试用假 agent，可脚本化——固定回复 / 回显 / 延迟 N 秒 / 输出畸形 JSON / 中途退出 / 伪流式输出。CLI 与 HTTP 两种形态各一个 |
| 真实 agent | Hermes、PicoClaw 本机可用，打 `//go:build e2e` tag，CI 默认跳过 |
| CI | GitHub Actions：`gofmt → go vet → golangci-lint → go test ./... → go test -tags=e2e ./...`（e2e 仅夜间定时跑） |
| 覆盖率 | 核心包（arbiter/bus/registry/session/orchestr）行覆盖 ≥ 80%，adapter ≥ 70%，整体门禁 75% |

## 2. 分 Feature 测试设计

### F1 纳管注册

| 用例 | 类型 | 验证点 |
|---|---|---|
| U-F1-01 | 单测 | 注册合法 spec → agent 入库，status=online（`echod` 探活通过） |
| U-F1-02 | 单测 | adapter 配置缺字段 → 注册拒绝，错误信息含字段名 |
| U-F1-03 | 单测 | 重名注册 → 409 |
| U-F1-04 | 单测 | 心跳连续 3 次失败 → offline + `agent.status` 事件（from=online,to=offline） |
| U-F1-05 | 单测 | offline 后恢复探活成功 → 自动 online |
| U-F1-06 | 单测 | 并发闸门：MaxConcurrency=1，两次 Acquire 并发 → 第二个阻塞；release 后放行 |
| U-F1-07 | 单测 | MaxConcurrency=0 → 注册拒绝 |
| U-F1-08 | 单测 | 配置含 `${API_KEY}` → 载入解析正确；日志输出脱敏 |
| C-F1-01 | 组件 | API 注册 echod-agent → list 可见 → 派消息成功 → 注销 → 派消息 404 |

### F2 私聊会话

| 用例 | 类型 | 验证点 |
|---|---|---|
| U-F2-01 | 单测 | 同步发消息 → `echod` 回显，messages 落库（sender_type=agent） |
| U-F2-02 | 单测 | 同步超时（agent 延迟 > timeout）→ 超时占位回复 + session 保留 |
| U-F2-03 | 单测 | 流式：伪流式 echod → SSE 收到 ≥2 个 delta + 1 个 final |
| U-F2-04 | 单测 | 流式 60s 无 delta → error 事件 |
| U-F2-05 | 单测 | 续话：第二条消息携带同 session → agent 收到相同 external_ref（echod 记录断言） |
| U-F2-06 | 组件 | 无会话机制 agent：第 11 条消息时平台注入的上下文包含第 1 轮内容 |
| U-F2-07 | 组件 | 上下文超 20 轮 → 摘要触发，新请求含摘要标记 |
| U-F2-08 | 组件 | 异步回报：async 模式 → 立即收到 accepted 回执 → 任务完成后 session 收到 system 回填消息 |
| C-F2-01 | e2e | 与真实 PicoClaw 私聊两轮，第二轮验证上下文延续 |
| C-F2-02 | e2e | 与真实 Hermes 私聊，流式输出 |

### F3 群聊仲裁（重点：表驱动回归）

**仲裁器为纯函数，全部场景表驱动单测，是整个项目的回归安全网。**

| 用例 | 类型 | 验证点 |
|---|---|---|
| U-F3-01 | 表驱动 | 人 @A → 仅 A 获准；B、C 的候选被 Deferred |
| U-F3-02 | 表驱动 | 人未 @ → 相关性×priority 加权最高者接话（纯 priority 不当选）；并列时最近未发言者优先 |
| U-F3-03 | 表驱动 | 回合占用：A 接话未结束，B 候选 → Deferred |
| U-F3-04 | 表驱动 | 预算：A 本话题已发言 2 次（默认预算）→ 再次候选被 Deferred + 降权 |
| U-F3-05 | 表驱动 | 防连环：上一条为 agent 发言且本轮无人插话 → 其他 agent 候选全部拒绝 |
| U-F3-06 | 表驱动 | 静默 4 轮 → Freeze Action + 摘要 Action + NotifyOwner |
| U-F3-07 | 表驱动 | 话题重置（显式 /topic）→ 预算计数归零 |
| U-F3-08 | 表驱动 | 插话：InterruptLevel=2 直接放行；=1 且无回合占用放行，有占用转站内信；=0 恒转站内信 |
| U-F3-09 | 表驱动 | 冻结状态：仅主持人发言解冻；agent 候选一律 Deferred |
| U-F3-10 | 表驱动 | 空内容发言 → 不占预算 |
| C-F3-01 | 组件 | 人 + 2×echod 三方"讨论"脚本化会话（echod 固定接话模式）→ 全程无连环触发、无超预算发言，arbiter.decision 事件完整可回放 |
| C-F3-02 | 组件 | 热更新：API 修改 max_turns_per_topic=5 → 新决策立即生效 |
| C-F3-03 | e2e | 人 + 真实 Hermes + 真实 PicoClaw 群聊一个话题 10 轮 → 无失控，冻结机制按预期触发 |

### F4 任务编排

| 用例 | 类型 | 验证点 |
|---|---|---|
| U-F4-01 | 组件 | 三节点链 A→B→C（echod）→ C 的输入含 B 输出（{{prev}} 渲染正确） |
| U-F4-02 | 组件 | fan-out 2 分支 + 汇聚 → 两侧均完成才标记 done |
| U-F4-03 | 组件 | 节点失败重试：echod 故障模式（前 2 次 500 第 3 次成功）→ retries=3 时整链 done |
| U-F4-04 | 组件 | 重试耗尽 → failed，已完成节点结果保留；resume 从失败节点继续 |
| U-F4-05 | 组件 | 节点超时 → 按超时失败，子进程被 kill（F7 联动验证无残留） |
| U-F4-06 | 组件 | {{prev}} 输出 >64KB → 注入摘要而非全文，原始输出在存档 |
| U-F4-07 | 组件 | cron 任务（1min 周期 + agent offline）→ 跳过记录，恢复后正常触发 |
| U-F4-08 | 组件 | 运行中 cancel → 状态 cancelled，子进程无残留 |
| U-F4-09 | 组件 | engine=builtin 降级：U-F4-01/03/04 同套用例跑通 |
| C-F4-01 | e2e | 真实链：PicoClaw 写代码 → Hermes 评审 → PicoClaw 修订，全程结果落库 |

### F5 接入层

| 用例 | 类型 | 验证点 |
|---|---|---|
| U-F5-01 | 单测 | 无/错 token → 401；正确 token 放行 |
| U-F5-02 | 单测 | 限流：61 req/min → 第 61 次 429 |
| U-F5-03 | 组件 | SSE 断线重连带 Last-Event-ID → 补偿重放不丢消息、不重复 |
| U-F5-04 | 组件 | 错误结构统一 `{"error":{code,message,request_id}}` |
| U-F5-05 | 组件 | CLI REPL：发消息 → 收回复；Ctrl-C → 当前请求取消，进程干净退出 |
| C-F5-01 | 手工 | Web UI 全页面走查：群聊页可见 Deferred 消息及原因；任务页 DAG 进度实时刷新；阈值修改即时生效 |

### F6 事件总线与存储

| 用例 | 类型 | 验证点 |
|---|---|---|
| U-F6-01 | 单测 | 发布 1000 事件 → event_log 全量落库（最终一致，500ms 内） |
| U-F6-02 | 单测 | 慢订阅者（channel 满）→ 发布不阻塞，丢弃计数 +1 |
| U-F6-03 | 单测 | 三个订阅者各自独立收到全量事件 |
| U-F6-04 | 单测 | messages 索引查询：万条消息按 session 查最近 20 条 < 10ms |
| U-F6-05 | 组件 | 进程重启 → 群状态快照恢复，agent 状态重新探活初始化 |
| U-F6-06 | 组件 | 定时备份生成且可恢复（VACUUM INTO 快照还原后数据一致） |

### F7 进程治理

| 用例 | 类型 | 验证点 |
|---|---|---|
| U-F7-01 | 组件 | echod-cli 挂起模式（不退出）→ timeout 触发 → 进程组被 kill（ps 断言无残留） |
| U-F7-02 | 组件 | SIGTERM 后 5s 未退 → SIGKILL 补刀 |
| U-F7-03 | 组件 | 平台总子进程达上限 32 → 新任务排队 |
| U-F7-04 | 组件 | default_child_timeout=0 且节点无超时 → 启动告警 |
| U-F7-05 | 组件 | kill 后输出尾部与退出码记录在 error 事件中 |

## 3. 专项验证

### 3.1 故障注入矩阵（组件级，自动化）

| 注入 | 预期 |
|---|---|
| agent 进程中途被外部 kill -9 | adapter 报 error；registry 短期内标记 offline；进行中任务进重试 |
| HTTP agent 网关 500/超时/慢响应 | 分别对应：可重试错误 / 超时重试 / 触发调用超时而非无限等 |
| SQLite 文件只读 | 启动失败并给明确报错（而非静默降级） |
| event_log 落库 goroutine panic | 不影响总线内存投递；panic 恢复 + 告警日志 |
| 仲裁器输入含超长消息（1MB） | 输入截断，决策正常 |
| 并发 50 会话同时活跃 | 无死锁、无 goroutine 泄漏（pprof goroutine 数回归对比） |

### 3.2 性能基线（验收参考值，M4 前达成）

| 指标 | 目标 |
|---|---|
| 群聊人发言 → agent 获准并发出首字（HTTP 型 agent） | P95 < 2s（不含 agent 自身耗时） |
| 仲裁决策延迟 | P99 < 50ms |
| SSE 端到端（bus 事件 → 浏览器） | P95 < 200ms |
| 持续负载 | 10 会话 × 1 msg/s 跑 1h：内存增长 < 200MB，无 goroutine 泄漏 |

### 3.3 安全验证

- [ ] 日志全量扫描无 api_key 明文（脱敏用例 U-F1-08 扩展为全局 grep 检查）
- [ ] 所有 adapter 命令执行仅来自配置文件，消息内容无法注入命令（构造含 `; rm` 的消息验证）
- [ ] `.env` / `*.local.yaml` 确认不在 git 追踪中
- [ ] 数据目录权限 0600 校验

## 4. 验收标准（里程碑级）

| 里程碑 | 验收内容 |
|---|---|
| M0 | U-F1-01~08、U-F6-01~04、F7 spawn 规范用例全绿；`echod` CLI 注册收发回显演示 |
| M1 | C-F1-01、真实 PicoClaw/Hermes 各 1 条派任务冒烟；故障注入矩阵前 3 行通过 |
| M2 | F2 全部用例 + C-F2-01/02、F5 用例（除手工走查）；浏览器双向聊天演示 |
| M3 | F3 全部表驱动用例 + C-F3-01/02/03；仲裁回归纳入 CI 必跑 |
| M4 | F4 全部用例 + C-F4-01；性能基线达标；安全验证清单全勾 |

## 5. 缺陷管理约定

- 阻断仲裁安全（连环触发、冻结失效）的 bug 一律 P0，修复必须补表驱动用例
- adapter 解析类 bug 修复时必须在 echod 中加对应故障模式，防止回归
- 每个 P0/P1 修复 PR 需附复现步骤与验证证据（命令输出）
