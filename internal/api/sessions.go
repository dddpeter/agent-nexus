package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/dddpeter/agent-nexus/internal/repo"
	"github.com/dddpeter/agent-nexus/internal/session/private"
)

// sessionDTO 会话视图。
type sessionDTO struct {
	ID          string  `json:"id"`
	AgentID     string  `json:"agent_id"`
	Kind        string  `json:"kind"`
	ExternalRef *string `json:"external_ref,omitempty"`
	CreatedAt   string  `json:"created_at"`
	ClosedAt    *string `json:"closed_at,omitempty"`
}

func toSessionDTO(s repo.Session) sessionDTO {
	dto := sessionDTO{ID: s.ID, Kind: s.Kind, ExternalRef: s.ExternalRef, CreatedAt: s.CreatedAt.UTC().Format(millis)}
	if s.AgentID != nil {
		dto.AgentID = *s.AgentID
	}
	if s.ClosedAt != nil {
		c := s.ClosedAt.UTC().Format(millis)
		dto.ClosedAt = &c
	}
	return dto
}

const millis = "2006-01-02T15:04:05.000Z07:00"

// messageDTO 消息视图。
type messageDTO struct {
	ID         int64   `json:"id"`
	SenderType string  `json:"sender_type"`
	SenderID   string  `json:"sender_id"`
	Content    string  `json:"content"`
	ReplyTo    *string `json:"reply_to,omitempty"`
	CreatedAt  string  `json:"created_at"`
}

func toMessageDTO(m repo.Message) messageDTO {
	return messageDTO{ID: m.ID, SenderType: m.SenderType, SenderID: m.SenderID,
		Content: m.Content, ReplyTo: m.ReplyTo, CreatedAt: m.CreatedAt.UTC().Format(millis)}
}

// POST /api/sessions 建私聊会话 {agent_id}（FR2.1）。
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AgentID == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "body 需要 {agent_id}")
		return
	}
	sess, err := s.deps.Router.CreateSession(r.Context(), req.AgentID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			mapWriteError(w, r, err)
			return
		}
		writeError(w, r, http.StatusBadRequest, "bad_session", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toSessionDTO(sess))
}

// GET /api/sessions/{id} 会话详情。
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.deps.Store.Sessions().Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toSessionDTO(sess))
}

// GET /api/sessions/{id}/messages 历史消息（存档检索）。
func (s *Server) handleSessionMessages(w http.ResponseWriter, r *http.Request) {
	msgs, err := s.deps.Store.Messages().ListBySession(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	out := make([]messageDTO, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toMessageDTO(m))
	}
	writeJSON(w, http.StatusOK, out)
}

// sendMessageResp 同步发送响应。
type sendMessageResp struct {
	Message  messageDTO            `json:"message"`
	Reply    *messageDTO           `json:"reply,omitempty"`
	TimedOut bool                  `json:"timed_out"`
	Async    *private.AsyncReceipt `json:"async,omitempty"`
}

// POST /api/sessions/{id}/messages 发消息（FR2.2：mode=sync|stream|async）。
//   - sync → 200 {message, reply}
//   - async → 202 {message, async:{type:"accepted",task_id,eta_hint}}
//   - stream → text/event-stream（delta/final/error，final 内容同时落库）
func (s *Server) handleSessionSend(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	var req struct {
		Content string `json:"content"`
		Mode    string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Content == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "body 需要 {content}")
		return
	}
	mode := private.Mode(req.Mode)
	if mode == "" {
		mode = private.ModeSync
	}

	if mode == private.ModeStream {
		s.streamSessionSend(w, r, sessionID, req.Content)
		return
	}

	res, err := s.deps.Router.SendMessage(r.Context(), sessionID, req.Content, mode)
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrNotFound):
			mapWriteError(w, r, err) // agent 已注销（软删）→ 404（C-F1-01）
		case errors.Is(err, private.ErrAgentUnavailable):
			writeError(w, r, http.StatusConflict, "agent_unavailable", err.Error()) // F2.4：offline/disabled 409 不排队
		case errors.Is(err, private.ErrSessionClosed):
			writeError(w, r, http.StatusConflict, "session_closed", err.Error())
		case errors.Is(err, private.ErrTimeout):
			// 超时占位已落库：200 + timed_out（session 保留可重发）
			writeJSON(w, http.StatusOK, sendMessageResp{
				Message: toMessageDTO(derefMsg(res.Reply)), TimedOut: true,
			})
		default:
			writeError(w, r, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}

	status := http.StatusOK
	if res.Async != nil {
		status = http.StatusAccepted
	}
	resp := sendMessageResp{}
	if res.Reply != nil {
		resp.Reply = ptrMessage(toMessageDTO(*res.Reply))
		resp.Message = toMessageDTO(*res.Reply) // 占位字段兼容（超时占位场景）
	}
	if res.Async != nil {
		resp.Async = res.Async
	}
	writeJSON(w, status, resp)
}

// streamSessionSend 流式模式：SSE 转译 Router 事件。
func (s *Server) streamSessionSend(w http.ResponseWriter, r *http.Request, sessionID, content string) {
	ch, cancel, err := s.deps.Router.StreamMessage(r.Context(), sessionID, content)
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrNotFound):
			mapWriteError(w, r, err)
		case errors.Is(err, private.ErrAgentUnavailable):
			writeError(w, r, http.StatusConflict, "agent_unavailable", err.Error())
		default:
			writeError(w, r, http.StatusBadRequest, "bad_stream", err.Error())
		}
		return
	}
	defer cancel()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: {\"type\":%q,\"data\":%q}\n\n", ev.Type, ev.Type, ev.Data)
			flusher.Flush()
			if ev.Type == "final" || ev.Type == "error" {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// GET /api/sessions?limit=20 最近会话（聊天侧栏数据源；跨 agent）。
func (s *Server) handleRecentSessions(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	sessions, err := s.deps.Store.Sessions().ListRecent(r.Context(), limit)
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	out := make([]sessionDTO, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, toSessionDTO(sess))
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/agents/{id}/sessions agent 的历史会话（软删后仍可查，C-F1-05）。
func (s *Server) handleAgentSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.deps.Store.Sessions().ListByAgent(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	out := make([]sessionDTO, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, toSessionDTO(sess))
	}
	writeJSON(w, http.StatusOK, out)
}

func derefMsg(m *repo.Message) repo.Message {
	if m == nil {
		return repo.Message{}
	}
	return *m
}

func ptrMessage(m messageDTO) *messageDTO { return &m }
