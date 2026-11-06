package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cookieOf 从 login 响应提取 nexus_session cookie 值。
func cookieOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == "nexus_session" && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

func login(t *testing.T, url, token string) (*http.Response, map[string]any) {
	t.Helper()
	return doReq(t, http.MethodPost, url+"/api/auth/login", map[string]any{"token": token}, nil)
}

// TODO#2 闭环：登录换 cookie → cookie 访问 API。
func TestAuthCookieLoginFlow(t *testing.T) {
	ts, _, _ := newTestServer(t)

	// 错 token → 401，无 cookie
	resp, body := login(t, ts.URL, "wrong")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, "unauthorized", body["error"].(map[string]any)["code"])
	assert.Empty(t, cookieOf(t, resp))

	// 正确 token → 200 + Set-Cookie
	resp, _ = login(t, ts.URL, testToken)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	sid := cookieOf(t, resp)
	require.NotEmpty(t, sid)

	// cookie 访问 API（无 Bearer）→ 200
	resp, _ = doReq(t, http.MethodGet, ts.URL+"/api/agents", nil,
		map[string]string{"Cookie": "nexus_session=" + sid})
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Bearer 与 cookie 双通道并存
	resp, _ = doReq(t, http.MethodGet, ts.URL+"/api/agents", nil, authed())
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// 非法/过期会话 → 401。
func TestAuthCookieInvalid(t *testing.T) {
	ts, _, _ := newTestServer(t)

	for _, sid := range []string{"bogus", ""} {
		h := map[string]string{"Cookie": "nexus_session=" + sid}
		resp, body := doReq(t, http.MethodGet, ts.URL+"/api/agents", nil, h)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "sid=%q", sid)
		assert.Equal(t, "unauthorized", body["error"].(map[string]any)["code"])
	}

	// 无 cookie 头
	resp, _ := doReq(t, http.MethodGet, ts.URL+"/api/agents", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// 登录后 status.authenticated=true；logout 后会话吊销。
func TestAuthStatusAndLogout(t *testing.T) {
	ts, _, _ := newTestServer(t)

	// 未登录
	resp, st := doReq(t, http.MethodGet, ts.URL+"/api/auth/status", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, true, st["auth_required"])
	assert.Equal(t, false, st["authenticated"])

	resp, _ = login(t, ts.URL, testToken)
	sid := cookieOf(t, resp)
	require.NotEmpty(t, sid)
	h := map[string]string{"Cookie": "nexus_session=" + sid}

	resp, st = doReq(t, http.MethodGet, ts.URL+"/api/auth/status", nil, h)
	assert.Equal(t, true, st["authenticated"])

	// logout（需已登录）→ 会话吊销
	resp, _ = doReq(t, http.MethodPost, ts.URL+"/api/auth/logout", nil, h)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, _ = doReq(t, http.MethodGet, ts.URL+"/api/agents", nil, h)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp, st = doReq(t, http.MethodGet, ts.URL+"/api/auth/status", nil, h)
	assert.Equal(t, false, st["authenticated"])
}

// 本地免登模式（未配置 auth_token）：login 拒绝、status auth_required=false、全放行。
func TestAuthLocalMode(t *testing.T) {
	ts, _, _ := newTestServerToken(t, "")

	resp, body := login(t, ts.URL, testToken)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "auth_not_required", body["error"].(map[string]any)["code"])

	resp, st := doReq(t, http.MethodGet, ts.URL+"/api/auth/status", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, false, st["auth_required"])
	assert.Equal(t, true, st["authenticated"])

	resp, _ = doReq(t, http.MethodGet, ts.URL+"/api/agents", nil, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// login/status 免鉴权可达（无需任何凭证）。
func TestAuthPublicEndpoints(t *testing.T) {
	ts, _, _ := newTestServer(t)

	resp, _ := doReq(t, http.MethodGet, ts.URL+"/api/auth/status", nil, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// 空 body / 非法 JSON → 400 而非 401（说明端点本身可达）
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/login", nil)
	require.NoError(t, err)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
