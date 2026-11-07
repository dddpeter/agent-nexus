package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/dddpeter/agent-nexus/internal/arbiter"
	"github.com/dddpeter/agent-nexus/internal/repo"
	"github.com/dddpeter/agent-nexus/internal/session/group"
)

// groupMemberDTO 群成员 DTO（含展示名，前端 @ 提示用）。
type groupMemberDTO struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	Name           string `json:"name,omitempty"`
	Trigger        string `json:"trigger,omitempty"`
	Priority       int    `json:"priority,omitempty"`
	MaxTurns       int    `json:"max_turns,omitempty"`
	InterruptLevel int    `json:"interrupt_level,omitempty"`
	Profile        string `json:"profile,omitempty"`
}

type groupDTO struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	Members      []groupMemberDTO `json:"members"`
	MaxTurns     int              `json:"max_turns_per_topic"`
	SilentThresh int              `json:"human_silent_threshold"`
	CreatedAt    string           `json:"created_at,omitempty"`
}

type groupStateDTO struct {
	GroupID        string         `json:"group_id"`
	TopicID        string         `json:"topic_id"`
	Frozen         bool           `json:"frozen"`
	TurnOccupied   bool           `json:"turn_occupied"`
	CurrentSpeaker string         `json:"current_speaker,omitempty"`
	TurnsUsed      map[string]int `json:"turns_used"`
	HumanSilent    int            `json:"human_silent_rounds"`
	MaxTurns       int            `json:"max_turns_per_topic"`
	SilentThresh   int            `json:"human_silent_threshold"`
}

// groupMemberDefs 请求成员 → arbiter.Member（ID/Type 直传，名称由协调器解析）。
type groupMemberReq struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	Trigger        string `json:"trigger"`
	Priority       int    `json:"priority"`
	MaxTurns       int    `json:"max_turns"`
	InterruptLevel int    `json:"interrupt_level"`
	Profile        string `json:"profile"`
}

// toGroupDTO 组装群 DTO（含解析后的成员展示名）。
func (s *Server) toGroupDTO(g repo.Group) groupDTO {
	var defs []groupMemberDTO
	_ = json.Unmarshal([]byte(g.MembersJSON), &defs)
	var cfg map[string]int
	_ = json.Unmarshal([]byte(g.ArbiterConfigJSON), &cfg)
	return groupDTO{
		ID: g.ID, Name: g.Name, Members: defs,
		MaxTurns: cfg["max_turns_per_topic"], SilentThresh: cfg["human_silent_threshold"],
	}
}

// memberReqsToArbiter 请求成员 → arbiter.Member。
func memberReqsToArbiter(reqs []groupMemberReq) []arbiter.Member {
	out := make([]arbiter.Member, len(reqs))
	for i, r := range reqs {
		out[i] = arbiter.Member{ID: r.ID, Type: r.Type, Trigger: r.Trigger, Priority: r.Priority,
			MaxTurns: r.MaxTurns, InterruptLevel: r.InterruptLevel, Profile: r.Profile}
	}
	return out
}

