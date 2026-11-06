package api_test

import (
	"bufio"
	"context"
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
	"github.com/dddpeter/agent-nexus/internal/api"
	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/internal/config"
	"github.com/dddpeter/agent-nexus/internal/registry"
	"github.com/dddpeter/agent-nexus/internal/repo"
	"github.com/dddpeter/agent-nexus/internal/session/private"
)

// newSessionTestServer 带私聊 Router 的 API 服务（内存 runner 立即执行异步任务）。
func newSessionTestServer(t *testing.T) (*httptestServer, *registry.Impl, repo.Store) {
	t.Helper()
	store := repo.NewMemoryStore()
	b, err := bus.New(store.Events(), bus.Options{FlushInterval: 10 * time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	reg, err := registry.New(store, b, []adapter.AdapterFactory{echo.NewFactory()},
		registry.Options{HeartbeatInterval: time.Hour})
	require.NoError(t, err)
	t.Cleanup(reg.Close)

	router := private.New(store, reg, b, private.Options{})
	router.SetAsyncRunner(syncRunner{})
	cfg := config.Default()
	cfg.Server.AuthToken = testToken
	srv := api.New(api.Deps{Cfg: cfg, Store: store, Bus: b, Registry: reg, Router: router})
	ts := newHttptest(t, srv.Handler())
	return &httptestServer{URL: ts.URL}, reg, store
}

// httptestServer 简化 URL 持有。
type httptestServer struct{ URL string }

func newHttptest(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

// doReqList JSON 数组响应解码。
func doReqList(t *testing.T, url string, headers map[string]string) (*http.Response, []map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	out := []map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// syncRunner 立即同步执行（测试确定性）。
type syncRunner struct{}

func (syncRunner) Submit(_ context.Context, job func(context.Context)) (string, error) {
	job(context.Background())
	return "task-sync-1", nil
}

// C-F1-01 全链路：API 注册 → list 可见 → 派消息成功 → 注销 → 派消息 404。
func TestAPIAgentLifecycle_C_F1_01(t *testing.T) {
	ts, _, _ := newSessionTestServer(t)

	// 注册
	resp, agent := doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "echod-c101", "adapter": "echod", "config": map[string]any{"mode": "echo"},
	}, authed())
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	id := agent["id"].(string)

	// list 可见
	raw := fetchRaw(t, ts.URL+"/api/agents")
	assert.Contains(t, raw, "echod-c101")

	// 建会话 + 派消息成功（回显）
	resp, sess := doReq(t, http.MethodPost, ts.URL+"/api/sessions", map[string]any{"agent_id": id}, authed())
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	sessID := sess["id"].(string)

	resp, sendRes := doReq(t, http.MethodPost, ts.URL+"/api/sessions/"+sessID+"/messages",
		map[string]any{"content": "ping-echo-test", "mode": "sync"}, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	reply := sendRes["reply"].(map[string]any)
	assert.Equal(t, "ping-echo-test", reply["content"])
	assert.Equal(t, "agent", reply["sender_type"])

	// 注销
	resp, _ = doReq(t, http.MethodDelete, ts.URL+"/api/agents/"+id, nil, authed())
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	// 注销后派消息 → 404（agent 不存在）
	resp, body := doReq(t, http.MethodPost, ts.URL+"/api/sessions/"+sessID+"/messages",
		map[string]any{"content": "again"}, authed())
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "注销后派消息应 404")
	assert.Equal(t, "not_found", body["error"].(map[string]any)["code"])
}

// C-F1-05 消息环节：disable → 派消息 409 → enable 恢复；软删后历史会话任务仍可查。
func TestDisableDeleteMessaging_C_F1_05(t *testing.T) {
	ts, reg, store := newSessionTestServer(t)

	_, agent := doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "echod-c105", "adapter": "echod", "config": map[string]any{"mode": "echo"},
	}, authed())
	id := agent["id"].(string)
	_, sess := doReq(t, http.MethodPost, ts.URL+"/api/sessions", map[string]any{"agent_id": id}, authed())
	sessID := sess["id"].(string)
	_, sendRes := doReq(t, http.MethodPost, ts.URL+"/api/sessions/"+sessID+"/messages",
		map[string]any{"content": "before disable"}, authed())
	require.NotNil(t, sendRes["reply"])

	// disable → 派消息 409
	resp, _ := doReq(t, http.MethodPost, ts.URL+"/api/agents/"+id+"/disable", nil, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp, body := doReq(t, http.MethodPost, ts.URL+"/api/sessions/"+sessID+"/messages",
		map[string]any{"content": "while disabled"}, authed())
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "agent_unavailable", body["error"].(map[string]any)["code"])

	// enable → 恢复
	resp, _ = doReq(t, http.MethodPost, ts.URL+"/api/agents/"+id+"/enable", nil, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp, sendRes = doReq(t, http.MethodPost, ts.URL+"/api/sessions/"+sessID+"/messages",
		map[string]any{"content": "after enable"}, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotNil(t, sendRes["reply"])

	// 软删 → 派消息失败；历史会话/任务仍可查
	resp, _ = doReq(t, http.MethodDelete, ts.URL+"/api/agents/"+id, nil, authed())
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp, _ = doReq(t, http.MethodPost, ts.URL+"/api/sessions/"+sessID+"/messages",
		map[string]any{"content": "after delete"}, authed())
	assert.NotEqual(t, http.StatusOK, resp.StatusCode, "软删后不可派消息")

	resp, sessions := doReqList(t, ts.URL+"/api/agents/"+id+"/sessions", authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, sessions, 1, "软删后历史会话仍可查")

	msgs, _ := store.Messages().ListBySession(t.Context(), sessID)
	assert.GreaterOrEqual(t, len(msgs), 4, "历史消息保留")
	_ = reg
}

// 私聊 API：async 回执 202 + accepted；stream 返回 SSE 事件流。
func TestSessionAsyncAndStreamAPI(t *testing.T) {
	ts, _, _ := newSessionTestServer(t)

	_, agent := doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "echod-async", "adapter": "echod", "config": map[string]any{"mode": "fixed", "reply": "异步完成结果"},
	}, authed())
	id := agent["id"].(string)
	_, sess := doReq(t, http.MethodPost, ts.URL+"/api/sessions", map[string]any{"agent_id": id}, authed())
	sessID := sess["id"].(string)

	// async → 202 + accepted（syncRunner 立即回填）
	resp, sendRes := doReq(t, http.MethodPost, ts.URL+"/api/sessions/"+sessID+"/messages",
		map[string]any{"content": "长任务", "mode": "async"}, authed())
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	asyncReceipt := sendRes["async"].(map[string]any)
	assert.Equal(t, "accepted", asyncReceipt["type"])
	assert.NotEmpty(t, asyncReceipt["task_id"])

	// 回填已落库（system 消息）
	time.Sleep(50 * time.Millisecond)
	resp, msgs := doReqList(t, ts.URL+"/api/sessions/"+sessID+"/messages", authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.GreaterOrEqual(t, len(msgs), 2)
	var backfilled bool
	for _, m := range msgs {
		if m["sender_type"] == "system" && m["content"] == "异步完成结果" {
			backfilled = true
		}
	}
	assert.True(t, backfilled, "异步回填 system 消息应可见")

	// stream agent
	_, streamAgent := doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "echod-stream", "adapter": "echod",
		"config": map[string]any{"mode": "stream", "delay_ms": 5, "stream_chunks": 3},
	}, authed())
	_, sess2 := doReq(t, http.MethodPost, ts.URL+"/api/sessions", map[string]any{"agent_id": streamAgent["id"]}, authed())

	// stream 模式 → SSE
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sessions/"+sess2["id"].(string)+"/messages",
		strings.NewReader(`{"content":"流式API测试","mode":"stream"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	sresp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer sresp.Body.Close()
	require.Equal(t, http.StatusOK, sresp.StatusCode)
	require.Contains(t, sresp.Header.Get("Content-Type"), "text/event-stream")

	scanner := bufio.NewScanner(sresp.Body)
	var deltas, finals, errs int
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: delta"):
			deltas++
		case strings.HasPrefix(line, "event: final"):
			finals++
		case strings.HasPrefix(line, "event: error"):
			errs++
		}
	}
	assert.GreaterOrEqual(t, deltas, 2, "SSE 应透传 ≥2 delta")
	assert.Equal(t, 1, finals)
	assert.Zero(t, errs)
}

// 未知 agent 建会话 → 404；未知会话发消息 → 404。
func TestSessionErrors(t *testing.T) {
	ts, _, _ := newSessionTestServer(t)
	resp, body := doReq(t, http.MethodPost, ts.URL+"/api/sessions", map[string]any{"agent_id": "ghost"}, authed())
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "not_found", body["error"].(map[string]any)["code"])

	resp, body2 := doReq(t, http.MethodPost, ts.URL+"/api/sessions/ghost/messages",
		map[string]any{"content": "x"}, authed())
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "未知会话应 404")
	assert.Equal(t, "not_found", body2["error"].(map[string]any)["code"])
}
