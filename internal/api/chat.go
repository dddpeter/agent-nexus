package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/dddpeter/agent-nexus/internal/registry"
	"github.com/dddpeter/agent-nexus/internal/repo"
	"github.com/dddpeter/agent-nexus/internal/session/private"
)

// 私聊聊天页（M2：浏览器双向聊天）。
// 服务端渲染 + htmx 局部刷新；接收侧用原生 EventSource 订阅 /api/stream 过滤本会话事件触发刷新；
// 流式发送为 fetch 读 SSE 分块渐进渲染（可降级：同步发送纯 htmx 表单，无 JS 也可用）。

func (s *Server) mountChat(r chi.Router) {
	r.Get("/chat", s.pageChat)
	r.Post("/ui/chat/session", s.createChatSession)
	r.Post("/ui/chat/send", s.chatSend)
	r.Get("/ui/chat/messages", s.chatMessages)
}

// chatVM 聊天页数据。
type chatVM struct {
	Agents   []agentDTO
	Session  *repo.Session
	Agent    *agentDTO
	Messages []repo.Message
	Flash    string
}

// pageChat GET /chat?session= —— 聊天页（无会话时展示 agent 选择器）。
func (s *Server) pageChat(w http.ResponseWriter, r *http.Request) {
	vm := chatVM{}
	agents, err := s.deps.Registry.List(r.Context(), registry.StatusFilter{})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for _, a := range agents {
		vm.Agents = append(vm.Agents, s.toDTO(a))
	}
	if sid := r.URL.Query().Get("session"); sid != "" && s.deps.Router != nil {
		if sess, err := s.deps.Store.Sessions().Get(r.Context(), sid); err == nil {
			vm.Session = &sess
			if ag, err := s.deps.Registry.Get(r.Context(), *sess.AgentID); err == nil {
				dto := s.toDTO(ag)
				vm.Agent = &dto
			}
			if msgs, err := s.deps.Store.Messages().ListBySession(r.Context(), sid); err == nil {
				vm.Messages = msgs
			}
		}
	}
	renderChatPage(w, http.StatusOK, vm)
}

// createChatSession POST /ui/chat/session（表单 agent_id）→ 303 到 /chat?session=。
func (s *Server) createChatSession(w http.ResponseWriter, r *http.Request) {
	if s.deps.Router == nil {
		writeError(w, r, http.StatusServiceUnavailable, "no_router", "session router unavailable")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	agentID := r.FormValue("agent_id")
	if agentID == "" {
		http.Redirect(w, r, "/chat", http.StatusSeeOther)
		return
	}
	sess, err := s.deps.Router.CreateSession(r.Context(), agentID)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_session", err.Error())
		return
	}
	http.Redirect(w, r, "/chat?session="+sess.ID, http.StatusSeeOther)
}

// chatSend POST /ui/chat/send（表单 session_id, content）→ 同步发送，返回消息局部。
func (s *Server) chatSend(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	sessionID := r.FormValue("session_id")
	content := r.FormValue("content")
	flash := ""
	if content == "" {
		flash = "消息内容为空"
	} else {
		_, err := s.deps.Router.SendMessage(r.Context(), sessionID, content, private.ModeSync)
		switch {
		case err == nil:
		case errors.Is(err, private.ErrTimeout):
			flash = "回复超时：已留占位消息，会话保留可重发"
		default:
			flash = "发送失败：" + err.Error()
		}
	}
	s.renderChatMessages(w, r, sessionID, flash)
}

// chatMessages GET /ui/chat/messages?session= —— 消息局部（轮询/操作后刷新）。
func (s *Server) chatMessages(w http.ResponseWriter, r *http.Request) {
	s.renderChatMessages(w, r, r.URL.Query().Get("session"), "")
}

func (s *Server) renderChatMessages(w http.ResponseWriter, r *http.Request, sessionID, flash string) {
	msgs, err := s.deps.Store.Messages().ListBySession(r.Context(), sessionID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	renderPartial(w, http.StatusOK, "chat_messages", chatMessagesVM{Messages: msgs, Flash: flash, SessionID: sessionID})
}
