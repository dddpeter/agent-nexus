package api

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/registry"
	"github.com/dddpeter/agent-nexus/internal/repo"
	"github.com/dddpeter/agent-nexus/web"
)

// UI 路由：服务端渲染 + htmx 局部刷新（technical-design §1：htmx + embed，单二进制）。
// 鉴权沿用全局中间件链（本地无 token 模式可直接访问，见 TODO#2）。

var (
	tplList   *template.Template // layout + agents
	tplWizard *template.Template // layout + wizard + config_form
	tplChat   *template.Template // layout + chat
	tplDetail *template.Template // layout + detail + config_form
)

var tplFuncs = template.FuncMap{
	"formatTime": func(t *time.Time) string {
		if t == nil {
			return "-"
		}
		return t.Format("15:04:05")
	},
	"formatTimeISO": func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	},
	"formatTimeISOPtr": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"initial": func(name string) string {
		if name == "" {
			return "?"
		}
		r := []rune(strings.ToUpper(name))
		return string(r[0])
	},
	"bubbleClass": func(senderType string) string {
		switch senderType {
		case repo.SenderHuman:
			return "human"
		case repo.SenderAgent:
			return "agent"
		default:
			return "system"
		}
	},
	"bubbleLabel": func(senderType string) string {
		switch senderType {
		case repo.SenderHuman:
			return "你"
		case repo.SenderAgent:
			return "Agent"
		default:
			return "系统"
		}
	},
	"chatMessagesData": func(msgs []repo.Message, flash, sid string) chatMessagesVM {
		return chatMessagesVM{Messages: msgs, Flash: flash, SessionID: sid}
	},
	"chatSessionsData": func(sessions []chatSessionItem, current string) map[string]any {
		return map[string]any{"Sessions": sessions, "CurrentID": current}
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
	base := func(name string, files ...string) *template.Template {
		return template.Must(template.New(name).Funcs(tplFuncs).
			ParseFS(web.Templates, append([]string{"templates/layout.gohtml"}, files...)...))
	}
	tplList = base("list", "templates/agents.gohtml")
	tplWizard = base("wizard", "templates/wizard.gohtml", "templates/config_form.gohtml")
	tplChat = base("chat", "templates/chat.gohtml")
	tplDetail = base("detail", "templates/detail.gohtml", "templates/config_form.gohtml")
}

// pageVM 布局数据：Active 高亮导航，Title 页标题，V 内容模板数据，Meta 顶栏计数。
type pageVM struct {
	Active string
	Title  string
	V      any
	Meta   *topMetaVM
}

// topMetaVM 顶栏 agent 计数。
type topMetaVM struct {
	Total  int
	Online int
}

// listVM 列表页/局部视图数据。
type listVM struct {
	Agents  []agentDTO
	Flash   string
	FlashOK bool
	Query   string  // 搜索关键字（空态提示用）
	Stats   statsVM // 首屏统计条（局部刷新走 /ui/agents/stats）
}

// statsVM 统计条数据。
type statsVM struct {
	Total, Online, Offline, Removed int
}

// configFormVM config_form 模板数据：字段渲染值在 Go 侧预计算，模板零比较逻辑。
type configFormVM struct {
	Fields []fieldVM
}

// fieldVM 单字段渲染视图（当前值 > 默认值；密钥只给 SecretSet 不给值）。
type fieldVM struct {
	Name, Label, Type, Help string
	Required, Secret        bool
	Options                 []string
	Display                 string // text/int/list 的 value
	SecretSet               bool   // 密钥已设置（占位提示用）
	Selected                string // select/bool 的选中值（"true"/"false" 或枚举值）
}