// handleCreateGroup POST /api/groups 建群（FR3.1）。
func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	if s.deps.Groups == nil {
		writeError(w, r, http.StatusNotImplemented, "not_available", "群聊协调器未启用")
		return
	}
	var req struct {
		Name                 string           `json:"name"`
		Members              []groupMemberReq `json:"members"`
		MaxTurnsPerTopic     int              `json:"max_turns_per_topic"`
		HumanSilentThreshold int              `json:"human_silent_threshold"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "body 需要 {name, members[]}")
		return
	}
	g, err := s.deps.Groups.CreateGroup(r.Context(), req.Name, memberReqsToArbiter(req.Members), req.MaxTurnsPerTopic, req.HumanSilentThreshold)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.toGroupDTO(g))
}

// handleListGroups GET /api/groups。
func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	if s.deps.Groups == nil {
		writeJSON(w, http.StatusOK, []groupDTO{})
		return
	}
	groups, err := s.deps.Store.Groups().List(r.Context())
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	out := make([]groupDTO, 0, len(groups))
	for _, g := range groups {
		out = append(out, s.toGroupDTO(g))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetGroup GET /api/groups/{id}：群配置 + 运行态（冻结/预算/回合）。
func (s *Server) handleGetGroup(w http.ResponseWriter, r *http.Request) {
	if s.deps.Groups == nil {
		writeError(w, r, http.StatusNotImplemented, "not_available", "群聊协调器未启用")
		return
	}
	id := chi.URLParam(r, "id")
	g, err := s.deps.Store.Groups().Get(r.Context(), id)
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	st, err := s.deps.Groups.State(r.Context(), id)
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group": s.toGroupDTO(g),
		"state": groupStateDTO{
			GroupID: st.GroupID, TopicID: st.TopicID, Frozen: st.Frozen,
			TurnOccupied: st.TurnOccupied, CurrentSpeaker: st.CurrentSpeaker,
			TurnsUsed: st.TurnsUsed, HumanSilent: st.HumanSilentRounds,
			MaxTurns: st.MaxTurnsPerTopic, SilentThresh: st.HumanSilentThreshold,
		},
	})
}

// handleGroupSend POST /api/groups/{id}/messages {content, sender_id?} 人发言（FR3.2/3.3）。
func (s *Server) handleGroupSend(w http.ResponseWriter, r *http.Request) {
	if s.deps.Groups == nil {
		writeError(w, r, http.StatusNotImplemented, "not_available", "群聊协调器未启用")
		return
	}
	var req struct {
		Content  string `json:"content"`
		SenderID string `json:"sender_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Content == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "body 需要 {content}")
		return
	}
	if req.SenderID == "" {
		req.SenderID = "user"
	}
	res, err := s.deps.Groups.SendHuman(r.Context(), chi.URLParam(r, "id"), req.Content, req.SenderID)
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	resp := struct {
		Message  *messageDTO            `json:"message"`
		Reply    *messageDTO            `json:"reply,omitempty"`
		Deferred []group.DeferredRecord `json:"deferred,omitempty"`
		Frozen   bool                   `json:"frozen"`
	}{Frozen: res.Frozen}
	if res.Human != nil {
		dto := toMessageDTO(*res.Human)
		resp.Message = &dto
	}
	if res.Reply != nil {
		dto := toMessageDTO(*res.Reply)
		resp.Reply = &dto
	}
	resp.Deferred = res.Deferred
	writeJSON(w, http.StatusOK, resp)
}

// handleGroupMessages GET /api/groups/{id}/messages 群消息历史。
func (s *Server) handleGroupMessages(w http.ResponseWriter, r *http.Request) {
	groupID := chi.URLParam(r, "id")
	g, err := s.deps.Store.Groups().Get(r.Context(), groupID)
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	sess, err := s.deps.Groups.SessionOf(r.Context(), g.ID)
	if err != nil || sess == "" {
		writeJSON(w, http.StatusOK, []messageDTO{})
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	msgs, err := s.deps.Store.Messages().RecentBySession(r.Context(), sess, limit)
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

// handleGroupUnfreeze POST /api/groups/{id}/unfreeze 主持人解冻（FR3.4）。
func (s *Server) handleGroupUnfreeze(w http.ResponseWriter, r *http.Request) {
	if s.deps.Groups == nil {
		writeError(w, r, http.StatusNotImplemented, "not_available", "群聊协调器未启用")
		return
	}
	if err := s.deps.Groups.Unfreeze(r.Context(), chi.URLParam(r, "id")); err != nil {
		mapWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleGroupTopic POST /api/groups/{id}/topic {topic_id} 显式话题切换（FR3.4）。
func (s *Server) handleGroupTopic(w http.ResponseWriter, r *http.Request) {
	if s.deps.Groups == nil {
		writeError(w, r, http.StatusNotImplemented, "not_available", "群聊协调器未启用")
		return
	}
	var req struct {
		TopicID string `json:"topic_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TopicID == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "body 需要 {topic_id}")
		return
	}
	if err := s.deps.Groups.ResetTopic(r.Context(), chi.URLParam(r, "id"), req.TopicID); err != nil {
		mapWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleGroupConfig PUT /api/groups/{id}/config 阈值热更新（FR3.7）。
func (s *Server) handleGroupConfig(w http.ResponseWriter, r *http.Request) {
	if s.deps.Groups == nil {
		writeError(w, r, http.StatusNotImplemented, "not_available", "群聊协调器未启用")
		return
	}
	var req struct {
		MaxTurnsPerTopic     int `json:"max_turns_per_topic"`
		HumanSilentThreshold int `json:"human_silent_threshold"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "bad json")
		return
	}
	st, err := s.deps.Groups.UpdateConfig(r.Context(), chi.URLParam(r, "id"), req.MaxTurnsPerTopic, req.HumanSilentThreshold)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			mapWriteError(w, r, err)
			return
		}
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, groupStateDTO{
		GroupID: st.GroupID, TopicID: st.TopicID, Frozen: st.Frozen,
		TurnOccupied: st.TurnOccupied, CurrentSpeaker: st.CurrentSpeaker,
		TurnsUsed: st.TurnsUsed, HumanSilent: st.HumanSilentRounds,
		MaxTurns: st.MaxTurnsPerTopic, SilentThresh: st.HumanSilentThreshold,
	})
}
