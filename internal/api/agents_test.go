package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/adapter/echo"
	"github.com/dddpeter/agent-nexus/internal/adapter/hermes"
	"github.com/dddpeter/agent-nexus/internal/adapter/picoclaw"
	"github.com/dddpeter/agent-nexus/internal/api"
	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/internal/config"
	"github.com/dddpeter/agent-nexus/internal/registry"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

const testToken = "nexus-test-token"

func newTestServer(t *testing.T) (*httptest.Server, *registry.Impl, repo.Store) {
	return newTestServerToken(t, testToken)
}

// newTestServerToken 可指定 auth_token（空串 = 本地免登模式）。
func newTestServerToken(t *testing.T, token string) (*httptest.Server, *registry.Impl, repo.Store) {
	t.Helper()
	store := repo.NewMemoryStore()
	b, err := bus.New(store.Events(), bus.Options{FlushInterval: 10 * time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	reg, err := registry.New(store, b, []adapter.AdapterFactory{
		echo.NewFactory(), hermes.NewFactory(), picoclaw.NewFactory(),
	}, registry.Options{HeartbeatInterval: time.Hour})
	require.NoError(t, err)
	t.Cleanup(reg.Close)

	cfg := config.Default()
	cfg.Server.AuthToken = token
	srv := api.New(api.Deps{Cfg: cfg, Store: store, Bus: b, Registry: reg})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, reg, store
}

func doReq(t *testing.T, method, url string, body any, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rd)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	out := map[string]any{}
	if resp.StatusCode != http.StatusNoContent {
		_ = json.NewDecoder(resp.Body).Decode(&out)
	}
	resp.Body.Close()
	return resp, out
}

func authed() map[string]string { return map[string]string{"Authorization": "Bearer " + testToken} }

// U-F5-01 无/错 token → 401；正确 token 放行。
func TestAuth_U_F5_01(t *testing.T) {
	ts, _, _ := newTestServer(t)

	// 无 token
	resp, body := doReq(t, http.MethodGet, ts.URL+"/api/agents", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, "unauthorized", body["error"].(map[string]any)["code"])

	// 错 token
	resp, _ = doReq(t, http.MethodGet, ts.URL+"/api/agents", nil,
		map[string]string{"Authorization": "Bearer wrong"})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// 正确 token
	resp, _ = doReq(t, http.MethodGet, ts.URL+"/api/agents", nil, authed())
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// healthz 免鉴权
	resp, _ = doReq(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// U-F5-02 限流：61 req/min → 第 61 次 429。
func TestRateLimit_U_F5_02(t *testing.T) {
	ts, _, _ := newTestServer(t)
	var lastStatus int
	for i := 1; i <= 61; i++ {
		resp, body := doReq(t, http.MethodGet, ts.URL+"/api/agents", nil, authed())
		lastStatus = resp.StatusCode
		if i < 61 {
			require.Equal(t, http.StatusOK, lastStatus, "前 60 次应放行（第 %d 次）", i)
		} else {
			require.Equal(t, http.StatusTooManyRequests, lastStatus, "第 61 次应 429")
			require.Equal(t, "rate_limited", body["error"].(map[string]any)["code"])
		}
	}
	_ = lastStatus
}

// U-F5-04 错误结构统一 {"error":{code,message,request_id}}。
func TestErrorEnvelope_U_F5_04(t *testing.T) {
	ts, _, _ := newTestServer(t)

	resp, raw := doReq(t, http.MethodGet, ts.URL+"/api/agents/nonexistent", nil, authed())
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Contains(t, raw, "error")
	errObj := raw["error"].(map[string]any)
	assert.Equal(t, "not_found", errObj["code"])
	assert.NotEmpty(t, errObj["message"])
	assert.NotEmpty(t, errObj["request_id"], "错误结构应含 request_id")
	assert.NotEmpty(t, resp.Header.Get("X-Request-Id"))
}

// C-F1-03 连通性测试：坏 endpoint 明确错误；编辑配置改坏 → 保存被拦且旧配置生效。
func TestConnectivityTestAndRollback_C_F1_03(t *testing.T) {
	ts, _, _ := newTestServer(t)

	// 注册一个健康 agent（echod）
	resp, agent := doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "echod-x", "adapter": "echod", "config": map[string]any{"mode": "echo"},
	}, authed())
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	id := agent["id"].(string)

	// ① 保存前按规格测试：指向不存在的 endpoint（hermes）→ 明确错误非 500
	resp, testRes := doReq(t, http.MethodPost, ts.URL+"/api/agents/test", map[string]any{
		"adapter": "hermes",
		"config":  map[string]any{"base_url": "http://127.0.0.1:1", "timeout": "500ms"},
	}, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, false, testRes["ok"])
	errMsg := testRes["error"].(string)
	assert.True(t, strings.Contains(errMsg, "unreachable") || strings.Contains(errMsg, "refused"),
		"错误应明确（连接拒绝/不可达），实际: %s", errMsg)

	// ② 已保存 agent 的 test 端点：当前配置测试通过
	resp, testRes = doReq(t, http.MethodPost, ts.URL+"/api/agents/"+id+"/test", nil, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, true, testRes["ok"])

	// ③ 编辑配置改为坏 endpoint（hermes 形态改 adapter 不可行——用 echod mode=down 模拟坏配置）
	resp, body := doReq(t, http.MethodPut, ts.URL+"/api/agents/"+id, map[string]any{
		"config": map[string]any{"mode": "down"},
	}, authed())
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "探活失败的保存应被拦")
	assert.Equal(t, "connectivity_test_failed", body["error"].(map[string]any)["code"])

	// 旧配置生效
	resp, after := doReq(t, http.MethodGet, ts.URL+"/api/agents/"+id, nil, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "echo", after["config"].(map[string]any)["mode"], "旧配置应生效")
	assert.Equal(t, "online", after["status"], "旧配置期间状态不变")

	// ④ force=true 强制保存坏配置（agent 转offline 待恢复）
	resp, _ = doReq(t, http.MethodPut, ts.URL+"/api/agents/"+id, map[string]any{
		"config": map[string]any{"mode": "down"}, "force": true,
	}, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp, after = doReq(t, http.MethodGet, ts.URL+"/api/agents/"+id, nil, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "offline", after["status"], "强制保存坏配置应 offline")
}

// C-F1-04 配置脱敏：注册填 api_key → 详情回显 ***；PUT 留空 → 原值保留。
func TestSecretMasking_C_F1_04(t *testing.T) {
	ts, reg, _ := newTestServer(t)

	resp, agent := doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "echod-secret", "adapter": "echod",
		"config": map[string]any{"mode": "echo", "api_key": "sk-registry-level-1"},
	}, authed())
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	id := agent["id"].(string)

	// 回显脱敏
	resp, detail := doReq(t, http.MethodGet, ts.URL+"/api/agents/"+id, nil, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	cfg := detail["config"].(map[string]any)
	assert.Equal(t, "***", cfg["api_key"], "密钥回显脱敏")

	// PUT 留空 → 原值保留（内部存储仍为原值）
	resp, _ = doReq(t, http.MethodPut, ts.URL+"/api/agents/"+id, map[string]any{
		"config": map[string]any{"mode": "echo", "api_key": ""},
	}, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	internal, err := reg.Get(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "sk-registry-level-1", internal.Config["api_key"], "留空表示不修改")

	// PUT 传 *** → 同样不修改
	resp, _ = doReq(t, http.MethodPut, ts.URL+"/api/agents/"+id, map[string]any{
		"config": map[string]any{"mode": "echo", "api_key": "***"},
	}, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	internal, _ = reg.Get(t.Context(), id)
	assert.Equal(t, "sk-registry-level-1", internal.Config["api_key"])

	// 列表也脱敏
	resp, _ = doReq(t, http.MethodGet, ts.URL+"/api/agents", nil, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rawList := fetchRaw(t, ts.URL+"/api/agents")
	assert.NotContains(t, rawList, "sk-registry-level-1", "列表输出不得含密钥明文")
	assert.Contains(t, rawList, "***")
}

func fetchRaw(t *testing.T, url string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// 注册闭环（C-F1-01 前半）：注册 → list 可见 → 软删 → list 默认不可见。
func TestRegisterListUnregister_C_F1_01(t *testing.T) {
	ts, _, _ := newTestServer(t)

	resp, agent := doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "echod-c1", "adapter": "echod", "config": map[string]any{"mode": "echo"},
	}, authed())
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, "online", agent["status"])
	id := agent["id"].(string)

	// list 可见
	raw := fetchRaw(t, ts.URL+"/api/agents")
	assert.Contains(t, raw, "echod-c1")

	// 重名 409
	resp, body := doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "echod-c1", "adapter": "echod", "config": map[string]any{"mode": "echo"},
	}, authed())
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "duplicate_name", body["error"].(map[string]any)["code"])

	// 非法配置 400（含字段名）
	resp, body = doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "echod-bad", "adapter": "echod", "config": map[string]any{"mode": "no-such"},
	}, authed())
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	errMsg := body["error"].(map[string]any)["message"].(string)
	assert.Contains(t, errMsg, "mode", "错误信息含字段名（U-F1-02 API 层）")

	// 软删
	resp, _ = doReq(t, http.MethodDelete, ts.URL+"/api/agents/"+id, nil, authed())
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	raw = fetchRaw(t, ts.URL+"/api/agents")
	assert.NotContains(t, raw, "echod-c1", "软删后默认不可见")
	raw = fetchRaw(t, ts.URL+"/api/agents?include_removed=true")
	assert.Contains(t, raw, "echod-c1", "include_removed 可查（历史保留）")
}

