# NIGHT-REPORT — agent-nexus 过夜实施报告

> 日期：2026-11-06 → 2026-11-07 · 执行：zcode（全栈实现）
> 起点 commit `3149231`（docs only）→ 终点见 `git log`；共 18 个提交，全部 Conventional Commits 小步提交，未 force push。

## ① 编号任务完成状态表

| # | 任务 | 状态 | 关键提交 |
|---|---|---|---|
| 1 | 项目骨架（go.mod/目录/Makefile/.gitignore） | ✅ | `4496d4f` |
| 2 | F6 事件总线（扇出/背压丢弃/批量落库） | ✅ | `e241775` |
| 3 | F6 存储层（goose 迁移+DDL+SQLite/内存 repo） | ✅ | `2a69fbe` |
| 4 | F1 registry（注册/状态机/心跳/闸门+AdapterFactory 核心+进程内 echod） | ✅ | `8ddb905` |
| 5 | F7 进程治理 exec 包（Setpgid/三层超时/TERM→5s→KILL/收尸） | ✅ | `9d5d6aa` |
| 6 | echod fake agent（CLI 二进制 + HTTP gateway + 故障注入） | ✅ | `beb10a2` `5620ce8` |
| 7 | CLI 骨架（cobra：serve / agent list / agent register） | ✅ | `7e9b33c` |
| 8 | hermes adapter（HTTP 范式） | ✅ | `2c00064` |
| 9 | picoclaw adapter（CLI 范式） | ✅ | `c18ff27` |
| 10 | F1 页面注册闭环后端（adapters/注册/编辑/测试/停用/软删/脱敏/回滚） | ✅ | `d4fa1a9` |
| 11 | htmx 最简页面（agent 列表 + 注册向导，无其他页面） | ✅ | `6a960b2` |
| 12 | F2 私聊会话 Router（双模式/续话/上下文窗口/异步回执/超时策略） | ✅ | `87ee903` `33f644b` |
| 13 | SSE /api/stream + Last-Event-ID 补偿重放 | ✅ | `c437921` |
| 14 | 私聊 API（POST /api/sessions、messages sync/async/stream）+ orchestr builtin | ✅ | `9c9c358` |
| 15 | F3 仲裁器纯函数核心（Evaluate/群状态/全部阈值） | ✅ | `29ad95c` |
| 16 | U-F3-01~10 全部表驱动用例 | ✅ | `29ad95c` `c811759` |

**16/16 完成**，含第 15-16 项（时间充裕档）。

## ② test-plan 用例覆盖对照表

