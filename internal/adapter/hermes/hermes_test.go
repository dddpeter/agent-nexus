package hermes_test

import (
	"context"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/adapter/echo/echodhttp"
	"github.com/dddpeter/agent-nexus/internal/adapter/hermes"
)

func newHermesWithGateway(t *testing.T, gwCfg echodhttp.Config, cfg map[string]any) adapter.AgentAdapter {
	t.Helper()
	srv := httptest.NewServer(echodhttp.NewMux(gwCfg))
	t.Cleanup(srv.Close)
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg["base_url"] = srv.URL
	ad, err := hermes.NewFactory().New(cfg)
	require.NoError(t, err)
	return ad
}

// 健康检查：正常 / down / 不可达（探活明确错误）。
func TestHealthCheck(t *testing.T) {
	ctx := context.Background()

	ad := newHermesWithGateway(t, echodhttp.Config{Mode: "echo"}, nil)
	require.NoError(t, ad.HealthCheck(ctx))

	down := newHermesWithGateway(t, echodhttp.Config{Mode: "down"}, nil)
	err := down.HealthCheck(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503", "错误应明确指向健康检查失败")

	// 指向不存在的 endpoint（C-F1-03 的 registry 层基础）
	bad, err := hermes.NewFactory().New(map[string]any{"base_url": "http://127.0.0.1:1"})
	require.NoError(t, err)
	err = bad.HealthCheck(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unreachable", "连接拒绝应给出明确错误而非泛化 500")
}

// 配置校验：缺 base_url → 字段错误。
func TestConfigValidation(t *testing.T) {
	_, err := hermes.NewFactory().New(map[string]any{})
	require.Error(t, err)
	var fe *adapter.FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, "base_url", fe.Field)
}

// 建会话 + Send 回显 + SessionRef 回填（external_ref 语义）。
func TestSendEchoRoundtrip(t *testing.T) {
	ctx := context.Background()
	ad := newHermesWithGateway(t, echodhttp.Config{Mode: "echo"}, nil)

	sid, err := ad.CreateSession(ctx, adapter.SessionOpts{})
	require.NoError(t, err)
	reply, err := ad.Send(ctx, sid, adapter.Message{Content: "hello hermes"})
	require.NoError(t, err)
	assert.Equal(t, "hello hermes", reply.Content)
	assert.NotEmpty(t, reply.SessionRef, "应回传 gateway 侧 session id")
	assert.True(t, reply.Done)

	// 续话：同 sid 再发
	reply2, err := ad.Send(ctx, sid, adapter.Message{Content: "second turn"})
	require.NoError(t, err)
	assert.Equal(t, "second turn", reply2.Content)
}

// 网关重启丢会话 → chat 404 → 懒建新会话自愈（F2.4 语义升级：adapter 侧重建而非报错）。
func TestSessionLost(t *testing.T) {
	ctx := context.Background()

	// gateway A：建会话；随后换成同端口的 gateway B（会话表为空）模拟 gateway 重启丢会话
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()

	srvA := httptest.NewUnstartedServer(echodhttp.NewMux(echodhttp.Config{Mode: "echo"}))
	srvA.Listener = ln
	srvA.Start()
	ad, err := hermes.NewFactory().New(map[string]any{"base_url": "http://" + addr})
	require.NoError(t, err)
	sid, err := ad.CreateSession(ctx, adapter.SessionOpts{})
	require.NoError(t, err)
	reply, err := ad.Send(ctx, sid, adapter.Message{Content: "before restart"})
	require.NoError(t, err)
	require.Equal(t, string(sid), reply.SessionRef)

	srvA.Close()
	time.Sleep(50 * time.Millisecond)
	ln2, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	srvB := httptest.NewUnstartedServer(echodhttp.NewMux(echodhttp.Config{Mode: "echo"}))
	srvB.Listener = ln2
	srvB.Start()
	t.Cleanup(srvB.Close)

	// 重启后旧会话 404 → 懒建新会话重发成功；SessionRef 指向新 gateway 会话
	reply2, err := ad.Send(ctx, sid, adapter.Message{Content: "after restart"})
	require.NoError(t, err, "网关丢会话应懒建自愈而非报错")
	assert.NotEqual(t, string(sid), reply2.SessionRef, "应指向新建的 gateway 会话")
	assert.Equal(t, "after restart", reply2.Content)

	// 平台 sid（非网关会话）首条消息同样自愈：懒建 + SessionRef 回填
	reply3, err := ad.Send(ctx, "platform-sid-xyz", adapter.Message{Content: "first turn"})
	require.NoError(t, err)
	assert.Equal(t, "first turn", reply3.Content)
	assert.NotEmpty(t, reply3.SessionRef, "回填 external_ref 依据")
}

// 网关 down 时懒建也失败 → 错误明确（不吞掉）。
func TestSessionLazyCreateDownGateway(t *testing.T) {
	ctx := context.Background()
	ad := newHermesWithGateway(t, echodhttp.Config{Mode: "down"}, nil)
	_, err := ad.Send(ctx, "platform-sid-abc", adapter.Message{Content: "hi"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lazy session create", "懒建失败应明确报错而非静默")
}

// Stream：ndjson 转译 delta/final。
func TestStreamTranslation(t *testing.T) {
	ctx := context.Background()
	ad := newHermesWithGateway(t, echodhttp.Config{Mode: "stream", Chunks: 4}, nil)

	sid, err := ad.CreateSession(ctx, adapter.SessionOpts{})
	require.NoError(t, err)
	ch, cancel, err := ad.Stream(ctx, sid, adapter.Message{Content: "流式翻译测试"})
	require.NoError(t, err)
	defer cancel()

	var deltas, finals, errs int
	deadline := time.After(3 * time.Second)
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
				assert.Equal(t, "流式翻译测试", ev.Data)
			case adapter.StreamError:
				errs++
			}
		case <-deadline:
			t.Fatal("stream 超时")
		}
	}
	assert.GreaterOrEqual(t, deltas, 2)
	assert.Equal(t, 1, finals)
	assert.Zero(t, errs)
}

