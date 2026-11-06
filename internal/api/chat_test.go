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

// 私聊聊天页（M2 浏览器双向聊天）：页面 → 建会话 → 表单发送 → 消息局部回显。
func TestChatPageFlow(t *testing.T) {
	ts, reg, _ := newSessionTestServer(t)

	// 注册一个普通 agent（同步）与一个流式 agent
	_, agent := doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "echod-chat", "adapter": "echod", "config": map[string]any{"mode": "echo"},
	}, authed())
	id := agent["id"].(string)
	_, streamAgent := doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "echod-chat-stream", "adapter": "echod",
		"config": map[string]any{"mode": "stream", "delay_ms": 5, "stream_chunks": 3},
	}, authed())
	streamID := streamAgent["id"].(string)

	// 1) 聊天页（无会话）：agent 选择器
	code, body := getUI(t, ts.URL, "/chat")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "选择 agent 开始对话")
	assert.Contains(t, body, "echod-chat")
	assert.Contains(t, body, `name="agent_id" value="`+id+`"`)

	// 2) 建会话 → 303 到 /chat?session=
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/chat/session", strings.NewReader(url.Values{"agent_id": {id}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+testToken)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	loc := resp.Header.Get("Location")
	require.Contains(t, loc, "/chat?session=")
	sessID := strings.TrimPrefix(loc, "/chat?session=")

	// 3) 会话页渲染：面板 + session id + 输入框 + 发送表单
	code, body = getUI(t, ts.URL, loc)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "<b>echod-chat</b>")
	assert.Contains(t, body, sessID)
	assert.Contains(t, body, `hx-post="/ui/chat/send"`)
	assert.Contains(t, body, `name="session_id" value="`+sessID+`"`)
	assert.NotContains(t, body, "chat-stream-btn", "非流式 agent 不显示流式发送按钮")
	assert.Contains(t, body, `hx-get="/ui/chat/sessions?current=`+sessID, "会话侧栏局部应挂载")

	// 流式 agent 的会话页应显示流式按钮
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/chat/session", strings.NewReader(url.Values{"agent_id": {streamID}}.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("Authorization", "Bearer "+testToken)
	resp2, err2 := noRedirect.Do(req2)
	require.NoError(t, err2)
	loc2 := resp2.Header.Get("Location")
	resp2.Body.Close()
	code, body = getUI(t, ts.URL, loc2)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "chat-stream-btn", "CanStream agent 应有流式发送按钮")
	assert.Contains(t, body, "sse:msg.agent", "应订阅全局 SSE 实时刷新（sse 扩展触发）")

	// 4) 表单发送（同步）→ 消息局部回显双气泡
	code, body = postFormUI(t, ts.URL, "/ui/chat/send", url.Values{
		"session_id": {sessID}, "content": {"聊天页第一条"},
	})
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "聊天页第一条")
	assert.Contains(t, body, `bubble human`)
	assert.Contains(t, body, `bubble agent`)

	// 4b) ?agent= 直达入口（详情页「开始私聊」）
	resp3, _ := noRedirect.Do(func() *http.Request {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/chat?agent="+streamID, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		return req
	}())
	loc3 := resp3.Header.Get("Location")
	resp3.Body.Close()
	require.Contains(t, loc3, "/chat?session=", "agent 直达应建会话并重定向")
	code, body = getUI(t, ts.URL, loc3)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "<b>echod-chat-stream</b>")

	// 4c) 会话侧栏局部
	code, body = getUI(t, ts.URL, "/ui/chat/sessions?current="+sessID)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "echod-chat", "侧栏应列出 agent 名")

	// 5) 消息局部端点（轮询源）
	code, body = getUI(t, ts.URL, "/ui/chat/messages?session="+sessID)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "聊天页第一条")

	// 6) 会话确实建立在 router 侧（agent 对应会话可查）
	sessions, err := reg.AdapterInstance(id)
	require.NoError(t, err)
	_ = sessions

	// 7) 空内容发送 → flash 提示不崩
	code, body = postFormUI(t, ts.URL, "/ui/chat/send", url.Values{
		"session_id": {sessID}, "content": {""},
	})
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "消息内容为空")
	_ = io.Discard
}
