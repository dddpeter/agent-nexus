# TEST-REPORT — 集成测试报告（2026-11-07 轮）

> 日期：2026-11-07 · 测试人：zcode（静态门禁 + 15 包单测 + 黑盒集成套件 46 项 + 浏览器 SPA 走查）
> 被测版本：`main@3297981`（含本轮测试中发现的 **2 处缺陷修复**）· 环境：本机 Go 1.26.8 / SQLite WAL / 真实 hermes 网关 v0.22.7（:8642）/ 真实 picoclaw CLI v0.5.4
> 范围：2026-11-06 报告（`TEST-REPORT-20261106.md`，T01–T35）之后的全部变更——SPA 五页面、流式渲染修复、disable 持久化（迁移 00002）、会话闲置回收、事件页、hermes/picoclaw 真实协议重写、M3 群聊协调器（迁移 00003）、VACUUM 备份、CLI REPL

## 总体结论

| 层 | 结果 |
|---|---|
| 静态门禁（build / vet / gofmt / vue-tsc） | ✅ 全过 |
| 自动化单测 | ✅ **15 个测试包全绿 / 0 失败**（新增回归：群协调器 6 项、群 API 3 项、备份 1 项、事件 API 1 项、config 默认值、流式落库竞态） |
| 黑盒集成套件（`scripts/integration_test.py`，双实例自管理） | ✅ **46/46 通过**（8 组 I01–I85，含真实 LLM 调用） |
| 浏览器 SPA 走查 | ✅ 五页面直达路由、私聊流式渐进渲染（修复后复验）、群聊仲裁可视化 |
| **测试中发现并修复的真实缺陷** | **2 个**（P0 × 1、P1 × 1，见 §缺陷） |

## 一、集成测试矩阵（46 项全绿，节选关键项）

> 完整矩阵由 `python3 scripts/integration_test.py` 可重复执行产出（`--skip-real` 跳过 LLM 成本）。

### B. 启动 / 迁移 / 持久化（F6）

| # | 用例 | 结果 | 证据 |
|---|---|---|---|
| I01 | healthz 就绪 | ✅ | `{"ok":true}` |
| I02 | 迁移 00002/00003 在新库自动应用 | ✅ | `agents.disabled=True groups.state_json=True` |
| I03 | VACUUM INTO 启动快照 | ✅ | `backups/nexus-20261107-*.db` 生成 |

### C/D. Agent 生命周期 + kill -9 持久化（F1）

| # | 用例 | 结果 | 证据 |
|---|---|---|---|
| I11/I12/I13 | 注册 echod（同步/流式）→ online；重名 409 | ✅ | `duplicate_name` |
| I14/I15 | 停用 → 派消息 409 `agent_unavailable`；disabled 可见 | ✅ | |
| I20 | **kill -9 → 重启后 disabled=true 持久恢复**（TODO#1 闭环验证） | ✅ | `disabled=True status=offline`（停用不探活） |
| I21/I22 | enable → probe 恢复 online；健康 agent 重启自动 online | ✅ | |

### E. 私聊回归 + 本轮修复（F2）

| # | 用例 | 结果 | 证据 |
|---|---|---|---|
| I30 | 同步发送回显 + reply_to | ✅ | |
| I31/I32 | **历史升序契约** + `?limit=2` 最近 N 条仍升序 | ✅ | 8 条升序 / n=2 |
| I33/I34 | 模式不匹配双向 409 `mode_not_supported` | ✅ | |
| I35 | 流式 SSE ≥2 delta + 1 final（`stream_interval_ms` 节奏） | ✅ | delta=3 final=1 |
| I36/I37 | async 202 + accepted 回执 + system 回填落库 | ✅ | |
| I38/I39 | `GET /api/events` seq 降序 + type 过滤 | ✅ | n=42 / n=7 |

### F/G. 群聊协调器（M3）

