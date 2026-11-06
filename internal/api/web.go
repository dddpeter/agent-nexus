package api

// SPA 接入层：Vue 3 前端构建产物（web/dist，go:embed 单二进制）。
// 页面路由由 vue-router 接管：除 /api、/assets 外的 GET 一律回 index.html（history fallback）。

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/dddpeter/agent-nexus/web"
)

// mountSPA 挂载前端静态资源与 history 路由 fallback。
func (s *Server) mountSPA(r chi.Router) {
	sub, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		panic("api: embed web/dist: " + err.Error())
	}
	// hash 文件名静态资源（浏览器长缓存）
	r.Handle("/assets/*", http.FileServer(http.FS(sub)))
	r.NotFound(s.spaFallback(sub))
}

// spaFallback 前端路由回 index.html；API 未知路径保持 JSON 404。
func (s *Server) spaFallback(fsys fs.FS) http.HandlerFunc {
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		// dist 缺失（fresh clone 未跑 make fe-build）：优雅降级而非 panic
		index = []byte("<!DOCTYPE html><meta charset=\"utf-8\"><title>Nexus</title><p>前端未构建：请先执行 <code>make fe-build</code>。</p>")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api") || r.Method != http.MethodGet {
			writeError(w, r, http.StatusNotFound, "not_found", "no such route")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache") // shell 不缓存；资产由 hash 文件名长缓存
		_, _ = w.Write(index)
	}
}