// buildConfigFormVM 由 schema + 当前值（可为 nil/脱敏值）构造渲染视图。
// secret 字段不回显值：有值 → SecretSet=true（占位「留空保持」）。
func buildConfigFormVM(schema adapter.Schema, values map[string]any) configFormVM {
	vm := configFormVM{Fields: make([]fieldVM, 0, len(schema.Fields))}
	for _, f := range schema.Fields {
		fv := fieldVM{
			Name: f.Name, Label: f.Label, Type: string(f.Type), Help: f.Help,
			Required: f.Required, Secret: f.Secret, Options: f.Options,
		}
		cur, hasCur := "", false
		if values != nil {
			if v, ok := values[f.Name]; ok && v != nil {
				cur, hasCur = anyToString(v), true
			}
		}
		def := anyToString(f.Default)
		switch f.Type {
		case adapter.FieldBool:
			pick := def
			if hasCur {
				pick = cur
			}
			if pick != "true" {
				pick = "false"
			}
			fv.Selected = pick
		case adapter.FieldString:
			if len(f.Options) > 0 {
				pick := def
				if hasCur {
					pick = cur
				}
				fv.Selected = pick
			} else if f.Secret {
				// 掩码 *** 或任意非空当前值都视为「已设置」（留空保持）
				fv.SecretSet = hasCur && cur != ""
			} else {
				fv.Display = def
				if hasCur {
					fv.Display = cur
				}
			}
		default: // int / duration / list
			fv.Display = def
			if hasCur {
				fv.Display = cur
			}
		}
		vm.Fields = append(vm.Fields, fv)
	}
	return vm
}

func anyToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case []string:
		return strings.Join(t, " ")
	case nil:
		return ""
	default:
		return ""
	}
}

func renderPageSet(tpl *template.Template, w http.ResponseWriter, status int, vm pageVM) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tpl.ExecuteTemplate(w, "layout", vm); err != nil {
		slog.Error("page render failed", "err", err)
	}
}

func renderPartial(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	for _, tpl := range []*template.Template{tplWizard, tplList, tplChat, tplDetail} {
		if tpl.Lookup(name) == nil {
			continue
		}
		if err := tpl.ExecuteTemplate(w, name, data); err != nil {
			slog.Error("template render failed", "name", name, "err", err)
		}
		return
	}
	slog.Error("template not found", "name", name)
}

// mountWeb 挂载静态资源与 UI 页面。
func (s *Server) mountWeb(r chi.Router) {
	s.mountChat(r)

	sub, _ := fs.Sub(web.Assets, "assets")
	r.Handle("/assets/*", http.StripPrefix("/assets/", http.FileServer(http.FS(sub))))

	r.Get("/ui/topmeta", s.partialTopMeta)
	r.Get("/ui/agents/stats", s.partialAgentStats)

	// agent 配置页与保存
	r.Get("/agents/{id}/view", s.pageAgentDetail)
	r.Post("/ui/agents/{id}/save", s.actionAgentSave)

	// 向导
	r.Get("/agents/new", s.pageWizard)
	r.Get("/agents/new/config-form", s.partialConfigForm)
	r.Post("/agents/new/test", s.actionWizardTest)
	r.Post("/agents/new/submit", s.actionWizardSubmit)

	// 列表页与局部
	r.Get("/", s.pageAgentList)
	r.Get("/agents/table", s.partialAgentsTable)

	// 行操作
	r.Post("/ui/agents/{id}/test", s.actionAgentTest)
	r.Post("/ui/agents/{id}/disable", s.actionAgentDisable)
	r.Post("/ui/agents/{id}/enable", s.actionAgentEnable)
	r.Post("/ui/agents/{id}/delete", s.actionAgentDelete)
}

// computeTopMeta 顶栏计数。
func (s *Server) computeTopMeta(ctx context.Context) topMetaVM {
	agents, err := s.deps.Registry.List(ctx, registry.StatusFilter{})
	if err != nil {
		return topMetaVM{}
	}
	vm := topMetaVM{Total: len(agents)}
	for _, a := range agents {
		if a.Status == repo.StatusOnline || a.Status == repo.StatusBusy {
			vm.Online++
		}
	}
	return vm
}