| # | 用例 | 结果 | 证据 |
|---|---|---|---|
| I40/I41 | 无 agent 成员 400；建群（预算 1/静默阈值 1） | ✅ | `grp-…` |
| I42 | **@注册名路由** → 唯一响应 + 回复落库 | ✅ | reply.sender=agent |
| I43/I44 | 超预算 → **Deferred 存档**（reason=budget_exceeded + system 消息可检索） | ✅ | |
| I45/I46/I47 | 解冻 API 幂等；`/topic` 预算归零；阈值热更新即时生效 | ✅ | turns_used={}=0 → 5 |
| I48 | arbiter.decision 事件全量落库（回放调参，F3.5） | ✅ | n≥3 |
| I50 | **kill -9 → 重启后群运行态计数恢复**（U-F6-05 闭环验证） | ✅ | before=after |

### H/I. CLI 与鉴权（F5）

| # | 用例 | 结果 | 证据 |
|---|---|---|---|
| I60–I62 | `nexus agent list` / `chat` REPL 同步往返 / `group send` 经仲裁路由 | ✅ | FR5.4 |
| I70–I74 | 无/错 token 401；正确 token 200；群资源同受保护；统一错误结构 | ✅ | 实例 B 带独立 config |

### J. 真实 agent（hermes :8642 / picoclaw CLI）

| # | 用例 | 结果 | 证据 |
|---|---|---|---|
| I80 | hermes 注册 → 真实网关 /health 探活 online | ✅ | Bearer `API_SERVER_KEY` |
| I81 | hermes 同步发送（真实 LLM） | ✅ | reply=收到 |
| I82 | hermes 流式（真实 OpenAI SSE → 统一 delta/final 帧） | ✅ | delta=2 final=「1、2、3」 |
| I83 | hermes 续话记忆（external_ref 稳定） | ✅ | 回忆上一轮指令原文 |
| I84/I85 | picoclaw probe online；同步发送 lastline 解析 CLI 噪音 | ✅ | reply=鲎 链路畅通 |

## 二、浏览器 SPA 走查

| # | 用例 | 结果 | 证据 |
|---|---|---|---|
| W1 | 五页面直达路由无白屏（/ /agents/new /chat /groups /events） | ✅ | h1 全部正确渲染（限流修复后） |
| W2 | 私聊流式渐进渲染（竞态修复后复验） | ✅ | 采样 lens=[8],[8,2],[8,4],[8,6],[8,8]：气泡数恒 2、agent 泡渐进增长；**结束后气泡保留、服务端落库 human+agent 双条** |
| W3 | 群聊仲裁可视化 | ✅ | 预算徽标 `1/1`、Deferred system 消息高亮类、@ 快捷条 |

## 三、测试中发现并修复的真实缺陷（2 个）

### 缺陷 1（P0）：流式 final 落库竞态丢失 agent 回复

- **现象**：浏览器流式发送后 agent 气泡消失，服务端仅有人消息；日志 `persist stream final failed … context canceled`。
- **根因**：API 层转发完 final 帧即 `return` → `defer cancel()` 取消请求 ctx；此时 Router goroutine 才开始 `finalizeStream(r.ctx)` 落库 → 插入失败。浏览器 fetch 读帧时序必现（curl 偶现，故 11-06 轮未暴露）。
- **修复**（`ad40d4c`）：final 落库与 idle 超时 system 消息改用独立背景 ctx（15s 超时）；持久化不依附请求生命周期。
- **回归**：`TestStreamFinalPersistAfterCancel`（final 后立即 cancel，回复仍必须落库）。

### 缺陷 2（P1）：限流误杀静态资源 → 限流窗口内整站白屏

- **现象**：五页面走查中 /groups、/events 渲染空壳；诊断显示 `index-*.js` HTTP 429。
- **根因**：限流中间件挂在根路由——60 req/min 窗口打满时 SPA shell 与 hash 资源一并 429，前端完全无法加载。限流本意只保护数据面。
- **修复**（`c8592e4`）：限流移入 `/api` 路由组。
- **验证**：API 70 连发后 `api=429` 而 `assets=200`、`/groups=200`。

### 测试设计注记（非缺陷）

