// Package echodhttp 提供内置测试用假 agent "echod" 的 HTTP 形态。
// 线协议对齐真实 hermes gateway（v0.22.7 实测校准，2026-11-07）：
//
//	GET  /health                     探活
//	POST /api/sessions               建会话 → {"session":{"id":...}}
//	POST /api/sessions/{id}/chat     {"message":...} → {"message":{"content":...}}；未知会话 404
//	POST /v1/chat/completions        OpenAI 兼容 SSE（X-Hermes-Session-Id 续话）
package echodhttp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/dddpeter/agent-nexus/internal/id"
)

// Config fake gateway 行为脚本。
type Config struct {
	Mode    string // echo|fixed|delay|hang|down|stream
	Reply   string
	DelayMs int
	Chunks  int
	// APIKey 非空时要求 Authorization: Bearer <key>（密钥配置测试用）。
	APIKey string
}

// Handler 构造完成的 http.Handler。
type Handler struct {
	cfg Config
	mu  sync.Mutex
	// sessions 记录已建会话（续话语义：消息带原 session 直接复用）
	sessions map[string]bool
}

// New 构造 handler。
func New(cfg Config) *Handler {
	if cfg.Mode == "" {
		cfg.Mode = "echo"
	}
	return &Handler{cfg: cfg, sessions: make(map[string]bool)}
}

// NewMux 返回已挂载路由的 ServeMux（httptest / echod serve 共用）。
func NewMux(cfg Config) *http.ServeMux {
	h := New(cfg)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("POST /api/sessions", h.createSession)
	mux.HandleFunc("POST /api/sessions/{id}/chat", h.chat)
	mux.HandleFunc("POST /v1/chat/completions", h.completions)
	return mux
}

func (h *Handler) auth(r *http.Request) bool {
	if h.cfg.APIKey == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	return auth == "Bearer "+h.cfg.APIKey
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Mode == "down" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "echod: mode=down"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) createSession(w http.ResponseWriter, r *http.Request) {
	if !h.auth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"message": "Invalid gateway API key", "code": "gateway_auth_failed"}})
		return
	}
	if h.cfg.Mode == "down" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "gateway down"})
		return
	}
	sid := "echod-http-" + id.New()
	h.mu.Lock()
	h.sessions[sid] = true
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"object":  "hermes.session",
		"session": map[string]any{"id": sid, "source": "api_server", "message_count": 0},
	})
}

func (h *Handler) replyFor(content string) string {
	if h.cfg.Reply != "" {
		return h.cfg.Reply
	}
	return content
}

// chat POST /api/sessions/{id}/chat（真实网关同步聊天门）。
func (h *Handler) chat(w http.ResponseWriter, r *http.Request) {
	if !h.auth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"message": "Invalid gateway API key", "code": "gateway_auth_failed"}})
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "bad json: " + err.Error()}})
		return
	}
	sid := r.PathValue("id")
	h.mu.Lock()
	known := h.sessions[sid]
	h.mu.Unlock()
	if !known {
		// agent 侧会话丢失（F2.4 场景）：404 session_not_found 供 adapter 懒建重试
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]string{"message": "Session not found: " + sid, "code": "session_not_found"},
		})
		return
	}
	switch h.cfg.Mode {
	case "down":
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "gateway down"})
	case "hang":
		select {} // 挂起：由调用方 ctx 超时取消
	case "delay":
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Duration(h.cfg.DelayMs) * time.Millisecond):
		}
		h.chatCompleted(w, sid, h.replyFor(req.Message))
	default: // echo / fixed / stream（同步门整段返回）
		h.chatCompleted(w, sid, h.replyFor(req.Message))
	}
}

func (h *Handler) chatCompleted(w http.ResponseWriter, sid, content string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object":     "hermes.session.chat.completion",
		"status":     "completed",
		"session_id": sid,
		"message":    map[string]any{"role": "assistant", "content": content},
	})
}

// completions POST /v1/chat/completions：OpenAI 兼容；stream=true 走 SSE delta 分片。
func (h *Handler) completions(w http.ResponseWriter, r *http.Request) {
	if !h.auth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"message": "Invalid gateway API key", "code": "gateway_auth_failed"}})
		return
	}
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "bad json"}})
		return
	}
	content := ""
	if len(req.Messages) > 0 {
		content = h.replyFor(req.Messages[len(req.Messages)-1].Content)
	}

	if !req.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "chatcmpl-echod", "object": "chat.completion", "model": req.Model,
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": content},
			}},
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	chunks := h.cfg.Chunks
	if chunks < 2 {
		chunks = 3
	}
	runes := []rune(content)
	per := (len(runes) + chunks - 1) / chunks
	if per == 0 {
		per = 1
	}
	sendChunk := func(delta string, finish *string) {
		chunk := map[string]any{
			"id": "chatcmpl-echod", "object": "chat.completion.chunk", "model": "echod",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": finish}},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	for i := 0; i < len(runes); i += per {
		end := i + per
		if end > len(runes) {
			end = len(runes)
		}
		sendChunk(string(runes[i:end]), nil)
		select {
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Millisecond):
		}
	}
	stop := "stop"
	if len(runes) == 0 {
		sendChunk("", &stop)
	}
	sendChunk("", &stop)
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}
