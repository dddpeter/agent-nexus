# TEST-REPORT — 已实现功能全量测试报告

> 日期：2026-11-06 · 测试人：zcode（静态门禁 + 100 例单测 + 35 项黑盒链路实测）
> 被测版本：`main@c857a52`（含本次测试中发现的 2 处缺陷修复）· 环境：本机 Go 1.26.8 / SQLite WAL

## 总体结论

| 层 | 结果 |
|---|---|
| 静态门禁（build/vet/gofmt） | ✅ 全过 |
| 自动化单测 | ✅ 12 个测试包 / **100 用例全绿 / 0 失败 0 跳过** |
| 黑盒链路（真实 serve 进程 + curl + CLI） | ✅ 35/35 通过（含 1 项限流语义确认注记） |
| **测试中发现并修复真实缺陷** | **2 个**（见 §缺陷） |

## 一、静态门禁与单测

- `go build ./...` / `go vet ./...` / `gofmt -l`（无输出）：PASS
- `go test ./... -count=1`：**100 PASS / 0 FAIL / 0 SKIP**，覆盖
  U-F1-01~08、C-F1-01~05、U-F2-01~08、U-F3-01~10、U-F6-01~04、U-F7-01~05、U-F5-01~04 等编号用例
  （逐例映射见 NIGHT-REPORT.md §②）

## 二、黑盒链路实测（实例 A 无 token :19171 / 实例 B 带 token :19172）

### 注册与配置管理（F1）

| # | 用例 | 结果 | 关键证据 |
|---|---|---|---|
| T01 | GET /healthz | ✅ | `{"ok":true}` |
| T02 | GET /api/adapters 三工厂 + ConfigSchema | ✅ | echod(7 字段, secret=api_key)/hermes/picoclaw |
| T03 | 注册 echod → 自动 online；api_key 脱敏 | ✅ | `status=online api_key=***` |
| T04 | hermes 死端口注册 → offline；测试端点明确报错 | ✅ | `connect: connection refused`（非泛化 500） |
| T05 | 重名注册 → 409 | ✅ | HTTP 409 |
| T06 | 非法枚举 → 400 含字段名 | ✅ | `config field "mode": must be one of echo|fixed|…` |
| T07 | MaxConcurrency=0 → 400 | ✅ | `invalid_capabilities` |
| T08 | 详情密钥脱敏回显 | ✅ | `api_key: ***` |
| T09 | 连通性测试端点（含试发消息） | ✅ | `{"ok":true,"duration_ms":0.012}` |
| T10 | 改坏配置被拦 + 旧配置生效 + 状态不变 | ✅ | 回读 `mode=echo status=online` |
| T11 | 密钥留空 PUT → 原值保留 | ✅ | 回读仍 `***`（未清空） |
| T12 | force 强制保存坏配置 → offline | ✅ | `status=offline` |
| T13 | 改回好配置 → 恢复 online | ✅ | `status=online` |
| T14 | 心跳实时刷新（2s 间隔） | ✅ | last_heartbeat_at 前后变化 |
| T21 | 停用 → 派消息 409 | ✅ | `agent_unavailable` |
| T22 | 启用 → 恢复派发 | ✅ | 回显正常 |
| T23 | 软删 → 派消息 404、历史会话仍可查 | ✅ | 204 / `not_found` / 会话 1 条保留 / `include_removed` 可见 `removed` |
| T24 | 列表过滤（默认排除 removed） | ✅ | 两列表对比 |

### 私聊会话（F2）

| # | 用例 | 结果 | 关键证据 |
|---|---|---|---|
| T15 | POST /api/sessions | ✅ | `kind=private` |
| T16 | 同步发送 → 回显 + reply_to | ✅ | `sender=agent content=第一条消息 reply_to=1` |
| T17 | 续话：external_ref 回填且稳定 | ✅ | 两轮发送后 `external_ref` 非空且再读不变 |
| T18 | 历史消息 | ✅ | 4 条 `[human,agent,human,agent]` |
| T19 | 流式发送 SSE | ✅ | delta×3 + final×1 |
| T20 | 异步：202 + accepted 回执 + system 回填 | ✅ | `task_id=wfb-… eta=50ms`；回填 `('system','长任务完成：报告已生成')` |

### 事件总线与容灾（F6/F5）

| # | 用例 | 结果 | 关键证据 |
|---|---|---|---|
| T25 | SSE 全局流实时接收 | ✅ | 单次订阅收 26 事件，含本条 msg.human+msg.agent |
| T26 | 断线重连 Last-Event-ID 重放 | ✅ | 断线位点 26 → 重放恰为 27~32，0 重复 0 丢失 |
| T27 | kill -9 → 重启 | ✅ | 3 个 agent 重新探活（hermes-dead 正确判 offline、软删者不恢复）；消息 14→14；event_log 32 条全量可重放；重启后同会话续话成功 |

### 接入层（F5）

| # | 用例 | 结果 | 关键证据 |
|---|---|---|---|
| T28 | Agent 列表页 | ✅ | 表格/状态徽标/操作按钮渲染 |
| T29 | 注册向导 | ✅ | echod 表单含密钥 password 输入；切 hermes 表单字段正确；连通性测试通过/失败两种反馈；提交注册成功 |
| T30 | 聊天页端到端 | ✅ | 建会话 303 → 面板 + EventSource + 发送表单；表单发送渲染 human+agent 双气泡 |
| T31 | 静态资源 | ✅ | /assets/htmx.min.js 200 |
| T32 | 鉴权 | ✅ | 无/错 token 401；对 token 200/201；错误结构含 code+request_id |
| T33 | CLI register/list | ✅ | `registered: id=… status=online`；列表五 agent 状态正确 |
| T34 | CLI 错误透传 | ✅ | `Error: HTTP 401 [unauthorized]: missing or invalid bearer token` |
| T35 | 限流 60/min + 窗口恢复 | ✅* | 超限即 429（`rate_limited` + request_id）；61s 后恢复 200。*注记：被拒请求同样计入窗口（严格 ≤60 处理/分钟），干净窗口下第 61 次 429 由单测 U-F5-02 精确覆盖 |

## 三、测试中发现并修复的真实缺陷（P0 级 2 个）

| 缺陷 | 现象 | 根因 | 修复 |
|---|---|---|---|
| D1 serve 未装配 hermes/picoclaw 工厂 | T04 注册 hermes 返回 `unknown adapter`（API 注册完全不可用） | 任务 7 提交时预留“随任务 8/9 加入”，实际遗漏 | `c857a52` 前序提交：serve.go 工厂列表补齐，重启复测通过 |
| D2 CLI 源码未入版本库 | `cmd/nexus/` 四个文件从未被 git 追踪，克隆仓库无法构建 | .gitignore 第 12 行 `nexus`（意图忽略编译产物）误伤同名目录 | 改为 `/nexus` 仅匹配根路径二进制；补交 4 个源文件；**干净克隆全新构建 + 冒烟测试通过** |

## 四、未覆盖项（与 NIGHT-REPORT 一致）

- 同步超时占位（serve 固定 300s，黑盒不可等）——单测 U-F2-02 已覆盖
- SSE 15s 心跳注释行——需长连接观察，未实测（实现于 sse.go）
- e2e 真实 hermes/picoclaw、群聊/任务编排、性能基线——M1 验收日/后续里程碑
- U-F6-05/06、U-F5-05 CLI REPL、24h 会话回收——见 TODO.md

## 五、复测方式

```bash
make check          # fmt+vet+test
./bin/nexus serve --config <cfg>   # 黑盒链路按本报告 §二 编号用 curl 复走
```
