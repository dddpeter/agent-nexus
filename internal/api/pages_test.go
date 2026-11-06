package api_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Agent 详情/配置编辑页（FR1.1b）：脱敏回显 → 保存（变更前探活）→ 密钥留空保留 → 能力更新。
func TestAgentDetailPageFlow(t *testing.T) {
	ts, reg, store := newSessionTestServer(t)

	// 注册带密钥的 agent
	_, agent := doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "echod-detail", "adapter": "echod",
		"config": map[string]any{"mode": "echo", "api_key": "sk-detail-secret"},
	}, authed())
	id := agent["id"].(string)

	// 建一个会话（详情页「最近会话」应有数据）
	_, sess := doReq(t, http.MethodPost, ts.URL+"/api/sessions", map[string]any{"agent_id": id}, authed())

	// 1) 详情页渲染：表单脱敏 + 当前值回填 + 最近会话
	code, body := getUI(t, ts.URL, "/agents/"+id+"/view")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "echod-detail")
	assert.Contains(t, body, `<option value="echo" selected`, "当前 mode=echo 应选中")
	assert.Contains(t, body, `type="password" name="cfg_api_key"`, "密钥字段为 password 输入")
	assert.Contains(t, body, "留空保持不变", "已设置密钥的占位提示")
	assert.Contains(t, body, sess["id"], "最近会话应展示")
	assert.Contains(t, body, `name="cap_max_concurrency"`, "能力编辑")

	// 2) 保存好配置（mode 仍 echo）：成功
	code, body = postFormUI(t, ts.URL, "/ui/agents/"+id+"/save", url.Values{
		"cfg_mode": {"echo"}, "cfg_api_key": {""}, // 密钥留空 = 不改
		"cap_max_concurrency": {"2"}, "cap_interrupt_level": {"2"},
	})
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "✔ 已保存", body)

	// 内部状态：密钥保留、能力生效
	internal, err := reg.Get(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "sk-detail-secret", internal.Config["api_key"], "密钥留空不修改")
	assert.Equal(t, 2, internal.Capabilities.MaxConcurrency)
	assert.Equal(t, 2, internal.Capabilities.InterruptLevel)

	// 3) 保存坏配置：被拦 + 结果条
	code, body = postFormUI(t, ts.URL, "/ui/agents/"+id+"/save", url.Values{
		"cfg_mode": {"down"},
	})
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "✘ 保存被拒")
	internal, _ = reg.Get(t.Context(), id)
	assert.Equal(t, "echo", internal.Config["mode"], "旧配置生效")

	// 4) force 强制保存坏配置
	code, body = postFormUI(t, ts.URL, "/ui/agents/"+id+"/save", url.Values{
		"cfg_mode": {"down"}, "force": {"true"},
	})
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "探活失败")
	internal, _ = reg.Get(t.Context(), id)
	assert.Equal(t, "offline", internal.Status)

	// 5) 改回
	code, body = postFormUI(t, ts.URL, "/ui/agents/"+id+"/save", url.Values{
		"cfg_mode": {"echo"},
	})
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "✔ 已保存")

	// 6) 未知 agent → 404
	code, _ = getUI(t, ts.URL, "/agents/ghost/view")
	assert.Equal(t, http.StatusNotFound, code)
	_ = store
}

// 顶栏计数与统计条局部。
func TestTopMetaAndStats(t *testing.T) {
	ts, _, _ := newSessionTestServer(t)

	code, body := getUI(t, ts.URL, "/ui/topmeta")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "0 agent")

	_, _ = doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "echod-meta", "adapter": "echod", "config": map[string]any{"mode": "echo"},
	}, authed())
	_, _ = doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "echod-meta-down", "adapter": "echod", "config": map[string]any{"mode": "down"},
	}, authed())

	code, body = getUI(t, ts.URL, "/ui/topmeta")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "2 agent · 1 在线")

	code, body = getUI(t, ts.URL, "/ui/agents/stats")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, `>已注册<`) // 统计标签
	assert.Contains(t, body, "已注册")
}

// 向导提交后 HX-Redirect 到详情页。
func TestWizardSubmitRedirects(t *testing.T) {
	ts, _, _ := newSessionTestServer(t)
	resp, body := postFormUIHeaders(t, ts.URL, "/agents/new/submit", url.Values{
		"name": {"echod-wiz-r"}, "adapter": {"echod"}, "cfg_mode": {"echo"},
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("HX-Redirect"), "/agents/", "成功提交应 HX-Redirect")
	assert.Contains(t, body, "✔ 注册成功")
}

// postFormUIHeaders 同 postFormUI 但保留响应头。
func postFormUIHeaders(t *testing.T, tsURL, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, tsURL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}
