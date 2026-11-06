package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dddpeter/agent-nexus/internal/bus"
)

// sseEvent 解析后的事件。
type sseEvent struct {
	ID   int64
	Type string
	Data string
}

// readSSE 连接 /api/stream 并读取事件（读到 keepalive 注释忽略）。
func readSSE(t *testing.T, tsURL string, lastEventID int64, max time.Duration, stopAfter func(sseEvent) bool) []sseEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), max)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, tsURL+"/api/stream", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(lastEventID, 10))
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	var out []sseEvent
	scanner := bufio.NewScanner(resp.Body)
	var cur sseEvent
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			id, _ := strconv.ParseInt(strings.TrimSpace(line[4:]), 10, 64)
			cur.ID = id
		case strings.HasPrefix(line, "event: "):
			cur.Type = line[7:]
		case strings.HasPrefix(line, "data: "):
			cur.Data = line[6:]
		case line == "":
			if cur.ID != 0 || cur.Type != "" {
				out = append(out, cur)
				if stopAfter != nil && stopAfter(cur) {
					cancel()
					return out
				}
			}
			cur = sseEvent{}
		}
	}
	return out
}

func fmtInt(n int64) string { return strings.TrimSpace(strings.Join(strings.Fields(itoa(n)), "")) }
func itoa(n int64) string   { b := []byte(jsonNumber(n)); return string(b) }
func jsonNumber(n int64) []byte {
	b, _ := json.Marshal(n)
	return b
}
func fmtSscan(s string, v *int64) (int, error) {
	var n int
	_, err := fmtSscanf(s, v)
	return n, err
}
func fmtSscanf(s string, v *int64) (int, error) {
	raw := strings.TrimSpace(s)
	var out int64
	for _, c := range raw {
		if c < '0' || c > '9' {
			break
		}
		out = out*10 + int64(c-'0')
	}
	*v = out
	return len(raw), nil
}

// publishAndWait 经总线发布事件并等待落库（重放可查）。
func publishAndWait(t *testing.T, tsURL string, events ...bus.Event) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, tsURL+"/api/agents", nil) // 占位不用
	_ = req
}

// U-F5-03 SSE 断线重连带 Last-Event-ID → 补偿重放不丢消息、不重复。
func TestSSEReplayLastEventID_U_F5_03(t *testing.T) {
	ts, _, store := newTestServer(t)

	waitFlush := func(n int64) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if max, _ := store.Events().MaxEventSeq(t.Context()); max >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// 通过注册 agent 产生事件（agent.status）—— 第一批 5 个事件
	for i := 0; i < 5; i++ {
		respReg, body := doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
			"name": "sse-echod-" + string(rune('a'+i)), "adapter": "echod",
			"config": map[string]any{"mode": "echo"},
		}, authed())
		require.Equal(t, http.StatusCreated, respReg.StatusCode, mustJSON(body))
	}
	waitFlush(5) // 5 个 agent.status 事件落库

	// 首次连接（无 Last-Event-ID）：全量重放 5 条
	got := readSSE(t, ts.URL, 0, 5*time.Second, func(e sseEvent) bool { return e.ID >= 5 })
	require.GreaterOrEqual(t, len(got), 5, "首连应重放全部 5 条")
	assert.Equal(t, int64(5), got[len(got)-1].ID)

	// 断线后新增 3 条（再注册 3 个 agent）
	for i := 0; i < 3; i++ {
		respReg, _ := doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
			"name": "sse-echod2-" + string(rune('a'+i)), "adapter": "echod",
			"config": map[string]any{"mode": "echo"},
		}, authed())
		require.Equal(t, http.StatusCreated, respReg.StatusCode)
	}
	waitFlush(8)

	// 重连带 Last-Event-ID=5：应恰好收到 seq 6,7,8（不丢、不重复）
	replayed := readSSE(t, ts.URL, 5, 5*time.Second, func(e sseEvent) bool { return e.ID >= 8 })
	require.NotEmpty(t, replayed)
	ids := make([]int64, 0, len(replayed))
	for _, e := range replayed {
		ids = append(ids, e.ID)
		assert.Equal(t, bus.TypeAgentStatus, e.Type, "事件类型应透传")
		assert.Contains(t, e.Data, `"agent_id"`)
	}
	assert.Equal(t, []int64{6, 7, 8}, ids, "重放应恰好为 6-8，不丢不重")

	// 去重断言：无任何 id <= 5
	for _, e := range replayed {
		assert.Greater(t, e.ID, int64(5), "不得重复重放已确认事件")
	}
}

// SSE 心跳注释行与事件格式。
func TestSSEFormat(t *testing.T) {
	ts, _, store := newTestServer(t)
	respReg, _ := doReq(t, http.MethodPost, ts.URL+"/api/agents", map[string]any{
		"name": "sse-fmt", "adapter": "echod", "config": map[string]any{"mode": "echo"},
	}, authed())
	require.Equal(t, http.StatusCreated, respReg.StatusCode)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if max, _ := store.Events().MaxEventSeq(t.Context()); max >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	events := readSSE(t, ts.URL, 0, 3*time.Second, func(e sseEvent) bool { return true })
	require.NotEmpty(t, events)
	assert.Equal(t, int64(1), events[0].ID)
	assert.NotEmpty(t, events[0].Type)
	assert.True(t, json.Valid([]byte(events[0].Data)), "data 应为合法 JSON")
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