// 延迟网关 + 超时 ctx → 错误而非无限等（故障注入矩阵：慢响应触发调用超时）。
func TestSlowGatewayTimeout(t *testing.T) {
	ctx := context.Background()
	ad := newHermesWithGateway(t, echodhttp.Config{Mode: "delay", DelayMs: 2000},
		map[string]any{"timeout": "300ms"})
	sid, err := ad.CreateSession(ctx, adapter.SessionOpts{})
	require.NoError(t, err)
	_, err = ad.Send(ctx, sid, adapter.Message{Content: "slow"})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "慢响应应触发调用超时而非无限等")
}

// API Key：错误密钥 → unauthorized 明确错误。
func TestAPIKeyAuth(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(echodhttp.NewMux(echodhttp.Config{Mode: "echo", APIKey: "sk-right"}))
	t.Cleanup(srv.Close)

	wrong, err := hermes.NewFactory().New(map[string]any{"base_url": srv.URL, "api_key": "sk-wrong"})
	require.NoError(t, err)
	_, err = wrong.CreateSession(ctx, adapter.SessionOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unauthorized")

	right, err := hermes.NewFactory().New(map[string]any{"base_url": srv.URL, "api_key": "sk-right"})
	require.NoError(t, err)
	sid, err := right.CreateSession(ctx, adapter.SessionOpts{})
	require.NoError(t, err)
	reply, err := right.Send(ctx, sid, adapter.Message{Content: "auth ok"})
	require.NoError(t, err)
	assert.Equal(t, "auth ok", reply.Content)
}

// 三个 profile 注册为三个逻辑 agent（Info 携带 profile）。
func TestProfiles(t *testing.T) {
	for _, p := range []string{"default", "coder", "research"} {
		ad := newHermesWithGateway(t, echodhttp.Config{Mode: "echo"}, map[string]any{"profile": p})
		info := ad.Info()
		assert.Equal(t, "hermes-"+p, info.Name)
		assert.True(t, info.Capabilities.CanStream)
	}
}
