package registry_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/adapter/echo"
	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/internal/registry"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

// ---- 测试总线：内存扇出 + 事件记录 ----

type captureBus struct {
	mu    sync.Mutex
	subs  []chan bus.Event
	saved []bus.Event
}

func (c *captureBus) Publish(_ context.Context, e bus.Event) error {
	c.mu.Lock()
	c.saved = append(c.saved, e)
	subs := append([]chan bus.Event(nil), c.subs...)
	c.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- e:
		default:
		}
	}
	return nil
}

func (c *captureBus) Subscribe(_ ...string) (<-chan bus.Event, func()) {
	return make(chan bus.Event, 8), func() {}
}
func (c *captureBus) CurrentSeq() int64   { return 0 }
func (c *captureBus) DroppedTotal() int64 { return 0 }
func (c *captureBus) Close() error        { return nil }

func (c *captureBus) statusEvents(t *testing.T, from, to string) []bus.Event {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []bus.Event
	for _, e := range c.saved {
		if e.Type == bus.TypeAgentStatus &&
			(from == "" || contains(e.Payload, `"from":"`+from+`"`)) &&
			(to == "" || contains(e.Payload, `"to":"`+to+`"`)) {
			out = append(out, e)
		}
	}
	return out
}

func contains(b []byte, sub string) bool {
	return len(sub) == 0 || bytesIndex(b, sub) >= 0
}

func bytesIndex(b []byte, sub string) int {
	return bytes.Index(b, []byte(sub))
}

// ---- 可翻转健康的测试工厂（心跳用例注入）----

type flakyAdapter struct {
	mu      sync.Mutex
	healthy bool
}

func (a *flakyAdapter) Info() adapter.AgentInfo {
	return adapter.AgentInfo{Name: "flaky", Adapter: "flaky", Capabilities: adapter.DefaultCapabilities()}
}
func (a *flakyAdapter) CreateSession(_ context.Context, _ adapter.SessionOpts) (adapter.SessionID, error) {
	return "flaky-1", nil
}
func (a *flakyAdapter) Send(_ context.Context, _ adapter.SessionID, _ adapter.Message) (adapter.Reply, error) {
	return adapter.Reply{Content: "ok", Done: true}, nil
}
func (a *flakyAdapter) Stream(ctx context.Context, _ adapter.SessionID, _ adapter.Message) (<-chan adapter.StreamEvent, func(), error) {
	ch := make(chan adapter.StreamEvent, 1)
	go func() { defer close(ch); <-ctx.Done() }()
	return ch, func() {}, nil
}
func (a *flakyAdapter) Close(_ context.Context, _ adapter.SessionID) error { return nil }
func (a *flakyAdapter) HealthCheck(_ context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.healthy {
		return errFlakyDown
	}
	return nil
}

var errFlakyDown = &staticErr{"flaky: gateway unreachable"}

type staticErr struct{ msg string }

func (e *staticErr) Error() string { return e.msg }

type flakyFactory struct{ ad *flakyAdapter }

func (f *flakyFactory) Name() string                 { return "flaky" }
func (f *flakyFactory) ConfigSchema() adapter.Schema { return adapter.Schema{} }
func (f *flakyFactory) New(_ adapter.Config) (adapter.AgentAdapter, error) {
	return f.ad, nil
}

// ---- 含必填字段的测试工厂（U-F1-02）----

type reqFactory struct{}

func (f *reqFactory) Name() string { return "reqagent" }
func (f *reqFactory) ConfigSchema() adapter.Schema {
	return adapter.Schema{Fields: []adapter.SchemaField{
		{Name: "endpoint", Label: "Endpoint", Type: adapter.FieldString, Required: true},
		{Name: "api_key", Label: "API Key", Type: adapter.FieldString, Required: true, Secret: true},
	}}
}
func (f *reqFactory) New(cfg adapter.Config) (adapter.AgentAdapter, error) {
	if _, err := adapter.CoerceConfig(f.ConfigSchema(), cfg); err != nil {
		return nil, err
	}
	return &flakyAdapter{healthy: true}, nil
}

// ---- 公共构造 ----

