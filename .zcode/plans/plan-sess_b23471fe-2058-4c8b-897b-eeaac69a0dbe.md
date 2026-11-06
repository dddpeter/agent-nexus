# Agent Nexus：htmx SSR → Vue 3 SPA 前后端分离改造

## Context

htmx SSR（Go html/template + HTML 片段交换）调试困难：无 devtools、无 HMR、逻辑散落在 gohtml 内联 JS 与 Go 模板函数中。本次改造为**前后端分离**：后端收敛为纯 JSON API + SSE，前端独立为 Vue 3 + Vite + TS 工程（开发期 vite dev server + proxy，生产 embed 进单二进制，保持零部署成本）。

已确认决策：Vue 3 + Vite + TS ｜ embed 单二进制 ｜ 登录页 + Cookie 会话（一并解决 TODO#2）｜ 沿用现有 system.css 设计系统重写。

**有利现状**：`/api/*` JSON REST + 全局 SSE（`internal/api/sse.go`，Last-Event-ID 重放）+ 流式发送（`internal/api/sessions.go:167` mode=stream→SSE）均已完备且有契约测试；所有 `/ui/*` 表单操作（向导 submit/test、详情 save、行操作 disable/enable/delete）在 `/api` 层均有等价端点（`internal/api/routes.go`）。**后端仅需新增 auth 三端点**，主要工作是前端工程与 SSR 层拆除。

**前置**：git 工作区有未提交修改（web redesign 后续），先作为基线提交（计划批准即视为同意）。

---

## 一、后端改造

### 1. Cookie 会话（新增 `internal/api/auth.go` + `auth_test.go`）
- `POST /api/auth/login` `{token}`：校验 `cfg.Server.AuthToken`，通过则 `crypto/rand` 生成 session id 存内存 store（`sync.RWMutex` + map，重启失效可接受），`Set-Cookie: nexus_session=<id>; HttpOnly; SameSite=Strict; Path=/; Max-Age=604800`
- `POST /api/auth/logout`：删 session + 清 cookie
- `GET /api/auth/status`：`{auth_required, authenticated}`（前端路由守卫用）
- `auth_token` 为空 → 本地免登模式（`auth_required=false`，中间件全放行），保留现状语义
- 会话 7 天过期；单用户场景内存 store 足够

### 2. `middlewareAuth` 双通道（`internal/api/middleware.go:123`）
- 通过条件：`Authorization: Bearer`（CLI/API 保留）**或** `nexus_session` cookie（浏览器/EventSource 自动携带）
- 放行清单调整：`/healthz`、`/api/auth/login`、`/api/auth/status`、`/assets/*`、SPA fallback（静态资源公开，API 鉴权——SPA 标准做法）
- EventSource 同源自动带 cookie，全局 SSE 无需改动

### 3. SPA fallback（重写 `internal/api/web.go` 的 mount 部分）
- `web.go` 大幅缩减：删除模板 init/tplFuncs/全部 view model/SSR handlers；保留 assets serve
- 新增 catch-all：非 `/api`、非 `/assets` 的 GET → `web/dist/index.html`（vue-router history 模式）；其余 → JSON 404。chi 路由树保证 `/api` 优先精确匹配
- cookie 方案顺带降低 CSRF 风险面：写操作全部走 `/api` JSON（Content-Type: application/json），内网单用户工具不加 CSRF token

## 二、前端工程（新建 `frontend/`）

### 工程骨架
- Vue 3 + Vite + TS + vue-router（history 模式）+ pinia；包管理 npm（Makefile `FE_PM ?= npm` 变量，bun 可换）
- `vite.config.ts`：`server.proxy {'/api': 'http://127.0.0.1:8080'}`；`build.outDir '../web/dist'`（emptyOutDir）
- `src/api/client.ts`：fetch 封装（`credentials:'include'`、JSON 错误 envelope 解析、401 → 跳登录）
- TS 接口与后端 DTO 对齐：Agent/Session/Message/AdapterSchema/StreamEvent

### 页面与组件（等价迁移现有 4 页）
| 现有 SSR | SPA |
|---|---|
| 列表页 `/`（stats 条 + 搜索 + 行操作 + SSE 刷新） | `AgentListPage`：统计从 `GET /api/agents` 计算（口径 Total/Online/Offline/Removed）；`agent.status` SSE 事件直接更新 store |
| 向导 `/agents/new`（3 步：adapter→schema 动态表单→test→submit） | `WizardPage` + `DynamicConfigForm` 组件：按 `GET /api/adapters` schema 渲染 5 类字段（bool / string+options→select / secret→password+「留空保持」占位 / int、duration、list→text），test 走 `POST /api/agents/test`，submit 走 `POST /api/agents` |
| 详情 `/agents/{id}/view`（脱敏回显、保存门禁、停用/软删） | `AgentDetailPage`：`PUT /api/agents/{id}` 保存前先 `POST /{id}/test` |
| 聊天 `/chat`（会话侧栏 + 气泡 + 轮询 + SSE + 流式） | `ChatPage` + `SessionSidebar`/`ChatBubble`；全局 SSE（EventSource `/api/stream`）收到 `msg.*` 且属于当前会话 → 重拉 `GET /api/sessions/{id}/messages`；流式发送 = fetch POST mode=stream + ReadableStream 解析 SSE 帧（移植 `chat.gohtml` 现有 `chatSendStream` 逻辑为 TS） |

