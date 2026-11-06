package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dddpeter/agent-nexus/internal/registry"
	"github.com/dddpeter/agent-nexus/internal/repo"
	"github.com/dddpeter/agent-nexus/internal/session/private"
)

// 私聊聊天页（M2：浏览器双向聊天）。
// 服务端渲染 + htmx 局部刷新；接收侧原生 EventSource 订阅 /api/stream 过滤本会话事件触发刷新；
// 流式发送为 fetch 读 SSE 分块渐进渲染（同步发送为纯 htmx 表单，无 JS 也可用）。

func (s *Server) mountChat(r chi.Router) {
	r.Get("/chat", s.pageChat)
	r.Post("/ui/chat/session", s.createChatSession)
	r.Post("/ui/chat/send", s.chatSend)
	r.Get("/ui/chat/messages", s.chatMessages)
	r.Get("/ui/chat/sessions", s.partialChatSessions)
}

// chatSessionItem 侧栏会话项。
type chatSessionItem struct {
	ID        string
	AgentID   string
	AgentName string
	CreatedAt time.Time
}

// toSessionItems 转换侧栏项（agent 名取当前注册表，软删后沿用 id）。
func (s *Server) toSessionItems(ctx context.Context, sessions []repo.Session) []chatSessionItem {
	items := make([]chatSessionItem, 0, len(sessions))
	for _, sess := range sessions {
		if sess.AgentID == nil {
			continue
		}
		name := *sess.AgentID
		if ag, err := s.deps.Registry.Get(ctx, *sess.AgentID); err == nil {
			name = ag.Name
		}
		items = append(items, chatSessionItem{
			ID: sess.ID, AgentID: *sess.AgentID, AgentName: name, CreatedAt: sess.CreatedAt,
		})
	}
	return items
}

// chatVM 聊天页数据。
type chatVM struct {
	Agents   []agentDTO
	Session  *repo.Session
	Agent    *agentDTO
	Messages []repo.Message    // 当前会话历史（首屏服务端渲染；之后走局部刷新）
	Sessions []chatSessionItem // 侧栏最近会话
}

// pageChat GET /chat?session=…&agent=… —— 聊天页。
// agent 参数：直接为该 agent 建会话（向导/详情页「开始私聊」入口）。
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
	if recent, err := s.deps.Store.Sessions().ListRecent(r.Context(), 20); err == nil {
		vm.Sessions = s.toSessionItems(r.Context(), recent)
	}
	if sid := r.URL.Query().Get("session"); sid != "" {
		if sess, err := s.deps.Store.Sessions().Get(r.Context(), sid); err == nil {
			vm.Session = &sess
		}
	} else if agentID := r.URL.Query().Get("agent"); agentID != "" && s.deps.Router != nil {
		if sess, err := s.deps.Router.CreateSession(r.Context(), agentID); err == nil {
			// 建会话后重定向到规范 URL（侧栏与面板一致）
			http.Redirect(w, r, "/chat?session="+sess.ID, http.StatusSeeOther)
			return
		}
	}
	if vm.Session != nil {
		if msgs, err := s.deps.Store.Messages().ListBySession(r.Context(), vm.Session.ID); err == nil {
			vm.Messages = msgs
		}
	}
	if vm.Session != nil && vm.Session.AgentID != nil {
		if ag, err := s.deps.Registry.Get(r.Context(), *vm.Session.AgentID); err == nil {
			dto := s.toDTO(ag)
			vm.Agent = &dto
		} else {
			// agent 已软删：占位展示（历史会话仍可读）
			dto := agentDTO{ID: *vm.Session.AgentID, Name: *vm.Session.AgentID, Status: repo.StatusRemoved}
			vm.Agent = &dto
		}
	}
	renderPageSet(tplChat, w, http.StatusOK, pageVM{Active: "chat", Title: "私聊", V: vm, Meta: s.metaOf(r)})
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

// chatMessagesVM 消息局部数据。
type chatMessagesVM struct {
	Messages  []repo.Message
	Flash     string
	SessionID string
}

// partialChatSessions GET /ui/chat/sessions?current= —— 侧栏会话局部。
func (s *Server) partialChatSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.deps.Store.Sessions().ListRecent(r.Context(), 20)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	renderPartial(w, http.StatusOK, "chat_sessions", map[string]any{
		"Sessions":  s.toSessionItems(r.Context(), sessions),
		"CurrentID": r.URL.Query().Get("current"),
	})
}