func newTestRegistry(t *testing.T, extra ...adapter.AdapterFactory) (*registry.Impl, repo.Store, *captureBus) {
	t.Helper()
	store := repo.NewMemoryStore()
	b := &captureBus{}
	factories := append([]adapter.AdapterFactory{echo.NewFactory()}, extra...)
	reg, err := registry.New(store, b, factories, registry.Options{
		HeartbeatInterval: time.Hour, // 关闭自动心跳，测试手动触发
		Logger:            slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
	require.NoError(t, err)
	t.Cleanup(reg.Close)
	return reg, store, b
}

func basicSpec() registry.AgentSpec {
	return registry.AgentSpec{Name: "echod-main", Adapter: "echod", Config: adapter.Config{"mode": "echo"}}
}

// U-F1-01 注册合法 spec → agent 入库，status=online（echod 探活通过）。
func TestRegisterValidSpec_U_F1_01(t *testing.T) {
	reg, store, _ := newTestRegistry(t)
	ctx := context.Background()

	agent, err := reg.Register(ctx, basicSpec())
	require.NoError(t, err)
	assert.NotEmpty(t, agent.ID)
	assert.Equal(t, repo.StatusOnline, agent.Status, "echod 探活通过应自动 online")

	rec, err := store.Agents().Get(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "echod", rec.Adapter)
	assert.Equal(t, repo.StatusOnline, rec.Status)
	require.NotNil(t, rec.LastHeartbeatAt)

	got, err := reg.Get(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "echod-main", got.Name)
}

// U-F1-02 adapter 配置缺字段 → 注册拒绝，错误信息含字段名。
func TestRegisterMissingField_U_F1_02(t *testing.T) {
	reg, _, _ := newTestRegistry(t, &reqFactory{})

	_, err := reg.Register(context.Background(), registry.AgentSpec{
		Name: "req-agent", Adapter: "reqagent",
		Config: adapter.Config{"api_key": "sk-x"}, // 缺 endpoint
	})
	require.Error(t, err)
	var fe *adapter.FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, "endpoint", fe.Field, "错误信息应含字段名")
	assert.Contains(t, err.Error(), "endpoint")

	// 枚举非法值同样含字段名（echod）
	_, err = reg.Register(context.Background(), registry.AgentSpec{
		Name: "echod-bad", Adapter: "echod", Config: adapter.Config{"mode": "bogus"},
	})
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, "mode", fe.Field)
}

// U-F1-03 重名注册 → 409（ErrDuplicateName）。
func TestRegisterDuplicateName_U_F1_03(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := context.Background()

	_, err := reg.Register(ctx, basicSpec())
	require.NoError(t, err)
	_, err = reg.Register(ctx, basicSpec())
	require.ErrorIs(t, err, registry.ErrDuplicateName)
}

// U-F1-04 心跳连续 3 次失败 → offline + agent.status 事件（from=online,to=offline）。
func TestHeartbeatOffline_U_F1_04(t *testing.T) {
	flaky := &flakyAdapter{healthy: true}
	reg, store, b := newTestRegistry(t, &flakyFactory{ad: flaky})
	reg.SetOfflineThresholdForTest(3)
	ctx := context.Background()

	agent, err := reg.Register(ctx, registry.AgentSpec{
		Name: "flaky-1", Adapter: "flaky", Config: adapter.Config{},
	})
	require.NoError(t, err)
	require.Equal(t, repo.StatusOnline, agent.Status)

	flaky.SetHealthy(false)
	for i := 1; i <= 3; i++ {
		require.NoError(t, reg.Heartbeat(ctx, agent.ID), "第 %d 次心跳", i)
	}
	// 第 1、2 次失败只计数；第 3 次触发 offline
	got, _ := reg.Get(ctx, agent.ID)
	assert.Equal(t, repo.StatusOffline, got.Status, "连续 3 败应 offline")

	evs := b.statusEvents(t, "online", "offline")
	require.Len(t, evs, 1, "应恰好一次 online→offline 事件")
	assert.Contains(t, string(evs[0].Payload), `"agent_id":"`+agent.ID+`"`)
	assert.Contains(t, string(evs[0].Payload), "probe failed (3/3)")

	rec, _ := store.Agents().Get(ctx, agent.ID)
	assert.Equal(t, repo.StatusOffline, rec.Status)
}

