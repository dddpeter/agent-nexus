package api_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	"github.com/dddpeter/agent-nexus/internal/session/group"
)

// newGroupTestServer 带群协调器的 API 服务（注册 echod agent 供群成员用）。
func newGroupTestServer(t *testing.T) (*httptest.Server, *registry.Impl, map[string]string) {
	t.Helper()
	store := repo.NewMemoryStore()
	b, err := bus.New(store.Events(), bus.Options{FlushInterval: 10 * time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	reg, err := registry.New(store, b, []adapter.AdapterFactory{echo.NewFactory()},
		registry.Options{HeartbeatInterval: time.Hour})
	require.NoError(t, err)
	t.Cleanup(reg.Close)

	cfg := config.Default()
	cfg.Server.AuthToken = testToken
	coord := group.New(store, reg, b, group.Options{Logger: slog.New(slog.NewTextHandler(&discards{}, nil))})
	require.NoError(t, coord.LoadPersisted(context.Background()))
	srv := api.New(api.Deps{Cfg: cfg, Store: store, Bus: b, Registry: reg, Groups: coord,
		Log: slog.New(slog.NewTextHandler(&discards{}, nil))})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// 注册群成员 agent（经 API，保证探活 online）
	resp, body := doReq(t, "POST", ts.URL+"/api/agents", map[string]any{
		"name": "grp-echo", "adapter": "echod", "config": map[string]any{"mode": "echo"},
	}, authed())
	require.Equal(t, http.StatusCreated, resp.StatusCode, "注册群成员: %v", body)
	ids := map[string]string{"grp-echo": body["id"].(string)}
	return ts, reg, ids
}

type discards struct{}

func (d *discards) Write(p []byte) (int, error) { return len(p), nil }

// POST /api/groups → @ 路由 → 回复；GET messages 历史。
func TestGroupAPIRoundtrip(t *testing.T) {
	ts, _, ids := newGroupTestServer(t)

	_, created := doReq(t, "POST", ts.URL+"/api/groups", map[string]any{
		"name": "API 群",
		"members": []map[string]any{
			{"id": ids["grp-echo"], "type": "agent", "trigger": "any", "priority": 1, "profile": "测试 回显"},
		},
		"max_turns_per_topic": 3,
	}, authed())
	require.NotEmpty(t, created["id"], "建群应成功: %v", created)
	gid := created["id"].(string)

	// @ 路由发言（人打注册名）
	resp, send := doReq(t, "POST", ts.URL+"/api/groups/"+gid+"/messages",
		map[string]any{"content": "@grp-echo 你好", "sender_id": "user"}, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode, "发言应成功: %v", send)
	require.NotNil(t, send["reply"], "获准 agent 应回复")
	reply := send["reply"].(map[string]any)
	assert.Equal(t, ids["grp-echo"], reply["sender_id"])

	// 历史
	resp, msgs := doReqList(t, ts.URL+"/api/groups/"+gid+"/messages", authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.GreaterOrEqual(t, len(msgs), 2)

	// 详情含运行态
	resp, detail := doReq(t, "GET", ts.URL+"/api/groups/"+gid, nil, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	state := detail["state"].(map[string]any)
	assert.Equal(t, float64(1), state["turns_used"].(map[string]any)[ids["grp-echo"]])
}

// 建群校验：无 agent 成员拒绝；未知群 404。
func TestGroupAPIValidation(t *testing.T) {
	ts, _, _ := newGroupTestServer(t)

	resp, _ := doReq(t, "POST", ts.URL+"/api/groups", map[string]any{
		"name":    "没有人",
		"members": []map[string]any{{"id": "human-1", "type": "human"}},
	}, authed())
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp, _ = doReq(t, "GET", ts.URL+"/api/groups/nope", nil, authed())
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// 阈值热更新（FR3.7）。
func TestGroupAPIConfigHotUpdate(t *testing.T) {
	ts, _, ids := newGroupTestServer(t)
	_, created := doReq(t, "POST", ts.URL+"/api/groups", map[string]any{
		"name": "热调群",
		"members": []map[string]any{
			{"id": ids["grp-echo"], "type": "agent", "trigger": "any", "priority": 1},
		},
	}, authed())
	gid := created["id"].(string)

	resp, updated := doReq(t, "PUT", ts.URL+"/api/groups/"+gid+"/config",
		map[string]any{"max_turns_per_topic": 5, "human_silent_threshold": 2}, authed())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, float64(5), updated["max_turns_per_topic"])
	assert.Equal(t, float64(2), updated["human_silent_threshold"])

	// 立即生效（Get 详情同步）
	_, detail := doReq(t, "GET", ts.URL+"/api/groups/"+gid, nil, authed())
	state := detail["state"].(map[string]any)
	assert.Equal(t, float64(5), state["max_turns_per_topic"])
}