- `stores/`：`events.ts`（EventSource 单例 + 事件分发）、`agents.ts`、`chat.ts`
- 设计系统：`web/assets/system.css` 迁移至 `frontend/src/styles/`，class 命名不变，视觉与现版一致；非流式 agent 不显示流式按钮（CanStream 规则）等业务规则在前端保留
- 登录页 `LoginPage`：读取 `/api/auth/status`，`auth_required=false` 直接放行

## 三、构建与打包

- `web/embed.go`：改为 `//go:embed all:dist`（`web/dist/.gitkeep` 占位防编译失败）；删除 Templates embed
- Makefile：`build` 依赖 `fe-build`（npm install + npm run build）；新增 `fe-install`/`fe-build`/`fe-dev`/`fe-test` target
- `.gitignore`：加 `web/dist/`、`frontend/node_modules/`
- 删除：`web/templates/*.gohtml`（6 个）、`web/assets/htmx*.js`、`internal/api/chat.go`、`internal/api/pages.go`、`web.go` 中全部 SSR handler/VM；`web/assets/system.css` 迁移后删除

## 四、测试策略

- **保留**：`agents_test.go` / `sessions_test.go` / `sse_test.go`（API 契约，helpers 在 agents_test.go:26-76 共用）
- **删除**：`web_test.go`、`chat_test.go`、`pages_test.go`（断言的全是 SSR HTML/hx-* 属性）；其中业务断言（如「保存被连通性门禁拦截」「密钥脱敏回显」）已由对应 `/api` 测试覆盖，补齐缺的
- **新增**：`internal/api/auth_test.go`（login 成功/失败、cookie 鉴权、Bearer 兼容、本地免登模式、logout、status）
- **前端**：vitest + @vue/test-utils 骨架，覆盖 `api/client.ts` 与 `DynamicConfigForm` 的 schema 渲染逻辑；页面 E2E 以手工链路验证为准
- 业务规则迁移清单（前端必须实现）：流式按钮仅 CanStream 时显示；secret 留空保持；软删 hx-confirm→confirm 弹窗；超时占位提示可重发

## 五、文档修订

- `docs/technical-design.md`：§1 前端行（htmx+Alpine → Vue 3+Vite+TS，embed 单二进制）；§2 目录结构（+`frontend/`、`web/dist/`）；§8 API 清单补 `/api/auth/*` 与 cookie 会话说明
- `docs/detailed-design.md` §F5.3：Web UI 段重写（SPA、cookie 会话、开发代理、无构建步骤→有构建）
- `docs/plans/2026-11-06-nexus-agent-hub-design.md` §5 技术栈行同步
- `TODO.md` 条目 2 标记已解决（cookie 会话落地）；`README.md` 补前端开发/构建说明

## 六、实施顺序（原子提交）

1. 基线：提交当前未提交的 redesign 工作
2. 后端 auth 三端点 + middleware 双通道 + auth_test（此时 SSR 仍在，互不干扰）
3. 前端脚手架：frontend/ + router + client.ts + system.css 迁移 + LoginPage
4. 页面迁移 ×4：列表 → 向导 → 详情 → 聊天（聊天含 SSE store + 流式发送）
5. 切换：删除 SSR 层/模板/失效测试，web.go 重写为 SPA fallback，embed dist，Makefile target
6. 文档修订 + TODO 关闭 + README

## 验证

1. `make check && go test ./...`（全绿）；`npm run build && npx vitest run`
2. `nexus serve --listen 127.0.0.1:19171`（18080 被 SearXNG 占用；curl 需 `--noproxy '*'`）：`/healthz`、未登录 `/api/agents` 401、`/api/auth/login` Set-Cookie、带 cookie 访问通过、任意前端路由 fallback 返回 index.html
3. 配置 `auth_token` 场景端到端：登录页 → 列表 → 向导（schema 表单 + 连通性测试 + 提交）→ 详情编辑保存 → 聊天同步发送 + 流式发送渐进渲染 + 全局 SSE 驱动列表/会话刷新 → disable/enable/软删
4. 开发模式验证：`npm run dev`（vite :5173）+ proxy 到后端，HMR 生效