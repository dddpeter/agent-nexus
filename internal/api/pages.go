package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

// Agent 详情/配置编辑页（FR1.1b：脱敏回显、变更前连通性测试、停用/软删）。

// detailVM 详情页数据。
type detailVM struct {
	Agent    agentDTO
	SchemaVM configFormVM
	Sessions []chatSessionItem
}

// pageAgentDetail GET /agents/{id}/view。
func (s *Server) pageAgentDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	agent, err := s.deps.Registry.Get(r.Context(), id)
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	f := s.deps.Registry.Factory(agent.Adapter)
	if f == nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "adapter factory missing: "+agent.Adapter)
		return
	}
	// 密钥脱敏后作为表单当前值（留空即沿用旧值）
	values := adapter.MaskConfig(agent.Config, f.ConfigSchema())
	dto := s.toDTO(agent)

	// 最近会话（软删后仍可查）
	sessions, _ := s.deps.Store.Sessions().ListByAgent(r.Context(), id)
	items := s.toSessionItems(r.Context(), sessions)

	renderPageSet(tplDetail, w, http.StatusOK, pageVM{
		Active: "agents", Title: agent.Name,
		V: detailVM{
			Agent:    dto,
			SchemaVM: buildConfigFormVM(f.ConfigSchema(), values),
			Sessions: items,
		},
		Meta: s.metaOf(r),
	})
}

// actionAgentSave POST /ui/agents/{id}/save —— 保存配置/能力变更。
// 变更前自动连通性测试：失败拒绝（force 可强制）；密钥留空沿用旧值。
func (s *Server) actionAgentSave(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		renderPartial(w, http.StatusBadRequest, "save_result", saveResultVM{Error: err.Error()})
		return
	}
	agent, err := s.deps.Registry.Get(r.Context(), id)
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	f := s.deps.Registry.Factory(agent.Adapter)
	if f == nil {
		renderPartial(w, http.StatusInternalServerError, "save_result", saveResultVM{Error: "adapter factory missing"})
		return
	}

	cfg := map[string]any{}
	for key, vals := range r.Form {
		if !strings.HasPrefix(key, "cfg_") || len(vals) == 0 {
			continue
		}
		field := strings.TrimPrefix(key, "cfg_")
		v := strings.TrimSpace(vals[0])
		if v == "" {
			continue // 密钥留空 = 不修改（registry.MergeConfigSecrets 兜底）
		}
		if sf, ok := f.ConfigSchema().Field(field); ok && sf.Type == adapter.FieldList {
			cfg[field] = strings.Fields(v)
			continue
		}
		cfg[field] = v
	}
	force := r.FormValue("force") == "true"

	updated, err := s.deps.Registry.UpdateConfig(r.Context(), id, cfg, force)
	probeFailed := false
	if err != nil {
		renderPartial(w, http.StatusOK, "save_result", saveResultVM{Error: err.Error()})
		return
	}
	if updated.Status == repo.StatusOffline {
		probeFailed = true
	}

	// 能力覆盖（表单含 cap_max_concurrency 时生效）
	if v := strings.TrimSpace(r.FormValue("cap_max_concurrency")); v != "" {
		caps := updated.Capabilities
		if n, e := strconv.Atoi(v); e == nil {
			caps.MaxConcurrency = n
		}
		if v := strings.TrimSpace(r.FormValue("cap_interrupt_level")); v != "" {
			if n, e := strconv.Atoi(v); e == nil && n >= 0 && n <= 2 {
				caps.InterruptLevel = n
			}
		}
		if v := r.FormValue("cap_can_stream"); v == "true" || v == "false" {
			caps.CanStream = v == "true"
		}
		if _, err := s.deps.Registry.UpdateCapabilities(r.Context(), id, caps); err != nil {
			renderPartial(w, http.StatusOK, "save_result", saveResultVM{Error: "配置已保存，能力更新失败：" + err.Error()})
			return
		}
	}
	renderPartial(w, http.StatusOK, "save_result", saveResultVM{OK: true, ProbeFailed: probeFailed})
}

// saveResultVM 保存结果局部。
type saveResultVM struct {
	OK          bool
	ProbeFailed bool
	Error       string
}
