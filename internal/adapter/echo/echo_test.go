package echo_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/adapter/echo"
	"github.com/dddpeter/agent-nexus/internal/adapter/echo/echodhttp"
	execx "github.com/dddpeter/agent-nexus/internal/adapter/exec"
)

// ---------- 进程内形态 ----------

// 进程内 echod：回显 / 固定 / 延迟 / 流式分片 / 会话失效注入。
func TestInProcessEchoModes(t *testing.T) {
	ctx := context.Background()
	f := echo.NewFactory()

	mk := func(cfg adapter.Config) *echo.Adapter {
		ad, err := f.New(cfg)
		require.NoError(t, err)
		return ad.(*echo.Adapter)
	}

	// 回显 + 探活
	a := mk(adapter.Config{"mode": "echo"})
	require.NoError(t, a.HealthCheck(ctx))
	sid, err := a.CreateSession(ctx, adapter.SessionOpts{})
	require.NoError(t, err)
	reply, err := a.Send(ctx, sid, adapter.Message{Content: "hello nexus"})
	require.NoError(t, err)
	assert.Equal(t, "hello nexus", reply.Content)
	assert.True(t, reply.Done)
	assert.Equal(t, string(sid), reply.SessionRef, "续话映射：回传 agent 侧会话")

	// 固定回复
	fixed := mk(adapter.Config{"mode": "fixed", "reply": "常量答复"})
	sid2, _ := fixed.CreateSession(ctx, adapter.SessionOpts{})
	reply, err = fixed.Send(ctx, sid2, adapter.Message{Content: "anything"})
	require.NoError(t, err)
	assert.Equal(t, "常量答复", reply.Content)

	// 伪流式：≥2 delta + 1 final（U-F2-03 前置）
	streamer := mk(adapter.Config{"mode": "stream", "delay_ms": 10, "stream_chunks": 4})
	sid3, _ := streamer.CreateSession(ctx, adapter.SessionOpts{})
	ch, cancel, err := streamer.Stream(ctx, sid3, adapter.Message{Content: "流式回显内容测试"})
	require.NoError(t, err)
	defer cancel()
	var deltas, finals int
	timeout := time.After(3 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				break loop
			}
			switch ev.Type {
			case adapter.StreamDelta:
				deltas++
			case adapter.StreamFinal:
				finals++
				assert.Equal(t, "流式回显内容测试", ev.Data)
			}
		case <-timeout:
			t.Fatal("stream 超时")
		}
	}
	assert.GreaterOrEqual(t, deltas, 2, "伪流式应 ≥2 delta")
	assert.Equal(t, 1, finals)

	// 挂起模式尊重 ctx 取消
	hang := mk(adapter.Config{"mode": "hang"})
	sid4, _ := hang.CreateSession(ctx, adapter.SessionOpts{})
	hctx, hcancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer hcancel()
	_, err = hang.Send(hctx, sid4, adapter.Message{Content: "x"})
	require.Error(t, err)

	// 会话失效注入（F2.4）
	lost := mk(adapter.Config{"mode": "echo", "session_lost_after": 1})
	sid5, _ := lost.CreateSession(ctx, adapter.SessionOpts{})
	_, err = lost.Send(ctx, sid5, adapter.Message{Content: "first"})
	require.NoError(t, err)
	_, err = lost.Send(ctx, sid5, adapter.Message{Content: "second"})
	require.ErrorIs(t, err, adapter.ErrSessionLost)

	// 记录断言（U-F2-05/06 前置能力）
	rec := a.Recorded()
	require.Len(t, rec, 1)
	assert.Equal(t, sid, rec[0].SessionID)
	assert.Equal(t, "hello nexus", rec[0].Content)
}

// ---------- CLI 形态（构建真二进制，经 execx 管控）----------

var (
	echodBinOnce sync.Once
	echodBin     string
	echodBinErr  error
)

func echodBinary(t *testing.T) string {
	t.Helper()
	echodBinOnce.Do(func() {
		bin := t.TempDir() + "/echod"
		out, err := exec.Command("go", "build", "-o", bin, "github.com/dddpeter/agent-nexus/cmd/echod").CombinedOutput()
		if err != nil {
			echodBinErr = err
			return
		}
		_ = out
		echodBin = bin
	})
	require.NoError(t, echodBinErr, "构建 echod 二进制失败")
	return echodBin
}

func runEchod(t *testing.T, args ...string) execx.Result {
	t.Helper()
	return execx.Run(context.Background(), execx.Options{Bin: echodBinary(t), Args: args})
}

