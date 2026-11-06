package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/registry"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

// agentDTO 对外 agent 视图（config 密钥脱敏回显，C-F1-04）。
type agentDTO struct {
	ID              string               `json:"id"`
	Name            string               `json:"name"`
	Adapter         string               `json:"adapter"`
	Status          string               `json:"status"`
	Config          map[string]any       `json:"config"`
	Capabilities    adapter.Capabilities `json:"capabilities"`
	LastHeartbeatAt *time.Time           `json:"last_heartbeat_at"`
	InFlight        int                  `json:"in_flight"`
	Disabled        bool                 `json:"disabled"`
}

func (s *Server) toDTO(a registry.Agent) agentDTO {
	cfg := a.Config
	if f := s.deps.Registry.Factory(a.Adapter); f != nil {
		cfg = adapter.MaskConfig(a.Config, f.ConfigSchema())
	}
	return agentDTO{
		ID: a.ID, Name: a.Name, Adapter: a.Adapter, Status: a.Status,
		Config: cfg, Capabilities: a.Capabilities,
		LastHeartbeatAt: a.LastHeartbeatAt, InFlight: a.InFlight, Disabled: a.Disabled,
	}
}

// adapterDTO 已安装 adapter 清单项（注册向导动态表单数据源，FR1.1a）。
type adapterDTO struct {
	Name   string         `json:"name"`
	Schema adapter.Schema `json:"config_schema"`
}

// GET /api/adapters
func (s *Server) handleListAdapters(w http.ResponseWriter, r *http.Request) {
	out := make([]adapterDTO, 0, 8)
	for _, f := range s.deps.Registry.Factories() {
		out = append(out, adapterDTO{Name: f.Name(), Schema: f.ConfigSchema()})
	}
	writeJSON(w, http.StatusOK, out)
}

type registerAgentReq struct {
	Name         string                `json:"name"`
	Adapter      string                `json:"adapter"`
	Config       map[string]any        `json:"config"`
	Capabilities *adapter.Capabilities `json:"capabilities,omitempty"`
}

// POST /api/agents 注册（探活通过自动 online，U-F1-01 / C-F1-02 ②）。
func (s *Server) handleRegisterAgent(w http.ResponseWriter, r *http.Request) {
	var req registerAgentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_json", "请求体不是合法 JSON: "+err.Error())
		return
	}
	agent, err := s.deps.Registry.Register(r.Context(), registry.AgentSpec{
		Name: req.Name, Adapter: req.Adapter,
		Config: adapter.Config(req.Config), Capabilities: req.Capabilities,
	})
	if err != nil {
		s.writeRegistryError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.toDTO(agent))
}

// GET /api/agents?status=&adapter=&include_removed=
func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := repo.AgentFilter{
		Status:         q.Get("status"),
		Adapter:        q.Get("adapter"),
		IncludeRemoved: q.Get("include_removed") == "true",
	}
	agents, err := s.deps.Registry.List(r.Context(), filter)
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	out := make([]agentDTO, 0, len(agents))
	for _, a := range agents {
		out = append(out, s.toDTO(a))
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/agents/{id} 详情（config 脱敏回显）。
func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	agent, err := s.deps.Registry.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toDTO(agent))
}

type updateAgentReq struct {
	Config       map[string]any        `json:"config"`
	Capabilities *adapter.Capabilities `json:"capabilities,omitempty"`
	Force        bool                  `json:"force"`
}

// PUT /api/agents/{id} 变更前自动连通性测试；失败拒绝（C-F1-03），force 可强制。
func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req updateAgentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_json", "请求体不是合法 JSON: "+err.Error())
		return
	}
	agent, err := s.deps.Registry.UpdateConfig(r.Context(), id, adapter.Config(req.Config), req.Force)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			mapWriteError(w, r, err)
			return
		}
		// 探活失败拒绝变更（含字段错误）
		writeError(w, r, http.StatusBadRequest, "connectivity_test_failed", err.Error())
		return
	}
	// 能力覆盖更新（配置变更成功后）
	if req.Capabilities != nil {
		if _, err := s.deps.Registry.UpdateCapabilities(r.Context(), id, *req.Capabilities); err != nil {
			writeError(w, r, http.StatusBadRequest, "bad_capabilities", err.Error())
			return
		}
		agent, _ = s.deps.Registry.Get(r.Context(), id)
	}
	writeJSON(w, http.StatusOK, s.toDTO(agent))
}

type testAgentReq struct {
	Adapter     string         `json:"adapter,omitempty"`
	Config      map[string]any `json:"config,omitempty"`
	SendMessage bool           `json:"send_message"`
}

// POST /api/agents/{id}/test 已保存 agent 连通性测试（不落库）。
func (s *Server) handleTestAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	agent, err := s.deps.Registry.Get(r.Context(), id)
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	var req testAgentReq
	_ = json.NewDecoder(r.Body).Decode(&req) // body 可空
	spec := registry.AgentSpec{Adapter: agent.Adapter, Config: agent.Config}
	if req.Adapter != "" || req.Config != nil { // body 覆盖（编辑前预检）
		if req.Adapter != "" {
			spec.Adapter = req.Adapter
		}
		if req.Config != nil {
			spec.Config = adapter.Config(req.Config)
		}
	}
	res := s.deps.Registry.TestConnection(r.Context(), spec, req.SendMessage)
	writeJSON(w, http.StatusOK, res)
}

// POST /api/agents/test 按规格测试（注册向导「保存前测试」，不落库）。
func (s *Server) handleTestSpec(w http.ResponseWriter, r *http.Request) {
	var req testAgentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_json", "请求体不是合法 JSON: "+err.Error())
		return
	}
	if req.Adapter == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "adapter 必填")
		return
	}
	spec := registry.AgentSpec{Adapter: req.Adapter, Config: adapter.Config(req.Config)}
	res := s.deps.Registry.TestConnection(r.Context(), spec, req.SendMessage)
	writeJSON(w, http.StatusOK, res)
}

// POST /api/agents/{id}/disable
func (s *Server) handleDisableAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Registry.Disable(chi.URLParam(r, "id")); err != nil {
		mapWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /api/agents/{id}/enable
func (s *Server) handleEnableAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Registry.Enable(r.Context(), chi.URLParam(r, "id")); err != nil {
		mapWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// DELETE /api/agents/{id} 软删（历史保留，FR1.6）。
func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Registry.Unregister(r.Context(), chi.URLParam(r, "id")); err != nil {
		mapWriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeRegistryError 注册/变更错误 → HTTP 语义（F1.4）。
func (s *Server) writeRegistryError(w http.ResponseWriter, r *http.Request, err error) {
	var fe *adapter.FieldError
	switch {
	case errors.Is(err, registry.ErrDuplicateName):
		writeError(w, r, http.StatusConflict, "duplicate_name", err.Error())
	case errors.As(err, &fe):
		writeError(w, r, http.StatusBadRequest, "invalid_config", err.Error())
	case errors.Is(err, registry.ErrBadCapabilities):
		writeError(w, r, http.StatusBadRequest, "invalid_capabilities", err.Error())
	case errors.Is(err, repo.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", err.Error())
	default:
		s.deps.Log.Error("registry error", "err", err)
		writeError(w, r, http.StatusInternalServerError, "internal", err.Error())
	}
}