- **静默自动冻结（FR3.5）在 v1 运行态不可达**：触发路径依赖 agent 主动候选（v1 无自发通道，人发言会清零计数），属"宁严勿松"的预期收敛；纯函数语义由 U-F3-06/09 覆盖，待 agent 自发发言通道（插话/群任务）接入后联通。
- 集成脚本初版 4 个自身 bug（重启误清数据目录、复用已停用 agent、漏启实例 B、事件 flush 时序）已在调试中修复——修复过程本身验证了"重启不清数据"与"停用恢复"语义。

## 四、M4 任务编排追加轮（同日晚）

M4（F4 协作链 DAG / cron / 重试取消恢复 / 任务 API+页面+CLI）交付后追加 K 组，套件扩至 **53/53 通过**：

### K. 任务编排（M4）

| # | 用例 | 结果 | 证据 |
|---|---|---|---|
| I90/I91 | 提交协作链 → done；**{{prev}} 渲染正确** | ✅ | result=第二环 prev=K 组第一环 |
| I92 | 不支持同步的节点 → 重试耗尽 failed（nodes 保留） | ✅ | |
| I93 | resume 非 failed 任务 → 409 拒绝 | ✅ | |
| I94/I95/I96 | cron 登记 / 非法表达式 400 / 删除 | ✅ | |

组件级 U-F4-01~09 全绿（15 包）；`nexus task submit/status/cancel` CLI 与 /tasks 页面（链表单/节点时间线/恢复按钮/cron 管理）浏览器走查通过。真实链 C-F4-01（picoclaw→hermes→picoclaw）中 hermes 流式 delta 出现模型自语外显（小爱马行为特性），final 对齐正确——nexus 侧无需处理。

**遗留更新**：U-F4-05 的 CLI 子进程 kill 联动（F7）由 execx 治理覆盖（hang 单测 ctx 感知）；cron 表达式不含秒级/时区配置（一期分钟粒度足够）。

## 五、收官轮：规划内遗留项全部闭环（同日）

| 项 | 结果 | 证据 |
|---|---|---|
| TODO#5 私聊摘要 agent 化 | ✅ | agentSummarizer：内容寻址缓存（同轮次不重复 LLM）、失败/超时降级截断（F2.3 路径）；回归 `TestAgentSummarizer` 五分支 |
| 安全 3.3 数据目录 0600 | ✅ | 实测新库 `data`=700、`nexus.db`=600（OpenSQLite 创建/收敛） |
| FR5.2 群独立 SSE | ✅ | `GET /api/groups/{id}/stream` 群消息+仲裁决策过滤转发、Last-Event-ID 同水位重放，实测收到 id:69/70 |
| **C-F4-01 真实三 agent 协作链 e2e** | ✅ | **picoclaw 写代码（230s 全链 done）→ hermes 评审（终端实测 15 用例，发现真 bug：'İ' U+0130 lower() 展开误判）→ picoclaw 据评审修订（NFKD 归一化 + isinstance 守卫）**；全程结果落库，{{prev}} 语义正确 |
| 附属修复 | ✅ | picoclaw `output_prefix`：lastline 多行回复/代码块完整提取（首跑 e2e 抓到空围栏缺陷） |
| 前端 v4 HUD 强化 | ✅ | ruofei-frontend-design 技能指导：JetBrains Mono 本地打包全等宽指挥台、琥珀次级信号、卡片四角 bracket、扫描线/噪点/辉光三层氛围、stagger reveal |

## 六、结论与遗留

- 11-06 报告之后的两轮改造（真实 agent 接入重写、M3 群聊、持久化/备份）经 46 项黑盒集成 + 浏览器走查验证**全绿**；过程中发现的 2 个真实缺陷均已修复并带回归测试。
- 遗留（终版）：FR3.5 静默冻结运行时触发路径（结构性收敛于 v1"agent 不自发"设计，随自发通道接入）；Temporal 引擎（二期，接口同签名）。**M0-M4 全部里程碑、TODO 清单功能项、验收用例（含真实 e2e）至此全部闭环。**
