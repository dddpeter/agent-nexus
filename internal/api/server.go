// Package api 提供 REST API、SSE 与鉴权中间件（F5 接入层）。
// 中间件链（F5.3）：recover → requestID → 鉴权 → 限流 → 日志。
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/internal/config"
	"github.com/dddpeter/agent-nexus/internal/registry"
	"github.com/dddpeter/agent-nexus/internal/repo"
	"github.com/dddpeter/agent-nexus/internal/session/private"
)

// Deps API 层依赖（全部为下层公开接口；api 不得被下层 import）。
type Deps struct {
	Cfg      config.Config
	Store    repo.Store
	Bus      bus.Bus
	Registry *registry.Impl
	Router   *private.Router // 私聊会话路由（任务 12 起）
	Log      *slog.Logger
}

// Server HTTP 接入层。
type Server struct {
	deps     Deps
	router   chi.Router
	limiter  *rateLimiter
	sessions *sessionStore
}

// New 构造 API server 并挂载全部路由。
func New(deps Deps) *Server {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	// 60 req/min（U-F5-02）；会话存储供浏览器 cookie 鉴权（TODO#2）
	s := &Server{deps: deps, limiter: newRateLimiter(60, time.Minute), sessions: newSessionStore(sessionTTL)}
	s.mount()
	return s
}

func (s *Server) mount() {
	r := chi.NewRouter()

	r.Use(middlewareRecover(s.deps.Log))
	r.Use(middlewareRequestID)
	r.Use(middlewareAuth(s.deps.Cfg.Server.AuthToken, s.sessions, s.deps.Log))
	r.Use(middlewareRateLimit(s.limiter))
	r.Use(middlewareLogger(s.deps.Log))

	// 健康检查（无需鉴权在 auth 中放行）
	r.Get("/healthz", s.handleHealthz)

	r.Route("/api", func(r chi.Router) {
		s.mountAPI(r)
	})

	// Web UI 静态资源（go:embed web/；任务 11）
	s.mountWeb(r)

	s.router = r
}

// Handler 返回根 http.Handler（测试与 Run 共用）。
func (s *Server) Handler() http.Handler { return s.router }

// Run 启动 HTTP 服务（阻塞）。
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.deps.Cfg.Server.Listen,
		Handler:           s.router,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	s.deps.Log.Info("nexus api listening", "listen", s.deps.Cfg.Server.Listen, "engine", s.deps.Cfg.Orchestration.Engine)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