// 停用/启用（C-F1-05 API 层）：disable → status offline + disabled 标记；enable → 恢复。
func TestDisableEnable_C_F1_05(t *testing.T) {
	ts, reg, _ := newTestServer(t)

	_, agent := doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "echod-dis", "adapter": "echod", "config": map[string]any{"mode": "echo"},
	}, authed())
	id := agent["id"].(string)

	resp, _ := doReq(t, http.MethodPost, ts.URL+"/api/agents/"+id+"/disable", nil, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, err := reg.Acquire(t.Context(), id)
	require.ErrorIs(t, err, registry.ErrAgentDisabled, "停用后派发拒绝（409 语义）")

	resp, _ = doReq(t, http.MethodPost, ts.URL+"/api/agents/"+id+"/enable", nil, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	release, err := reg.Acquire(t.Context(), id)
	require.NoError(t, err, "启用后恢复派发")
	release()
}

// adapters schema 输出（向导动态表单数据源）。
func TestAdaptersSchemaEndpoint(t *testing.T) {
	ts, _, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/adapters", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var adapters []map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&adapters))
	require.GreaterOrEqual(t, len(adapters), 3, "echod/hermes/picoclaw 均应出现")

	var echodSchema map[string]any
	for _, a := range adapters {
		if a["name"] == "echod" {
			echodSchema = a["config_schema"].(map[string]any)
		}
	}
	require.NotNil(t, echodSchema, "echod ConfigSchema 应存在")
	fields := echodSchema["fields"].([]any)
	require.NotEmpty(t, fields)
	var hasSecret bool
	for _, f := range fields {
		fm := f.(map[string]any)
		if fm["secret"] == true {
			hasSecret = true
		}
	}
	assert.True(t, hasSecret, "echod schema 应声明密钥字段（api_key）")
}
