// Package echodhttp 提供内置测试用假 agent "echod" 的 HTTP 形态
// （hermes 范式 fake gateway：/health、/v1/sessions、/v1/sessions/{id}/messages、stream）。
package echodhttp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
	mux.HandleFunc("POST /v1/sessions", h.createSession)
	mux.HandleFunc("POST /v1/sessions/{id}/messages", h.sendMessage)
	mux.HandleFunc("POST /v1/sessions/{id}/stream", h.streamMessage)
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

type createSessionResp struct {
	SessionID string `json:"session_id"`
}

func (h *Handler) createSession(w http.ResponseWriter, r *http.Request) {
	if !h.auth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "bad api key"})
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
	writeJSON(w, http.StatusCreated, createSessionResp{SessionID: sid})
}

type sendMessageReq struct {
	Content string `json:"content"`
	// Session 显式续话（可省略，URL 已带）
	Session string `json:"session,omitempty"`
}

type sendMessageResp struct {
	Reply   string `json:"reply"`
	Session string `json:"session"`
	Done    bool   `json:"done"`
}

func (h *Handler) sendMessage(w http.ResponseWriter, r *http.Request) {
	if !h.auth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "bad api key"})
		return
	}
	var req sendMessageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad json: " + err.Error()})
		return
	}
	sid := r.PathValue("id")
	h.mu.Lock()
	known := h.sessions[sid]
	h.mu.Unlock()
	if !known {
		// agent 侧会话丢失（F2.4 场景）：返回 410 供 adapter 翻译为 ErrSessionLost
		writeJSON(w, http.StatusGone, map[string]any{"error": "session expired on agent side", "session": sid})
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
		content := h.replyFor(req.Content)
		writeJSON(w, http.StatusOK, sendMessageResp{Reply: content, Session: sid, Done: true})
	case "fixed":
		content := h.replyFor(req.Content)
		writeJSON(w, http.StatusOK, sendMessageResp{Reply: content, Session: sid, Done: true})
	default: // echo
		writeJSON(w, http.StatusOK, sendMessageResp{Reply: req.Content, Session: sid, Done: true})
	}
}

func (h *Handler) replyFor(content string) string {
	if h.cfg.Reply != "" {
		return h.cfg.Reply
	}
	return content
}

type streamLine struct {
	Type    string `json:"type"`
	Data    string `json:"data"`
	Session string `json:"session,omitempty"`
}

func (h *Handler) streamMessage(w http.ResponseWriter, r *http.Request) {
	if !h.auth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "bad api key"})
		return
	}
	var req sendMessageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
		return
	}
	sid := r.PathValue("id")
	content := h.replyFor(req.Content)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
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
	for i := 0; i < len(runes); i += per {
		end := i + per
		if end > len(runes) {
			end = len(runes)
		}
		b, _ := json.Marshal(streamLine{Type: "delta", Data: string(runes[i:end])})
		fmt.Fprintln(w, string(b))
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Millisecond):
		}
	}
	b, _ := json.Marshal(streamLine{Type: "final", Data: content, Session: sid})
	fmt.Fprintln(w, strings.TrimSpace(string(b)))
	flusher.Flush()
}