func (a *flakyAdapter) SetHealthy(h bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.healthy = h
}

// U-F1-05 offline 后恢复探活成功 → 自动 online。
func TestHeartbeatRecover_U_F1_05(t *testing.T) {
	flaky := &flakyAdapter{healthy: true}
	reg, _, b := newTestRegistry(t, &flakyFactory{ad: flaky})
	reg.SetOfflineThresholdForTest(1)
	ctx := context.Background()

	agent, err := reg.Register(ctx, registry.AgentSpec{Name: "flaky-2", Adapter: "flaky", Config: adapter.Config{}})
	require.NoError(t, err)
	require.Equal(t, repo.StatusOnline, agent.Status)

	flaky.SetHealthy(false)
	require.NoError(t, reg.Heartbeat(ctx, agent.ID))
	got, _ := reg.Get(ctx, agent.ID)
	require.Equal(t, repo.StatusOffline, got.Status)

	flaky.SetHealthy(true)
	require.NoError(t, reg.Heartbeat(ctx, agent.ID))
	got, _ = reg.Get(ctx, agent.ID)
	assert.Equal(t, repo.StatusOnline, got.Status, "探活恢复应自动 online")
	assert.NotEmpty(t, b.statusEvents(t, "offline", "online"))
}

// U-F1-06 并发闸门：MaxConcurrency=1，两次 Acquire 并发 → 第二个阻塞；release 后放行。
func TestConcurrencyGate_U_F1_06(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := context.Background()

	one := 1
	spec := basicSpec()
	spec.Name = "echod-gate"
	spec.Capabilities = &adapter.Capabilities{MaxConcurrency: one}
	agent, err := reg.Register(ctx, spec)
	require.NoError(t, err)

	release1, err := reg.Acquire(ctx, agent.ID)
	require.NoError(t, err)

	blocked := make(chan error, 1)
	go func() {
		rel, err := reg.Acquire(ctx, agent.ID)
		blocked <- err
		if rel != nil {
			rel() // 释放占用，避免影响后续断言
		}
	}()

	select {
	case err := <-blocked:
		t.Fatalf("第二个 Acquire 不应立即返回: %v", err)
	case <-time.After(150 * time.Millisecond):
		// 阻塞中，符合预期
	}

	release1()
	select {
	case err := <-blocked:
		require.NoError(t, err, "release 后第二个 Acquire 应放行")
	case <-time.After(2 * time.Second):
		t.Fatal("release 后第二个 Acquire 超时未放行")
	}

	// 闸门满时派生 busy 展示态
	r1, _ := reg.Acquire(ctx, agent.ID)
	got, _ := reg.Get(ctx, agent.ID)
	assert.Equal(t, repo.StatusBusy, got.Status, "inFlight>=MaxConcurrency 派生 busy")
	r1()
}

// U-F1-07 MaxConcurrency=0 → 注册拒绝。
func TestMaxConcurrencyZeroRejected_U_F1_07(t *testing.T) {
	reg, _, _ := newTestRegistry(t)

	spec := basicSpec()
	spec.Name = "echod-zero"
	zero := 0
	spec.Capabilities = &adapter.Capabilities{MaxConcurrency: zero}
	_, err := reg.Register(context.Background(), spec)
	require.ErrorIs(t, err, registry.ErrBadCapabilities)
}

