package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// GET /api/events：最近事件 seq 降序、type 过滤生效；未鉴权 401。
func TestEventsEndpoint(t *testing.T) {
	ts, reg, _ := newTestServer(t)

	// 产生事件：注册 agent（探活通过 → agent.status 事件落库）
	_, body := doReq(t, "POST", ts.URL+"/api/agents", map[string]any{
		"name": "ev-echod", "adapter": "echod", "config": map[string]any{"mode": "echo"},
	}, authed())
	require.NotEmpty(t, body["id"], "注册应成功: %v", body)

	// 未鉴权 → 401
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// 鉴权 → 最近事件（bus 异步批量落库，轮询等待 flush）
	fetch := func(url string) []map[string]any {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var out []map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		return out
	}

	require.Eventually(t, func() bool {
		return len(fetch(ts.URL+"/api/events?limit=50")) > 0
	}, 2*time.Second, 20*time.Millisecond, "注册产生的 agent.status 事件应在 flush 后可查")

	evs := fetch(ts.URL + "/api/events?limit=50")
	for i := 1; i < len(evs); i++ {
		prev, _ := evs[i-1]["seq"].(float64)
		cur, _ := evs[i]["seq"].(float64)
		require.Greater(t, prev, cur, "seq 应降序（最新在前）")
	}

	// type 过滤
	byType := fetch(ts.URL + "/api/events?type=agent.status&limit=50")
	require.NotEmpty(t, byType)
	for _, e := range byType {
		require.Equal(t, "agent.status", e["type"])
	}

	_ = reg // fixture 借用
}
