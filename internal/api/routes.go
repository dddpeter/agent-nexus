package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// mountAPI 挂载 /api 路由。
// 任务 7 仅占位；agents/adapters（任务 10）与 sessions/stream（任务 12-14）随各自任务落地。
func (s *Server) mountAPI(r chi.Router) {
	r.Get("/", s.handleAPIIndex)

	// adapters（注册向导数据源）
	r.Get("/adapters", s.handleListAdapters)

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