// echod-cli：echo 回显 JSON、续话 session 透传。
func TestEchodCLIEcho(t *testing.T) {
	res := runEchod(t, "--mode", "echo", "-p", "hello from cli", "--session", "sess-42", "--output", "json")
	require.NoError(t, res.Err)
	var out struct {
		Reply   string `json:"reply"`
		Session string `json:"session"`
		Done    bool   `json:"done"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &out))
	assert.Equal(t, "hello from cli", out.Reply)
	assert.Equal(t, "sess-42", out.Session, "续话：--session 透传")
	assert.True(t, out.Done)
}

// echod-cli：fixed / delay / badjson / exit 故障模式。
func TestEchodCLIFaultModes(t *testing.T) {
	// fixed
	res := runEchod(t, "--mode", "fixed", "--reply", "固定", "-p", "x")
	require.NoError(t, res.Err)
	assert.Contains(t, res.Stdout, "固定")

	// delay
	start := time.Now()
	res = runEchod(t, "--mode", "delay", "--delay-ms", "300", "-p", "slow")
	require.NoError(t, res.Err)
	assert.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond)
	assert.Contains(t, res.Stdout, "slow")

	// badjson：输出不可解析
	res = runEchod(t, "--mode", "badjson", "-p", "x")
	require.NoError(t, res.Err, "畸形 JSON 退出码仍为 0")
	assert.False(t, json.Valid([]byte(strings.TrimSpace(res.Stdout))), "输出应为畸形 JSON")

	// exit：非零退出
	res = runEchod(t, "--mode", "exit", "--exit-code", "3")
	require.Error(t, res.Err)
	assert.Equal(t, 3, res.ExitCode)

	// probe：down 失败 / 正常成功
	res = runEchod(t, "--mode", "down", "--probe")
	require.Error(t, res.Err)
	assert.Equal(t, 1, res.ExitCode)
	res = runEchod(t, "--mode", "echo", "--probe")
	require.NoError(t, res.Err)
}

// echod-cli：伪流式 JSON lines。
func TestEchodCLIStream(t *testing.T) {
	res := runEchod(t, "--mode", "stream", "--reply", "一二三四五", "--chunks", "3", "--stream-interval-ms", "10")
	require.NoError(t, res.Err)
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	require.GreaterOrEqual(t, len(lines), 3)
	var deltas, finals int
	for _, ln := range lines {
		var sl struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(ln), &sl), "每行应为 JSON: %s", ln)
		switch sl.Type {
		case "delta":
			deltas++
		case "final":
			finals++
			assert.Equal(t, "一二三四五", sl.Data)
		}
	}
	assert.GreaterOrEqual(t, deltas, 2)
	assert.Equal(t, 1, finals)
}

// echod-cli 挂起模式 + 超时进程组 kill（U-F7-01 echod 实证）。
func TestEchodCLIHangGroupKill_U_F7_01(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	res := execx.Run(ctx, execx.Options{Bin: echodBinary(t), Args: []string{"--mode", "hang", "-p", "x"}})
	require.True(t, res.Killed)
	require.ErrorIs(t, res.Err, execx.ErrTimeout)
	time.Sleep(100 * time.Millisecond)
	out, err := exec.Command("sh", "-c", `ps -eo args | grep "echod --mode hang" | grep -v grep || true`).Output()
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(string(out)), "echod 挂起进程应被整组清理")
}

// ---------- HTTP 形态 ----------

func newEchodHTTPServer(t *testing.T, cfg echodhttp.Config) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(echodhttp.NewMux(cfg))
	t.Cleanup(srv.Close)
	return srv
}

func TestEchodHTTPGateway(t *testing.T) {
	srv := newEchodHTTPServer(t, echodhttp.Config{Mode: "echo"})
	base := srv.URL

	// health
	resp, err := http.Get(base + "/health")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)

	// 建会话
	resp, err = http.Post(base+"/v1/sessions", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	require.Equal(t, 201, resp.StatusCode)
	var cr struct {
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&cr))
	resp.Body.Close()
	require.NotEmpty(t, cr.SessionID)

	// 发消息 → 回显（同 session）
	payload := `{"content":"hi over http"}`
	resp, err = http.Post(base+"/v1/sessions/"+cr.SessionID+"/messages", "application/json", strings.NewReader(payload))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	var mr struct {
		Reply   string `json:"reply"`
		Session string `json:"session"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&mr))
	assert.Equal(t, "hi over http", mr.Reply)
	assert.Equal(t, cr.SessionID, mr.Session)

	// 未知会话 → 410（F2.4 session lost 翻译源）
	resp, err = http.Post(base+"/v1/sessions/ghost/messages", "application/json", strings.NewReader(payload))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusGone, resp.StatusCode)
}

func TestEchodHTTPAuthAndDown(t *testing.T) {
	// APIKey 鉴权
	srv := newEchodHTTPServer(t, echodhttp.Config{Mode: "echo", APIKey: "sk-http-test"})
	resp, err := http.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, 401, resp.StatusCode)

	req, _ := http.NewRequest("POST", srv.URL+"/v1/sessions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-http-test")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, 201, resp.StatusCode)

	// down：health 503、建会话 503
	srv2 := newEchodHTTPServer(t, echodhttp.Config{Mode: "down"})
	resp, err = http.Get(srv2.URL + "/health")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, 503, resp.StatusCode)

	resp, err = http.Post(srv2.URL+"/v1/sessions", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, 503, resp.StatusCode)
}