// U-F1-08 配置含 ${API_KEY} → 载入解析正确；日志输出脱敏。
func TestSecretEnvResolveAndMask_U_F1_08(t *testing.T) {
	require.NoError(t, os.Setenv("NEXUS_TEST_ECHOD_KEY", "sk-super-secret-123"))
	defer os.Unsetenv("NEXUS_TEST_ECHOD_KEY")

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	store := repo.NewMemoryStore()
	reg, err := registry.New(store, &captureBus{}, []adapter.AdapterFactory{echo.NewFactory()},
		registry.Options{Logger: logger})
	require.NoError(t, err)
	defer reg.Close()

	spec := basicSpec()
	spec.Config = adapter.Config{"mode": "echo", "api_key": "${NEXUS_TEST_ECHOD_KEY}"}
	agent, err := reg.Register(context.Background(), spec)
	require.NoError(t, err)

	// 载入解析正确（运行时配置为真实值）
	assert.Equal(t, "sk-super-secret-123", agent.Config["api_key"])

	// 日志脱敏：不含明文，含 ***
	out := logBuf.String()
	assert.NotContains(t, out, "sk-super-secret-123", "日志不得出现密钥明文")
	assert.Contains(t, out, "***", "密钥应脱敏为 ***")

	// 脱敏副本 API
	masked := adapter.MaskConfig(agent.Config, echo.NewFactory().ConfigSchema())
	assert.Equal(t, "***", masked["api_key"])
	assert.Equal(t, "echo", masked["mode"], "非密钥字段不脱敏")
}

// offline 期间 Acquire → ErrAgentOffline（F1.4）。
func TestAcquireOfflineRejected(t *testing.T) {
	flaky := &flakyAdapter{healthy: true}
	reg, _, _ := newTestRegistry(t, &flakyFactory{ad: flaky})
	reg.SetOfflineThresholdForTest(1)
	ctx := context.Background()

	agent, err := reg.Register(ctx, registry.AgentSpec{Name: "flaky-3", Adapter: "flaky", Config: adapter.Config{}})
	require.NoError(t, err)

	flaky.SetHealthy(false)
	require.NoError(t, reg.Heartbeat(ctx, agent.ID))

	got, _ := reg.Get(ctx, agent.ID)
	require.Equal(t, repo.StatusOffline, got.Status)
	_, err = reg.Acquire(ctx, agent.ID)
	assert.ErrorIs(t, err, registry.ErrAgentOffline)
}

// 软删：status=removed，Acquire 拒绝，List 含 removed 时可查（历史保留）。
func TestUnregisterSoftDelete(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := context.Background()

	agent, err := reg.Register(ctx, basicSpec())
	require.NoError(t, err)
	require.NoError(t, reg.Unregister(ctx, agent.ID))

	_, err = reg.Acquire(ctx, agent.ID)
	assert.ErrorIs(t, err, repo.ErrNotFound, "软删后运行时移除")

	list, err := reg.List(ctx, repo.AgentFilter{IncludeRemoved: true})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, repo.StatusRemoved, list[0].Status)
}

// 停用/启用（C-F1-05 registry 层）：disable → ErrAgentDisabled；enable 恢复。
func TestDisableEnable(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := context.Background()

	agent, err := reg.Register(ctx, basicSpec())
	require.NoError(t, err)

	require.NoError(t, reg.Disable(agent.ID))
	_, err = reg.Acquire(ctx, agent.ID)
	assert.ErrorIs(t, err, registry.ErrAgentDisabled)

	require.NoError(t, reg.Enable(ctx, agent.ID))
	release, err := reg.Acquire(ctx, agent.ID)
	require.NoError(t, err, "enable 后可 Acquire")
	release()
}

