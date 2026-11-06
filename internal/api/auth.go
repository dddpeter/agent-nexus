package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// ---------- Cookie 会话（TODO#2：浏览器无法携带 Bearer 头）----------
// SPA + EventSource 通过 HttpOnly cookie 携带凭证；CLI/API 仍走 Bearer（中间件双通道）。
// 内存会话存储：单用户工具，重启失效可接受（前端 401 后跳登录页重登）。

const sessionCookie = "nexus_session"

const sessionTTL = 7 * 24 * time.Hour

// sessionStore 会话 id → 过期时间。
type sessionStore struct {
	mu       sync.Mutex
	ttl      time.Duration
	sessions map[string]time.Time
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, sessions: map[string]time.Time{}}
}

// create 生成高熵会话 id 并登记；顺手清理过期会话。
func (s *sessionStore) create() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)
	now := time.Now()
	s.mu.Lock()
	for k, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = now.Add(s.ttl)
	s.mu.Unlock()
	return id, nil
}

func (s *sessionStore) valid(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[id]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, id)
		return false
	}
	return true
}

func (s *sessionStore) revoke(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// ---------- /api/auth handlers ----------

type loginReq struct {
	Token string `json:"token"`
}

type authStatusResp struct {
	AuthRequired  bool `json:"auth_required"`
	Authenticated bool `json:"authenticated"`
}

// handleAuthLogin POST /api/auth/login {token} —— 校验配置的 auth_token，换取会话 cookie。
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	expect := s.deps.Cfg.Server.AuthToken
	if expect == "" {
		writeError(w, r, http.StatusBadRequest, "auth_not_required",
			"auth_token not configured; local mode allows all requests")
		return
	}
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", `body must be {"token": "..."}`)
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(expect)) != 1 {
		writeError(w, r, http.StatusUnauthorized, "unauthorized", "invalid token")
		return
	}
	id, err := s.sessions.create()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAuthLogout POST /api/auth/logout —— 注销会话并清除 cookie。
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAuthStatus GET /api/auth/status —— 前端路由守卫用；免鉴权。
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	expect := s.deps.Cfg.Server.AuthToken
	st := authStatusResp{AuthRequired: expect != ""}
	if expect == "" {
		st.Authenticated = true // 本地免登模式
	} else if c, err := r.Cookie(sessionCookie); err == nil && s.sessions.valid(c.Value) {
		st.Authenticated = true
	}
	writeJSON(w, http.StatusOK, st)
}
