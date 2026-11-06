package api

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/registry"
	"github.com/dddpeter/agent-nexus/web"
)

// UI 路由（任务 11 范围：agent 列表 + 注册向导，无其他页面）。
// 走服务端渲染 + htmx 局部刷新；鉴权沿用全局中间件链（本地无 token 模式可直接访问）。

var (
	tplList   *template.Template // layout + agents
	tplWizard *template.Template // layout + wizard
)

var tplFuncs = template.FuncMap{
	"formatTime": func(t *time.Time) string {
		if t == nil {
			return "-"
		}
		return t.Format("15:04:05")
	},
	"maskedConfig": func(cfg map[string]any) string {
		if len(cfg) == 0 {
			return "{}"
		}
		raw, err := json.Marshal(cfg)
		if err != nil {
			return "{}"
		}
		s := string(raw)
		if len(s) > 120 {
			s = s[:120] + "…"
		}
		return s
	},
}

func init() {
	tplList = template.Must(template.New("list").Funcs(tplFuncs).
		ParseFS(web.Templates, "templates/layout.gohtml", "templates/agents.gohtml"))
	tplWizard = template.Must(template.New("wizard").Funcs(tplFuncs).
		ParseFS(web.Templates, "templates/layout.gohtml", "templates/wizard.gohtml"))
}

// pageVM 布局模板数据：Active 高亮导航，V 为内容模板数据。
type pageVM struct {
	Active string
	V      any
}

func renderListPage(w http.ResponseWriter, status int, vm any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = tplList.ExecuteTemplate(w, "layout", pageVM{Active: "agents", V: vm})
}

func renderWizardPage(w http.ResponseWriter, status int, vm any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = tplWizard.ExecuteTemplate(w, "layout", pageVM{Active: "wizard", V: vm})
}

func renderPartial(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	// config_form/test_result/submit_result 定义于 wizard 模板集
	_ = tplWizard.ExecuteTemplate(w, name, data)
	// agents_table 定义于 list 模板集
	_ = tplList.ExecuteTemplate(w, name, data)
}

// mountWeb 挂载静态资源与 UI 页面。
func (s *Server) mountWeb(r chi.Router) {
	// 静态资源
	sub, _ := fs.Sub(web.Assets, "assets")
	r.Handle("/assets/*", http.StripPrefix("/assets/", http.FileServer(http.FS(sub))))

	// 页面
	r.Get("/", s.pageAgentList)
	r.Get("/agents/table", s.partialAgentsTable)
	r.Get("/agents/new", s.pageWizard)
	r.Get("/agents/new/config-form", s.partialConfigForm)
	r.Post("/agents/new/test", s.actionWizardTest)
	r.Post("/agents/new/submit", s.actionWizardSubmit)

	// 列表行操作
	r.Post("/ui/agents/{id}/test", s.actionAgentTest)
	r.Post("/ui/agents/{id}/disable", s.actionAgentDisable)
	r.Post("/ui/agents/{id}/enable", s.actionAgentEnable)
	r.Post("/ui/agents/{id}/delete", s.actionAgentDelete)
}

// listVM 列表页/局部视图数据。
type listVM struct {
	Agents []agentDTO
	Flash  string
}

// pageAgentList GET / —— agent 列表页。
func (s *Server) pageAgentList(w http.ResponseWriter, r *http.Request) {
	agents, err := s.deps.Registry.List(r.Context(), registry.StatusFilter{})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	dtos := make([]agentDTO, 0, len(agents))
	for _, a := range agents {
		dtos = append(dtos, s.toDTO(a))
	}
	renderListPage(w, http.StatusOK, listVM{Agents: dtos})
}

// partialAgentsTable GET /agents/table —— 列表局部（轮询/操作后刷新）。
func (s *Server) partialAgentsTable(w http.ResponseWriter, r *http.Request) {
	s.renderTable(w, r, "")
}

func (s *Server) renderTable(w http.ResponseWriter, r *http.Request, flash string) {
	agents, err := s.deps.Registry.List(r.Context(), registry.StatusFilter{})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	dtos := make([]agentDTO, 0, len(agents))
	for _, a := range agents {
		dtos = append(dtos, s.toDTO(a))
	}
	renderPartial(w, http.StatusOK, "agents_table", listVM{Agents: dtos, Flash: flash})
}

// pageWizard GET /agents/new —— 注册向导。
func (s *Server) pageWizard(w http.ResponseWriter, _ *http.Request) {
	type adapterItem struct{ Name string }
	items := make([]adapterItem, 0, 8)
	for _, f := range s.deps.Registry.Factories() {
		items = append(items, adapterItem{Name: f.Name()})
	}
	// 首个 adapter 的表单直接内联渲染
	var initial any
	if factories := s.deps.Registry.Factories(); len(factories) > 0 {
		initial = schemaVM{factories[0].ConfigSchema()}
	}
	renderWizardPage(w, http.StatusOK, map[string]any{
		"Adapters": items, "InitialSchema": initial,
	})
}

// schemaVM config_form 模板数据。
type schemaVM struct{ adapter.Schema }

