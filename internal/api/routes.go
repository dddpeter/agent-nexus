package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// mountAPI 挂载 /api 路由。
// 任务 7 仅占位；agents/adapters（任务 10）与 sessions/stream（任务 12-14）随各自任务落地。
func (s *Server) mountAPI(r chi.Router) {
	r.Get("/", s.handleAPIIndex)
}

// mountWeb 挂载 Web 静态资源（任务 11 htmx 页面）。
func (s *Server) mountWeb(r chi.Router) {}

func (s *Server) handleAPIIndex(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "nexus",
		"status":  "ok",
		"version": "0.1.0",
	})
}