| 用例 | 结果 | 实现位置 / 说明 |
|---|---|---|
| U-F1-01 注册→online（echod 探活） | ✅ PASS | `internal/registry` TestRegisterValidSpec_U_F1_01 |
| U-F1-02 缺字段→含字段名拒绝 | ✅ PASS | TestRegisterMissingField_U_F1_02（FieldError） |
| U-F1-03 重名→409 | ✅ PASS | TestRegisterDuplicateName_U_F1_03 |
| U-F1-04 心跳 3 败→offline+事件(from/to) | ✅ PASS | TestHeartbeatOffline_U_F1_04（可翻转健康工厂注入） |
| U-F1-05 恢复→自动 online | ✅ PASS | TestHeartbeatRecover_U_F1_05 |
| U-F1-06 闸门阻塞/放行/busy 派生 | ✅ PASS | TestConcurrencyGate_U_F1_06 |
| U-F1-07 MaxConcurrency=0 拒绝 | ✅ PASS | TestMaxConcurrencyZeroRejected_U_F1_07 |
| U-F1-08 ${API_KEY} 解析+日志脱敏 | ✅ PASS | TestSecretEnvResolveAndMask_U_F1_08 |
| C-F1-01 注册→list→派消息→注销→404 | ✅ PASS | `internal/api` TestAPIAgentLifecycle_C_F1_01 |
| C-F1-02 向导：adapters schema→注册→online | ✅ PASS | TestAdaptersSchemaEndpoint + TestUIPagesSmoke + TestRegisterListUnregister |
| C-F1-03 坏 endpoint 明确错误；改坏被拦+旧配置生效 | ✅ PASS | TestConnectivityTestAndRollback_C_F1_03（hermes 指向 127.0.0.1:1，见 TODO#3） |
| C-F1-04 api_key 脱敏回显 / 留空保留 | ✅ PASS | TestSecretMasking_C_F1_04（*** 与空串两路径） |
| C-F1-05 停用 409→启用恢复；软删历史可查 | ✅ PASS | TestDisableEnable_C_F1_05 + TestDisableDeleteMessaging_C_F1_05 |
| U-F2-01 同步回显+落库 sender=agent | ✅ PASS | `internal/session/private` TestSyncEcho_U_F2_01 |
| U-F2-02 同步超时→占位+session 保留 | ✅ PASS | TestSyncTimeoutPlaceholder_U_F2_02 |
| U-F2-03 流式 ≥2 delta+1 final | ✅ PASS | TestStreamDeltas_U_F2_03（Router 级）+ API 级 TestSessionAsyncAndStreamAPI |
| U-F2-04 流式无 delta→error 事件 | ✅ PASS | TestStreamIdleTimeout_U_F2_04（看门狗阈值注入） |
| U-F2-05 续话 external_ref 一致 | ✅ PASS | TestSessionContinuation_U_F2_05（echod 记录断言） |
| U-F2-06 无会话机制第 11 条含第 1 轮 | ✅ PASS | TestStatelessContextWindow_U_F2_06（stateless echod） |
| U-F2-07 超 20 轮→摘要标记 | ✅ PASS | TestSummaryTrigger_U_F2_07（[summary] 标记） |
| U-F2-08 async→accepted→system 回填 | ✅ PASS | TestAsyncReceiptAndBackfill_U_F2_08 + API 级 202 断言 |
| C-F2-01/02 真实 agent e2e | ⏭️ 跳过 | 铁律禁止真实 agent e2e（M1 验收日执行） |
| U-F3-01 @路由唯一响应 | ✅ PASS | `internal/arbiter` TestMentionRouting_U_F3_01 |
| U-F3-02 相关性×priority、并列取未发言者 | ✅ PASS | TestRelevancePriority_U_F3_02 |
| U-F3-03 回合占用 Deferred | ✅ PASS | TestTurnOccupancy_U_F3_03 |
| U-F3-04 预算超限 Deferred+降权 | ✅ PASS | TestBudget_U_F3_04 |
| U-F3-05 防连环全拒 | ✅ PASS | TestChainGuard_U_F3_05 |
| U-F3-06 静默 4 轮 Freeze+摘要+Notify | ✅ PASS | TestSilenceFreeze_U_F3_06 |
| U-F3-07 /topic 预算归零 | ✅ PASS | TestTopicReset_U_F3_07 |
| U-F3-08 插话 IL=2/1/0 全分支 | ✅ PASS | TestInterruptLevels_U_F3_08 |
| U-F3-09 冻结仅主持人解冻 | ✅ PASS | TestFrozenState_U_F3_09 |
| U-F3-10 空内容不占预算 | ✅ PASS | TestEmptyContentNoBudget_U_F3_10 |
| U-F4-01~09 / C-F4-01 | ⏭️ 未做 | M4 范围（任务清单禁止触碰） |
| U-F5-01 无/错 token 401 | ✅ PASS | `internal/api` TestAuth_U_F5_01 |
| U-F5-02 61 次→429 | ✅ PASS | TestRateLimit_U_F5_02 |
| U-F5-03 SSE 重放不丢不重 | ✅ PASS | TestSSEReplayLastEventID_U_F5_03（恰好 6,7,8） |
| U-F5-04 统一错误结构 | ✅ PASS | TestErrorEnvelope_U_F5_04 |
| U-F5-05 CLI REPL | ⏭️ 未做 | 任务 7 范围限定 serve/list/register（TODO#10） |
| C-F5-01 Web 全页面走查 | ⏭️ 部分 | 仅做了范围内两页面（列表+向导）自动冒烟；群聊/任务页属后续里程碑 |
| U-F6-01 千事件全量落库 | ✅ PASS | `internal/bus` TestBusPersistAllEvents（700ms 内断言） |
| U-F6-02 慢订阅丢弃计数+1 | ✅ PASS | TestBusSlowSubscriberDropped（9/10 丢弃） |
| U-F6-03 三订阅者全量独立 | ✅ PASS | TestBusThreeSubscribersAllEvents |
| U-F6-04 万条消息查 20 条 <10ms | ✅ PASS | `internal/repo` 实测 **98µs** |
| U-F6-05 重启群状态快照恢复 | ⏭️ 未做 | 依赖 M3 群协调器壳（TODO#9） |
| U-F6-06 VACUUM INTO 备份 | ⏭️ 未做 | TODO#9 |
| U-F7-01 挂起超时→进程组 kill 无残留 | ✅ PASS | `internal/adapter/exec` TestGroupKillOnTimeout_U_F7_01 + echod 实证（echo 包） |
| U-F7-02 SIGTERM 不退→SIGKILL | ✅ PASS | TestSIGKILLEscalation_U_F7_02（ExitSignal==SIGKILL 断言） |
| U-F7-03 总子进程上限排队 | ✅ PASS | TestGlobalChildLimitQueue_U_F7_03（上限 2 时 3 个排队） |
| U-F7-04 无超时配置启动告警 | ✅ PASS | TestDeriveDeadlineWarn_U_F7_04 + serve 启动告警日志 |
| U-F7-05 kill 尾部+退出码入 Result | ✅ PASS | TestKilledResultTailAndExitCode_U_F7_05（adapter 层据此发事件，picoclaw 错误信息含 killed/tail/exit） |
| 3.1 故障注入矩阵 | ✅ 大部分 | 进程 kill/网关慢响应/超时→hermes/picoclaw/exec 测试覆盖；event_log 落库失败→bus 重试+丢弃不阻塞；SQLite 只读启动失败/1MB 长消息截断/50 并发未专项覆盖 |
| 3.2 性能基线 | ⏭️ 未做 | M4 前达成即可（test-plan 注明） |
| 3.3 安全验证 | ✅ 部分 | 日志脱敏（U-F1-08 全局 grep 断言）；命令模板仅来自配置、参数不经 shell（消息内容无法注入命令）；.env/*.local.yaml 已在 .gitignore；数据目录 0600 未逐项核验 |

**汇总：U-F1/U-F2/U-F3/U-F6/U-F7/U-F5 可自动化用例全绿；C-F1-01~05 全绿；e2e（C-F2/C-F3-03/C-F4-01）按铁律跳过。**

## ③ DoD curl 链路实际执行输出

内置降级模式（engine=builtin）`nexus serve`，监听 127.0.0.1:19171，SQLite WAL。

### ① GET /api/adapters 拿到 echod ConfigSchema

```
$ curl -s http://127.0.0.1:19171/api/adapters
[{"name":"echod","config_schema":{"fields":[{"name":"mode","label":"行为模式","type":"string",
"required":true,"default":"echo","options":["echo","fixed","delay","hang","stream","down"],...},
{"name":"api_key",...,"secret":true,...},...]}},{"name":"hermes",...},{"name":"picoclaw",...}]
```

### ② POST /api/agents 注册 echod（探活通过自动 online）

```
$ curl -s -X POST .../api/agents -d '{"name":"echod-demo","adapter":"echod","config":{"mode":"echo"}}'
{"id":"GJ5Sj4vqKLH3_L5FXSEHM","name":"echod-demo","adapter":"echod","status":"online",
 "config":{"delay_ms":0,"mode":"echo",...},"capabilities":{"can_group_chat":true,
 "can_stream":false,"max_concurrency":1,"typical_latency":"50ms","interrupt_level":1},
 "last_heartbeat_at":"2026-11-06T13:31:18.703676695+08:00","in_flight":0,"disabled":false}
```

### ③ POST /api/agents/{id}/test 连通性测试通过

```
$ curl -s -X POST .../api/agents/GJ5Sj4vqKLH3_L5FXSEHM/test
{"ok":true,"duration_ms":0.009}
```

### ④ POST /api/sessions 建私聊 → POST messages → 收到回显

```
$ curl -s -X POST .../api/sessions -d '{"agent_id":"GJ5Sj4vqKLH3_L5FXSEHM"}'
{"id":"ux-TOkS5UUbS2q7mHlkOv","agent_id":"GJ5Sj4vqKLH3_L5FXSEHM","kind":"private",
 "created_at":"2026-11-06T05:31:24.670Z"}

$ curl -s -X POST .../api/sessions/ux-TOkS5UUbS2q7mHlkOv/messages \
    -d '{"content":"你好 echod，这是 DoD 回显验证","mode":"sync"}'
{"message":{"id":2,"sender_type":"agent","sender_id":"GJ5Sj4vqKLH3_L5FXSEHM",
 "content":"你好 echod，这是 DoD 回显验证","reply_to":"1",...},
 "reply":{...同上...},"timed_out":false}
```

### ⑤ SSE 订阅 /api/stream 收到全过程事件

```
$ curl -s -N http://127.0.0.1:19171/api/stream
id: 1
event: agent.status
data: {"agent_id":"GJ5Sj4vqKLH3_L5FXSEHM","name":"echod-demo","from":"","to":"online","reason":"initial probe ok"}

id: 2
event: msg.human
data: {"session_id":"ux-TOkS5UUbS2q7mHlkOv",...,"content":"你好 echod，这是 DoD 回显验证","message_id":1,...}

id: 3
event: msg.agent
data: {...,"content":"你好 echod，这是 DoD 回显验证","message_id":2,"reply_to":"1"}

id: 4 / id: 5   （SSE 验证第二轮 msg.human + msg.agent）
id: 6
event: agent.status
data: {"agent_id":"My2GZIOXEPUnSkNP2_YB_","name":"echod-demo2",...,"to":"online",...}
```

### kill -9 重启验证

```
$ kill -9 <nexus 主进程 PID> && 重启 nexus serve
$ curl -s .../api/agents
echod-demo2 → online heartbeat: 2026-11-06T13:31:54.879Z   （LoadPersisted 重新探活）
echod-demo  → online heartbeat: 2026-11-06T13:31:54.879Z

$ curl -s -N ".../api/stream?last_event_id=0" | grep ^id:    （event_log 持久仍在）
id: 1 … id: 6   全量重放，无丢失

serve2.log: {"msg":"agent restored","name":"echod-demo2","status":"online"}
           {"msg":"agent restored","name":"echod-demo","status":"online"}
```

## ④ TODO.md 摘要（全文见 TODO.md）

- **需文档决策**：disable 状态持久化缺 DDL 列；Web UI 与 Bearer 鉴权的组合方式；C-F1-03 坏 endpoint 用例改用 hermes 指向死端口构造。
- **文档允许的降级**：F3 相关性一期为 bigram-Jaccard（Scorer 接口已抽象，gse 一行可换）；F2 摘要默认截断降级；messages 同步落库；限流为窗口计数。
- **未做（范围外/后续里程碑）**：U-F6-05/06（群状态快照、VACUUM 备份）、U-F5-05 CLI REPL、会话 24h 回收、群协调器壳与 groups API、任务编排 API、性能基线（M4 前）。
- **工程备注**：3 个提交先行后立即修复转绿（详见 TODO#14）；本机 18080 被 SearXNG 占用，DoD 用 19171；环境代理需 curl --noproxy。

## ⑤ 未完成项与建议

| 未完成 | 原因 | 建议 |
|---|---|---|
| U-F6-05/06、群状态快照 | 群协调器壳属 M3 | M3 开工时以 `arbiter.ApplyDecision` + groups 表落壳，快照 30s 落库 |
| U-F5-05 CLI REPL | 任务 7 范围限定 | 复用 cmd/nexus 的 client，`chat` 子命令读 stdin 行 + Ctrl-C 取消 |
| VACUUM INTO 每日备份 | F6.2 运营项 | serve 内置 ticker + 保留 7 份轮转 |
| F4 任务编排（DAG/cron/Temporal） | 铁律禁 Temporal、M4 范围 | orchestr.Scheduler 接口已就绪，builtin 已能跑单任务（async 已用） |
| 真实 agent 冒烟（M1 验收） | 铁律禁真实 e2e | 白天人工 `nexus agent register` + `nexus serve` 冒烟 hermes/picoclaw 各 1 条 |
| 会话 24h 闲置回收 | 时间取舍 | serve 内置 ticker 扫 `closed_at IS NULL AND created_at < now-24h` |
| disable 持久化 | DDL 无列 | 建议加列（见 TODO#1），迁移脚本 00002 |
| UI 鉴权 | 需产品决策 | cookie 会话或 `?token=` 登录（见 TODO#2） |

## DoD 逐条自查

- [x] `go build ./...` 通过
- [x] `go vet ./...` 干净
- [x] `gofmt -l` 无输出
- [x] `go test ./...` 全绿（12 个测试包，多轮 `-count=1` 验证）
- [x] U-F1-xx / U-F6-xx / U-F7-xx / U-F2-xx 全部可自动化用例通过；U-F3-01~10 亦全绿
- [x] 内置降级模式 nexus serve 可启动，curl 链路 ①-⑤ 全程通（输出见上）
- [x] kill -9 重启：agent 状态重新探活、event_log 数据仍在（输出见上）
