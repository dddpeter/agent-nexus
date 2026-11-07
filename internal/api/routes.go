package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// mountAPI 挂载 /api 路由。
// 任务 7 仅占位；agents/adapters（任务 10）与 sessions/stream（任务 12-14）随各自任务落地。
func (s *Server) mountAPI(r chi.Router) {
	r.Get("/", s.handleAPIIndex)

	// 浏览器会话（TODO#2：login/status 免鉴权在 auth 中间件放行，logout 需已登录）
	r.Post("/auth/login", s.handleAuthLogin)
	r.Get("/auth/status", s.handleAuthStatus)
	r.Post("/auth/logout", s.handleAuthLogout)

	// adapters（注册向导数据源）
	r.Get("/adapters", s.handleListAdapters)
	r.Get("/adapters/hermes/discover", s.handleHermesDiscover) // 本机网关自动探测（降低注册难度）

	// SSE 全局流（断线 Last-Event-ID 补偿重放）
	r.Get("/stream", s.handleGlobalStream)

	// 事件检索视图（可观测性）：最近事件 / 按类型过滤
	r.Get("/events", s.handleListEvents)

	// 私聊会话（任务 12-14）
	if s.deps.Router != nil {
		r.Post("/sessions", s.handleCreateSession)
		r.Get("/sessions", s.handleRecentSessions) // 聊天侧栏：最近会话（跨 agent）
		r.Route("/sessions/{id}", func(r chi.Router) {
			r.Get("/", s.handleGetSession)
			r.Get("/messages", s.handleSessionMessages)
			r.Post("/messages", s.handleSessionSend)
		})
		r.Get("/agents/{id}/sessions", s.handleAgentSessions)
	}

	// 群聊（M3；协调器未启用时返回 501）
	r.Post("/groups", s.handleCreateGroup)
	r.Get("/groups", s.handleListGroups)
	r.Route("/groups/{id}", func(r chi.Router) {
		r.Get("/", s.handleGetGroup)
		r.Get("/stream", s.handleGroupStream) // FR5.2 群独立 SSE
		r.Get("/messages", s.handleGroupMessages)
		r.Post("/messages", s.handleGroupSend)
		r.Post("/unfreeze", s.handleGroupUnfreeze)
		r.Post("/topic", s.handleGroupTopic)
		r.Put("/config", s.handleGroupConfig)
	})

	// 任务编排（M4；编排器未启用时 501）
	r.Post("/tasks", s.handleCreateTask)
	r.Get("/tasks", s.handleListTasks)
	r.Route("/tasks/{id}", func(r chi.Router) {
		r.Get("/", s.handleGetTask)
		r.Post("/cancel", s.handleCancelTask)
		r.Post("/resume", s.handleResumeTask)
	})
	r.Get("/cron", s.handleListCron)
	r.Delete("/cron/{id}", s.handleDeleteCron)

	// agents
	r.Post("/agents", s.handleRegisterAgent)
	r.Get("/agents", s.handleListAgents)
	r.Post("/agents/test", s.handleTestSpec) // 保存前按规格测试（向导用）
	r.Route("/agents/{id}", func(r chi.Router) {
		r.Get("/", s.handleGetAgent)
		r.Put("/", s.handleUpdateAgent)
		r.Post("/test", s.handleTestAgent)
		r.Post("/disable", s.handleDisableAgent)
		r.Post("/enable", s.handleEnableAgent)
		r.Delete("/", s.handleDeleteAgent)
	})
}

func (s *Server) handleAPIIndex(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "nexus",
		"status":  "ok",
		"version": "0.1.0",
	})
}