// partialConfigForm GET /agents/new/config-form?adapter=echod —— 动态表单局部。
func (s *Server) partialConfigForm(w http.ResponseWriter, r *http.Request) {
	f := s.deps.Registry.Factory(r.URL.Query().Get("adapter"))
	if f == nil {
		http.Error(w, "unknown adapter", http.StatusBadRequest)
		return
	}
	renderPartial(w, http.StatusOK, "config_form", schemaVM{f.ConfigSchema()})
}

// wizardSpecFromForm 从向导表单解析注册规格（cfg_* → config，cap_* → capabilities）。
func (s *Server) wizardSpecFromForm(r *http.Request) (name string, spec registry.AgentSpec, sendMsg bool, err error) {
	if err := r.ParseForm(); err != nil {
		return "", spec, false, err
	}
	name = strings.TrimSpace(r.FormValue("name"))
	adapterName := r.FormValue("adapter")
	cfg := map[string]any{}
	for key, vals := range r.Form {
		if !strings.HasPrefix(key, "cfg_") || len(vals) == 0 {
			continue
		}
		field := strings.TrimPrefix(key, "cfg_")
		v := strings.TrimSpace(vals[0])
		if v == "" {
			continue // 留空 = 不设置（密钥保持/用默认值）
		}
		if f := s.deps.Registry.Factory(adapterName); f != nil {
			if sf, ok := f.ConfigSchema().Field(field); ok && sf.Type == adapter.FieldList {
				cfg[field] = strings.Fields(v)
				continue
			}
		}
		cfg[field] = v
	}
	spec = registry.AgentSpec{Name: name, Adapter: adapterName, Config: cfg}

	// 能力覆盖
	var caps adapter.Capabilities
	hasCaps := false
	if v := strings.TrimSpace(r.FormValue("cap_max_concurrency")); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			caps.MaxConcurrency, hasCaps = n, true
		}
	}
	if v := strings.TrimSpace(r.FormValue("cap_interrupt_level")); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n >= 0 && n <= 2 {
			caps.InterruptLevel, hasCaps = n, true
		}
	}
	if v := strings.TrimSpace(r.FormValue("cap_typical_latency_s")); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			caps.TypicalLatency, hasCaps = adapter.Duration(time.Duration(n)*time.Second), true
		}
	}
	if hasCaps {
		spec.Capabilities = &caps
	}
	sendMsg = r.FormValue("send_message") == "on" || r.FormValue("send_message") == "true"
	return name, spec, sendMsg, nil
}

// actionWizardTest POST /agents/new/test —— 保存前连通性测试。
func (s *Server) actionWizardTest(w http.ResponseWriter, r *http.Request) {
	_, spec, sendMsg, err := s.wizardSpecFromForm(r)
	if err != nil {
		renderPartial(w, http.StatusBadRequest, "test_result", testResultVM{OK: false, Error: err.Error()})
		return
	}
	res := s.deps.Registry.TestConnection(r.Context(), spec, sendMsg)
	renderPartial(w, http.StatusOK, "test_result", testResultVM{
		OK: res.OK, DurationMs: fmt.Sprintf("%.1f", res.DurationMs),
		Error: res.Err, SentMessage: sendMsg,
	})
}

type testResultVM struct {
	OK          bool
	DurationMs  string
	Error       string
	SentMessage bool
}

// actionWizardSubmit POST /agents/new/submit —— 提交注册。
func (s *Server) actionWizardSubmit(w http.ResponseWriter, r *http.Request) {
	name, spec, _, err := s.wizardSpecFromForm(r)
	if err != nil {
		renderPartial(w, http.StatusOK, "submit_result", submitResultVM{Error: err.Error()})
		return
	}
	agent, err := s.deps.Registry.Register(r.Context(), spec)
	if err != nil {
		renderPartial(w, http.StatusOK, "submit_result", submitResultVM{Error: err.Error()})
		return
	}
	renderPartial(w, http.StatusOK, "submit_result", submitResultVM{OK: true, Name: name, Status: agent.Status})
}

type submitResultVM struct {
	OK     bool
	Name   string
	Status string
	Error  string
}

// ---------- 列表行操作 ----------

func (s *Server) actionAgentTest(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	agent, err := s.deps.Registry.Get(r.Context(), id)
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	res := s.deps.Registry.TestConnection(r.Context(),
		registry.AgentSpec{Adapter: agent.Adapter, Config: agent.Config}, false)
	flash := fmt.Sprintf("连通性测试 %s：%.1f ms", map[bool]string{true: "通过", false: "失败"}[res.OK], res.DurationMs)
	if !res.OK {
		flash += " — " + res.Err
	}
	s.renderTable(w, r, flash)
}

func (s *Server) actionAgentDisable(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Registry.Disable(chi.URLParam(r, "id")); err != nil {
		mapWriteError(w, r, err)
		return
	}
	s.renderTable(w, r, "已停用（enable 可恢复）")
}

func (s *Server) actionAgentEnable(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Registry.Enable(r.Context(), chi.URLParam(r, "id")); err != nil {
		mapWriteError(w, r, err)
		return
	}
	s.renderTable(w, r, "已启用并重新探活")
}

func (s *Server) actionAgentDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Registry.Unregister(r.Context(), chi.URLParam(r, "id")); err != nil {
		mapWriteError(w, r, err)
		return
	}
	s.renderTable(w, r, "已软删（历史会话/任务保留，include_removed 可查）")
}