// metaOf 供布局渲染的顶栏计数。
func (s *Server) metaOf(r *http.Request) *topMetaVM {
	vm := s.computeTopMeta(r.Context())
	return &vm
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
	renderPageSet(tplList, w, http.StatusOK, pageVM{
		Active: "agents", Title: "Agent",
		V:    listVM{Agents: dtos, Stats: s.computeStats(r.Context())},
		Meta: s.metaOf(r),
	})
}

// partialAgentsTable GET /agents/table?q= —— 列表局部（支持实时搜索过滤）。
func (s *Server) partialAgentsTable(w http.ResponseWriter, r *http.Request) {
	s.renderTable(w, r, "", false)
}

func (s *Server) renderTable(w http.ResponseWriter, r *http.Request, flash string, ok bool) {
	agents, err := s.deps.Registry.List(r.Context(), registry.StatusFilter{})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	dtos := make([]agentDTO, 0, len(agents))
	for _, a := range agents {
		dto := s.toDTO(a)
		if q != "" && !dtoMatches(dto, q) {
			continue
		}
		dtos = append(dtos, dto)
	}
	vm := listVM{Agents: dtos, Flash: flash, FlashOK: ok, Query: r.URL.Query().Get("q")}

	// 行操作（flash 非空）附带 OOB：一次响应同步刷新统计条与顶栏计数
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	exec := func(tpl *template.Template, name string, data any) {
		if err := tpl.ExecuteTemplate(w, name, data); err != nil {
			slog.Error("template render failed", "name", name, "err", err)
		}
	}
	exec(tplList, "agents_table", vm)
	if flash != "" {
		// OOB：与页面元素同 id 且显式声明 hx-swap-oob（普通局部端点不带此属性，
		// 否则 htmx 会将纯 OOB 响应从 innerHTML 目标中抽走、清空容器）
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<div id="stats-oob" class="telemetry" hx-swap-oob="true">`)
		exec(tplList, "agents_stats_cells", s.computeStats(r.Context()))
		_, _ = fmt.Fprint(w, `</div>`)
		tm := s.computeTopMeta(r.Context())
		_, _ = fmt.Fprintf(w, `<span id="topmeta-oob" hx-swap-oob="true">%d agent · %d 在线</span>`, tm.Total, tm.Online)
	}
}

// dtoMatches 搜索过滤：名称 / adapter / 状态 包含即命中。
func dtoMatches(a agentDTO, q string) bool {
	return strings.Contains(strings.ToLower(a.Name), q) ||
		strings.Contains(strings.ToLower(a.Adapter), q) ||
		strings.Contains(strings.ToLower(a.Status), q)
}

// partialTopMeta GET /ui/topmeta。
func (s *Server) partialTopMeta(w http.ResponseWriter, r *http.Request) {
	renderPartial(w, http.StatusOK, "topmeta", s.computeTopMeta(r.Context()))
}

// partialAgentStats GET /ui/agents/stats。
func (s *Server) partialAgentStats(w http.ResponseWriter, r *http.Request) {
	renderPartial(w, http.StatusOK, "agents_stats", s.computeStats(r.Context()))
}

func (s *Server) computeStats(ctx context.Context) statsVM {
	agents, err := s.deps.Registry.List(ctx, registry.StatusFilter{IncludeRemoved: true})
	if err != nil {
		return statsVM{}
	}
	vm := statsVM{Total: len(agents)}
	for _, a := range agents {
		switch a.Status {
		case repo.StatusOnline, repo.StatusBusy:
			vm.Online++
		case repo.StatusOffline:
			vm.Offline++
		case repo.StatusRemoved:
			vm.Removed++
		}
	}
	vm.Total -= vm.Removed
	return vm
}

// pageWizard GET /agents/new —— 注册向导。
func (s *Server) pageWizard(w http.ResponseWriter, r *http.Request) {
	type adapterItem struct{ Name string }
	items := make([]adapterItem, 0, 8)
	factories := s.deps.Registry.Factories()
	for _, f := range factories {
		items = append(items, adapterItem{Name: f.Name()})
	}
	var initial any
	if len(factories) > 0 {
		initial = buildConfigFormVM(factories[0].ConfigSchema(), nil)
	}
	renderPageSet(tplWizard, w, http.StatusOK, pageVM{
		Active: "wizard", Title: "注册 Agent",
		V:    map[string]any{"Adapters": items, "InitialSchema": initial},
		Meta: s.metaOf(r),
	})
}

// partialConfigForm GET /agents/new/config-form?adapter=echod —— 动态表单局部。
func (s *Server) partialConfigForm(w http.ResponseWriter, r *http.Request) {
	f := s.deps.Registry.Factory(r.URL.Query().Get("adapter"))
	if f == nil {
		http.Error(w, "unknown adapter", http.StatusBadRequest)
		return
	}
	renderPartial(w, http.StatusOK, "config_form", buildConfigFormVM(f.ConfigSchema(), nil))
}

// wizardSpecFromForm 从表单解析注册/编辑规格（cfg_* → config，cap_* → capabilities）。
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
	if v := r.FormValue("cap_can_stream"); v == "true" || v == "false" {
		caps.CanStream = v == "true"
		hasCaps = true
	}
	if hasCaps {
		spec.Capabilities = &caps
	}
	sendMsg = r.FormValue("send_message") == "true" || r.FormValue("send_message") == "on"
	return name, spec, sendMsg, nil
}

// actionWizardTest POST /agents/new/test —— 保存前连通性测试。
func (s *Server) actionWizardTest(w http.ResponseWriter, r *http.Request) {
	_, spec, sendMsg, err := s.wizardSpecFromForm(r)
	if err != nil {
		renderPartial(w, http.StatusBadRequest, "test_result", testResultVM{Error: err.Error()})
		return
	}
	res := s.deps.Registry.TestConnection(r.Context(), spec, sendMsg)
	renderPartial(w, http.StatusOK, "test_result", testResultVM{
		OK: res.OK, DurationMs: strconv.FormatFloat(res.DurationMs, 'f', 1, 64),
		Error: res.Err, SentMessage: sendMsg,
	})
}

type testResultVM struct {
	OK          bool
	DurationMs  string
	Error       string
	SentMessage bool
}

// actionWizardSubmit POST /agents/new/submit —— 提交注册；成功后 HX-Redirect 到配置页。
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
	w.Header().Set("HX-Redirect", "/agents/"+agent.ID+"/view")
	renderPartial(w, http.StatusOK, "submit_result", submitResultVM{
		OK: true, Name: name, Status: agent.Status,
		ViewURL: "/agents/" + agent.ID + "/view", AgentID: agent.ID,
	})
}

type submitResultVM struct {
	OK      bool
	Name    string
	Status  string
	Error   string
	ViewURL string
	AgentID string
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
	s.renderTable(w, r, fmtConnectivity(res), res.OK)
}

func fmtConnectivity(res registry.TestResult) string {
	flash := fmt.Sprintf("连通性测试 %s：%.1f ms", map[bool]string{true: "通过", false: "失败"}[res.OK], res.DurationMs)
	if !res.OK && res.Err != "" {
		flash += " — " + res.Err
	}
	return flash
}

func (s *Server) actionAgentDisable(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Registry.Disable(chi.URLParam(r, "id")); err != nil {
		mapWriteError(w, r, err)
		return
	}
	s.renderTable(w, r, "已停用（可随时启用恢复）", true)
}

func (s *Server) actionAgentEnable(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Registry.Enable(r.Context(), chi.URLParam(r, "id")); err != nil {
		mapWriteError(w, r, err)
		return
	}
	s.renderTable(w, r, "已启用并重新探活", true)
}

func (s *Server) actionAgentDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Registry.Unregister(r.Context(), chi.URLParam(r, "id")); err != nil {
		mapWriteError(w, r, err)
		return
	}
	s.renderTable(w, r, "已软删（历史会话/消息保留）", true)
}