// 停用状态持久化（TODO#1）：disable 落库 → 模拟重启 LoadPersisted → 仍停用且 Acquire 拒绝；
// enable 后探活恢复 online。
func TestDisablePersistenceAcrossRestart(t *testing.T) {
	reg, store, _ := newTestRegistry(t)
	ctx := context.Background()

	agent, err := reg.Register(ctx, basicSpec())
	require.NoError(t, err)
	require.NoError(t, reg.Disable(agent.ID))

	rec, err := store.Agents().Get(ctx, agent.ID)
	require.NoError(t, err)
	assert.True(t, rec.Disabled, "disable 应落库")

	// 模拟重启：同一 store 上新建 registry 并恢复
	reg2, err := registry.New(store, &captureBus{},
		[]adapter.AdapterFactory{echo.NewFactory()},
		registry.Options{HeartbeatInterval: time.Hour, Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	require.NoError(t, err)
	t.Cleanup(reg2.Close)
	require.NoError(t, reg2.LoadPersisted(ctx))

	got, err := reg2.Get(ctx, agent.ID)
	require.NoError(t, err)
	assert.True(t, got.Disabled, "重启后停用状态应恢复")
	assert.Equal(t, repo.StatusOffline, got.Status, "停用 agent 恢复为 offline，不探活")

	_, err = reg2.Acquire(ctx, agent.ID)
	assert.ErrorIs(t, err, registry.ErrAgentDisabled, "重启后停用闸仍生效")

	// enable → 探活 → online
	require.NoError(t, reg2.Enable(ctx, agent.ID))
	got, _ = reg2.Get(ctx, agent.ID)
	assert.False(t, got.Disabled)
	assert.Equal(t, repo.StatusOnline, got.Status)

	rec, _ = store.Agents().Get(ctx, agent.ID)
	assert.False(t, rec.Disabled, "enable 应落库清除停用标记")
}

// 连通性测试（C-F1-03 registry 层）：好配置 OK；坏配置返回明确错误详情。
func TestTestConnection(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := context.Background()

	ok := reg.TestConnection(ctx, registry.AgentSpec{Adapter: "echod", Config: adapter.Config{"mode": "echo"}}, false)
	require.True(t, ok.OK)
	assert.NotZero(t, ok.DurationMs)

	bad := reg.TestConnection(ctx, registry.AgentSpec{Adapter: "echod", Config: adapter.Config{"mode": "down"}}, false)
	assert.False(t, bad.OK)
	assert.Contains(t, bad.Err, "health check failed", "错误详情应明确")

	unknown := reg.TestConnection(ctx, registry.AgentSpec{Adapter: "nope", Config: adapter.Config{}}, false)
	assert.False(t, unknown.OK)
	assert.Contains(t, unknown.Err, "unknown adapter")

	withMsg := reg.TestConnection(ctx, registry.AgentSpec{Adapter: "echod", Config: adapter.Config{"mode": "echo"}}, true)
	assert.True(t, withMsg.OK)
}

// 配置变更失败回滚（C-F1-03 registry 层）：坏配置保存被拦，旧配置生效；密钥留空沿用。
func TestUpdateConfigRollback(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := context.Background()

	agent, err := reg.Register(ctx, basicSpec())
	require.NoError(t, err)

	_, err = reg.UpdateConfig(ctx, agent.ID, adapter.Config{"mode": "down"}, false)
	require.Error(t, err, "探活失败应拒绝变更")
	assert.Contains(t, err.Error(), "rejected")

	got, _ := reg.Get(ctx, agent.ID)
	assert.Equal(t, "echo", got.Config["mode"], "旧配置应生效")
	assert.Equal(t, repo.StatusOnline, got.Status, "旧配置期间状态不变")

	// 密钥留空/脱敏占位 → 沿用旧值（C-F1-04 registry 层）
	require.NoError(t, os.Setenv("NEXUS_TEST_KEY2", "sk-old"))
	_, err = reg.UpdateConfig(ctx, agent.ID, adapter.Config{"mode": "echo", "api_key": "${NEXUS_TEST_KEY2}"}, false)
	require.NoError(t, err)
	os.Unsetenv("NEXUS_TEST_KEY2")

	_, err = reg.UpdateConfig(ctx, agent.ID, adapter.Config{"mode": "echo", "api_key": ""}, false)
	require.NoError(t, err)
	got, _ = reg.Get(ctx, agent.ID)
	assert.Equal(t, "sk-old", got.Config["api_key"], "留空表示不修改")

	_, err = reg.UpdateConfig(ctx, agent.ID, adapter.Config{"mode": "echo", "api_key": "***"}, false)
	require.NoError(t, err)
	got, _ = reg.Get(ctx, agent.ID)
	assert.Equal(t, "sk-old", got.Config["api_key"], "脱敏占位回传表示不修改")
}

// 未知 adapter / 空名拒绝。
func TestRegisterValidation(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	_, err := reg.Register(context.Background(), registry.AgentSpec{Name: "x", Adapter: "ghost"})
	assert.ErrorContains(t, err, "unknown adapter")
	_, err = reg.Register(context.Background(), registry.AgentSpec{Adapter: "echod"})
	assert.ErrorContains(t, err, "name required")
}
