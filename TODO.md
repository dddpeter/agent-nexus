# TODO — 待决事项与已知缺口

> 本夜实施过程中发现的问题、文档疑点与绕行方案。按铁律 5：不修改 docs/，记录于此待文档 owner 决策。

## 文档问题 / 设计决策待定

1. **disable/enable 状态无法持久化** ✅ 已解决（2026-11-07）
   - 原现象：停用状态仅存 registry 内存，重启后丢失并自动重新探活恢复。
   - 方案：迁移 00002 增加 `agents.disabled INTEGER NOT NULL DEFAULT 0` 列（不扩展 status 枚举，
     disabled 与探活状态机正交）；Disable/Enable 落库，`LoadPersisted` 启动恢复（停用 agent 不探活
     直接 offline，Enable 时主动 probe）；心跳对停用 agent 跳过探活避免状态抖动。
   - 回归：`TestDisablePersistenceAcrossRestart`（registry 层 kill -9 语义重启恢复）。

2. **Web UI 与 Bearer 鉴权的组合方式** ✅ 已解决（2026-11-06）
   - 原现象：浏览器无法带 `Authorization: Bearer` 头，配置 token 后页面不可用。
   - 方案：SPA + cookie 会话落地——`POST /api/auth/login` 校验 token 后签发 HttpOnly
     SameSite=Strict 会话 cookie（7 天），auth 中间件双通道（Bearer 供 API/CLI、cookie 供浏览器）；
     未配置 auth_token 保持本地免登模式。SPA 静态 shell 不含数据、不参与鉴权。

3. **C-F1-03 的「指向不存在的 endpoint」用例在 echod 上无法构造**
   - echod 为进程内 adapter，无 endpoint 概念。用例改用 hermes adapter 指向 `127.0.0.1:1`（连接拒绝）验证「明确错误而非泛化 500」，语义等价。
   - echod 坏配置用 `mode=down`（探活失败）模拟，已覆盖「保存被拦 + 旧配置生效」。

## 一期简化（文档允许的降级范围内）

4. **F3.3 相关性分词**：设计指定 gse；一期实现为 rune-bigram + Jaccard（`arbiter.Scorer` 接口已抽象，替换 gse 为一行注入）。原因：夜间时间与模块体积权衡。`Member.Profile` 关键词画像即打分数据源。
5. **F2.3 摘要生成**：`Summarizer` 注入点已留，默认实现为「[summary] 标记 + 截断」（设计允许的降级路径）；「用任意可用 agent 生成摘要」待 M2 后期接入。
6. **messages 落库时序**：设计为「异步写入」；当前为同步写（本地 SQLite WAL 单条插入 <1ms，不阻塞回复路径），U-F2-01 语义不受影响。如需严格异步可改为 bus 消费者。
7. **限流实现**：简单窗口计数（60 req/min）而非令牌桶；U-F5-02 的 61 次 → 429 语义精确满足。
8. **orchestr**：按铁律仅实现 `Scheduler` 接口 + builtin 降级（goroutine + tasks 表，重启 running → failed，已文档化）。Temporal 实现未做（禁止项）。

## 未完成项（按任务范围裁剪，非本夜 DoD）

9. **U-F6-05/06**：群状态快照恢复（依赖 M3 群协调器壳）与 `VACUUM INTO` 定时备份（F6.2 备份策略）未实现。
10. **U-F5-05 CLI REPL**（`nexus chat`）：任务 7 范围限定 serve/list/register；REPL 属 FR5.4 完整形态。
11. **FR2.5 会话闲置 24h 自动回收** ✅ 已实现（2026-11-07）：`internal/session.Janitor` 周期扫描
    （`server.session_sweep_interval` 默认 30min），闲置阈值 `server.session_idle_timeout` 默认 24h
    （显式负值禁用）；闲置 = 末条消息早于阈值（无消息回退 created_at）；归档时追加系统消息说明原因。
    回归：`TestJanitorSweep` + `TestSessionCloseIdle`（memory/sqlite 双实现）。
12. **仲裁器状态快照 30s 异步落库**（F3.3）：`ApplyDecision` 纯转移已就绪，群状态壳（M3）负责落库。
13. **serve 静态 agents 的 upsert 语义**：同名 agent 走 `UpdateConfig(force=false)`，新配置探活失败时跳过更新并记日志（保留旧配置运行）。

## 工程备注

14. 本夜 3 个提交（c18ff27 / 87ee903 / 29ad95c）在测试未全绿时先行提交，随后即以修复提交（5620ce8 / 33f644b / c811759）转绿；最终树全绿，过程纪律记录在此备查。
15. 环境代理会拦截 localhost HTTP（curl 需 `--noproxy '*'`）；本机 18080 端口被 SearXNG 占用，DoD 验证使用 19171。
