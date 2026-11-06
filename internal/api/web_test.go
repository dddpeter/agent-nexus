package api_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dddpeter/agent-nexus/internal/registry"
)

func getUI(t *testing.T, tsURL, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, tsURL+path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func postFormUI(t *testing.T, tsURL, path string, form url.Values) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, tsURL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// 页面冒烟：agent 列表页 + 注册向导（C-F1-02 页面侧）。
func TestUIPagesSmoke(t *testing.T) {
	ts, reg, _ := newTestServer(t)

	// 列表页
	code, body := getUI(t, ts.URL, "/")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "被纳管的 agent 目录")
	assert.Contains(t, body, "注册 Agent")

	// 列表局部
	code, body = getUI(t, ts.URL, "/agents/table")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "还没有注册任何 agent")

	// 首屏整页渲染不中断：统计条 + 空态同时在场（防 agents_stats 数据契约回归）
	code, body = getUI(t, ts.URL, "/")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "已注册", "首屏统计条应渲染")
	assert.Contains(t, body, "还没有注册任何 agent", "首屏表格空态应渲染")

	// 向导页：adapter 下拉 + 首个 schema 内联表单
	code, body = getUI(t, ts.URL, "/agents/new")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, `value="echod"`)
	assert.Contains(t, body, "cfg_mode")
	assert.Contains(t, body, `type="password"`, "api_key 密钥字段应渲染为 password 输入")

	// 动态表单局部（换 hermes）
	code, body = getUI(t, ts.URL, "/agents/new/config-form?adapter=hermes")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "cfg_base_url")
	assert.Contains(t, body, "Gateway 地址")

	// 未知 adapter → 400
	code, _ = getUI(t, ts.URL, "/agents/new/config-form?adapter=ghost")
	assert.Equal(t, http.StatusBadRequest, code)

	// 向导：测试连通性（表单值）→ 通过
	form := url.Values{}
	form.Set("name", "echod-ui-1")
	form.Set("adapter", "echod")
	form.Set("cfg_mode", "echo")
	form.Set("cfg_delay_ms", "0")
	code, body = postFormUI(t, ts.URL, "/agents/new/test", form)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "连通性测试通过")

	// 向导：提交注册 → online（探活通过自动 online）
	code, body = postFormUI(t, ts.URL, "/agents/new/submit", form)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "注册成功")
	assert.Contains(t, body, "online")

	agents, err := reg.List(t.Context(), registry.StatusFilter{})
	require.NoError(t, err)
	require.Len(t, agents, 1)
	assert.Equal(t, "echod-ui-1", agents[0].Name)

	// 列表页可见 + 操作按钮渲染
	code, body = getUI(t, ts.URL, "/agents/table")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "echod-ui-1")
	assert.Contains(t, body, "/ui/agents/"+agents[0].ID+"/test")

	// 坏配置测试 → 失败详情展示
	form.Set("cfg_mode", "down")
	code, body = postFormUI(t, ts.URL, "/agents/new/test", form)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "测试失败")

	// 行操作：停用 → 表格刷新含 disabled
	code, body = postFormUI(t, ts.URL, "/ui/agents/"+agents[0].ID+"/disable", url.Values{})
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "已停用")

	// 行操作：测试连通性 → flash
	code, body = postFormUI(t, ts.URL, "/ui/agents/"+agents[0].ID+"/test", url.Values{})
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "连通性测试")

	// 静态资源
	code, body = getUI(t, ts.URL, "/assets/htmx.min.js")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "htmx")
}
