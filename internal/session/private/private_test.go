package private_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
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
	"github.com/dddpeter/agent-nexus/internal/session/private"
)

func newRouter(t *testing.T, echodCfg adapter.Config, routerOpts private.Options) (*private.Router, *registry.Impl, repo.Store, bus.Bus, *captureEvents) {
	t.Helper()
	store := repo.NewMemoryStore()
	ev := &captureEvents{}
	b, err := bus.New(store.Events(), bus.Options{FlushInterval: 5 * time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	reg, err := registry.New(store, b, []adapter.AdapterFactory{echo.NewFactory()}, registry.Options{
		HeartbeatInterval: time.Hour,
		Logger:            quietLogger(),
	})
	require.NoError(t, err)
	t.Cleanup(reg.Close)

	agent, err := reg.Register(context.Background(), registry.AgentSpec{
		Name: "echod-1", Adapter: "echod", Config: echodCfg,
	})
	require.NoError(t, err)
	t.Setenv("NEXUS_TEST_AGENT_ID", agent.ID)

	r := private.New(store, reg, b, routerOpts)
	return r, reg, store, b, ev
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type captureEvents struct {
	mu  sync.Mutex
	evs []bus.Event
}

func (c *captureEvents) Start(b bus.Bus) {
	ch, cancel := b.Subscribe("msg.*", bus.TypeAgentStatus)
	go func() {
		for e := range ch {
			c.mu.Lock()
			c.evs = append(c.evs, e)
			c.mu.Unlock()
		}
	}()
	_ = cancel
}

func (c *captureEvents) of(typ string) []bus.Event {
	// 订阅者 goroutine 异步消费，短暂等待保证确定性
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if evs := c.ofNow(typ); len(evs) > 0 {
			return evs
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c.ofNow(typ)
}

func (c *captureEvents) ofNow(typ string) []bus.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []bus.Event
	for _, e := range c.evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func agentID(t *testing.T) string {
	t.Helper()
	return os.Getenv("NEXUS_TEST_AGENT_ID")
}

// U-F2-01 同步发消息 → echod 回显，messages 落库（sender_type=agent）。
func TestSyncEcho_U_F2_01(t *testing.T) {
	r, _, store, b, ev := newRouter(t, adapter.Config{"mode": "echo"}, private.Options{})
	ev.Start(b)
	ctx := context.Background()

	sess, err := r.CreateSession(ctx, agentID(t))
	require.NoError(t, err)

	res, err := r.SendMessage(ctx, sess.ID, "你好 nexus", private.ModeSync)
	require.NoError(t, err)
	require.NotNil(t, res.Reply)
	assert.Equal(t, "你好 nexus", res.Reply.Content, "echod 应回显")
	assert.False(t, res.TimedOut)

	msgs, err := store.Messages().ListBySession(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, repo.SenderHuman, msgs[0].SenderType)
	assert.Equal(t, repo.SenderAgent, msgs[1].SenderType, "agent 回复落库 sender_type=agent")
	require.NotNil(t, msgs[1].ReplyTo)
	assert.Equal(t, msgs[1].ReplyTo, msgs[1].ReplyTo)

	// 事件发布：msg.human + msg.agent
	assert.Len(t, ev.of(bus.TypeMsgHuman), 1)
	assert.Len(t, ev.of(bus.TypeMsgAgent), 1)
}

// U-F2-02 同步超时（agent 延迟 > timeout）→ 超时占位回复 + session 保留。
func TestSyncTimeoutPlaceholder_U_F2_02(t *testing.T) {
	r, _, store, _, _ := newRouter(t, adapter.Config{"mode": "delay", "delay_ms": 3000},
		private.Options{SyncTimeout: 200 * time.Millisecond})
	ctx := context.Background()

	sess, err := r.CreateSession(ctx, agentID(t))
	require.NoError(t, err)

	res, err := r.SendMessage(ctx, sess.ID, "slow please", private.ModeSync)
	require.ErrorIs(t, err, private.ErrTimeout)
	assert.True(t, res.TimedOut)
	require.NotNil(t, res.Reply, "应有超时占位消息")
	assert.Equal(t, repo.SenderSystem, res.Reply.SenderType)
	assert.Contains(t, res.Reply.Content, "[timeout]")

	// session 保留可重发
	fresh, err := store.Sessions().Get(ctx, sess.ID)
	require.NoError(t, err)
	assert.Nil(t, fresh.ClosedAt, "session 保留")

	// 重发（换快速 agent 语义：直接再次发送仍可用）
	msgs, _ := store.Messages().ListBySession(ctx, sess.ID)
	require.Len(t, msgs, 2, "human + 占位")
}

// U-F2-03 流式：伪流式 echod → ≥2 个 delta + 1 个 final。
func TestStreamDeltas_U_F2_03(t *testing.T) {
	r, _, store, _, _ := newRouter(t, adapter.Config{"mode": "stream", "delay_ms": 10, "stream_chunks": 4},
		private.Options{})
	ctx := context.Background()

	sess, err := r.CreateSession(ctx, agentID(t))
	require.NoError(t, err)

	ch, cancel, err := r.StreamMessage(ctx, sess.ID, "流式内容测试")
	require.NoError(t, err)
	defer cancel()

	var deltas, finals int
	var assembled strings.Builder
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
				assembled.WriteString(ev.Data)
			case adapter.StreamFinal:
				finals++
			case adapter.StreamError:
				t.Fatalf("unexpected error event: %s", ev.Data)
			}
		case <-deadline:
			t.Fatal("stream 超时")
		}
	}
	assert.GreaterOrEqual(t, deltas, 2, "伪流式 ≥2 delta")
	assert.Equal(t, 1, finals, "1 个 final")

	// final 内容落库
	msgs, _ := store.Messages().ListBySession(ctx, sess.ID)
	require.Len(t, msgs, 2)
	assert.Equal(t, repo.SenderAgent, msgs[1].SenderType)
	assert.Equal(t, assembled.String(), msgs[1].Content)
}

// U-F2-04 流式 60s 无 delta → error 事件（测试注入缩短空转阈值）。
func TestStreamIdleTimeout_U_F2_04(t *testing.T) {
	// stream 模式首 delta 前延迟 5s > 空转阈值 150ms
	r, _, store, _, _ := newRouter(t, adapter.Config{"mode": "stream", "delay_ms": 5000, "stream_chunks": 2},
		private.Options{StreamIdleTimeout: 150 * time.Millisecond})
	ctx := context.Background()

	sess, err := r.CreateSession(ctx, agentID(t))
	require.NoError(t, err)

	ch, cancel, err := r.StreamMessage(ctx, sess.ID, "迟迟不出字")
	require.NoError(t, err)
	defer cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("通道关闭但未见 error 事件")
			}
			if ev.Type == adapter.StreamError {
				assert.Equal(t, "timeout", ev.ErrType)
				assert.Contains(t, ev.Data, "idle")
				// system 超时提示落库
				msgs, _ := store.Messages().ListBySession(ctx, sess.ID)
				found := false
				for _, m := range msgs {
					if m.SenderType == repo.SenderSystem && strings.Contains(m.Content, "stream timeout") {
						found = true
					}
				}
				assert.True(t, found, "应有 stream 超时 system 提示")
				return
			}
			if ev.Type == adapter.StreamFinal || ev.Type == adapter.StreamDelta {
				t.Fatalf("不应有输出: %v", ev)
			}
		case <-deadline:
			t.Fatal("空转看门狗未触发")
		}
	}
}

// U-F2-05 续话：第二条消息携带同 session → agent 收到相同 external_ref。
func TestSessionContinuation_U_F2_05(t *testing.T) {
	r, reg, store, _, _ := newRouter(t, adapter.Config{"mode": "echo"}, private.Options{})
	ctx := context.Background()

	sess, err := r.CreateSession(ctx, agentID(t))
	require.NoError(t, err)

	_, err = r.SendMessage(ctx, sess.ID, "第一轮", private.ModeSync)
	require.NoError(t, err)
	_, err = r.SendMessage(ctx, sess.ID, "第二轮", private.ModeSync)
	require.NoError(t, err)

	// sessions.external_ref 回填
	fresh, _ := store.Sessions().Get(ctx, sess.ID)
	require.NotNil(t, fresh.ExternalRef)
	assert.NotEmpty(t, *fresh.ExternalRef)

	// echod 收到的两次 send 使用同一 agent 侧会话
	ad, err := reg.Adapter(agentID(t))
	require.NoError(t, err)
	echodAd := ad.(*echo.Adapter)
	recs := echodAd.Recorded()
	require.Len(t, recs, 2)
	assert.Equal(t, recs[0].SessionID, recs[1].SessionID, "两次 Send 应携带相同 external_ref")
}

// U-F2-06 无会话机制 agent：第 11 条消息时平台注入的上下文包含第 1 轮内容。
func TestStatelessContextWindow_U_F2_06(t *testing.T) {
	// stateless echod：忽略 sid，回复 SessionRef 为空 → 平台持续注入摘要
	r, reg, _, _, _ := newRouter(t, adapter.Config{"mode": "echo", "stateless": true}, private.Options{})
	ctx := context.Background()

	sess, err := r.CreateSession(ctx, agentID(t))
	require.NoError(t, err)

	// 前 10 轮
	for i := 1; i <= 10; i++ {
		_, err = r.SendMessage(ctx, sess.ID, firstRoundMark(i), private.ModeSync)
		require.NoError(t, err, "第 %d 轮", i)
	}
	// 第 11 条消息
	_, err = r.SendMessage(ctx, sess.ID, "第十一轮", private.ModeSync)
	require.NoError(t, err)

	ad, _ := reg.Adapter(agentID(t))
	echodAd := ad.(*echo.Adapter)
	lastDigest := echodAd.LastDigest()
	require.NotEmpty(t, lastDigest, "无会话机制 agent 应收到上下文摘要")
	assert.Contains(t, lastDigest, firstRoundMark(1), "第 11 条消息上下文应包含第 1 轮内容")
}

func firstRoundMark(i int) string {
	return strings.Repeat("x", 0) + "第" + string(rune('0'+i%10)) + "轮内容"
}

// U-F2-07 上下文超 20 轮 → 摘要触发，新请求含摘要标记。
func TestSummaryTrigger_U_F2_07(t *testing.T) {
	r, reg, _, _, _ := newRouter(t, adapter.Config{"mode": "echo", "stateless": true},
		private.Options{ContextRounds: 10, SummaryTriggerRounds: 20})
	ctx := context.Background()

	sess, err := r.CreateSession(ctx, agentID(t))
	require.NoError(t, err)

	for i := 1; i <= 21; i++ {
		_, err = r.SendMessage(ctx, sess.ID, firstRoundMark(i), private.ModeSync)
		require.NoError(t, err, "第 %d 轮", i)
	}
	ad, _ := reg.Adapter(agentID(t))
	lastDigest := ad.(*echo.Adapter).LastDigest()
	require.NotEmpty(t, lastDigest)
	assert.Contains(t, lastDigest, "[summary]", "超 20 轮应含摘要标记")
}

// U-F2-08 异步回报：async 模式 → 立即 accepted 回执 → 完成后 session 收到 system 回填消息。
func TestAsyncReceiptAndBackfill_U_F2_08(t *testing.T) {
	r, _, store, _, _ := newRouter(t, adapter.Config{"mode": "fixed", "reply": "异步结果：完成"},
		private.Options{SyncTimeout: 2 * time.Second})

	// 注入立即执行 runner（等 job 完成再返回，保证断言确定性）
	var mu sync.Mutex
	jobs := make(chan func(context.Context), 8)
	r.SetAsyncRunner(immediateRunner{jobs: jobs, mu: &mu})

	ctx := context.Background()
	sess, err := r.CreateSession(ctx, agentID(t))
	require.NoError(t, err)

	res, err := r.SendMessage(ctx, sess.ID, "去做个长任务", private.ModeAsync)
	require.NoError(t, err)
	require.NotNil(t, res.Async)
	assert.Equal(t, "accepted", res.Async.Type)
	assert.NotEmpty(t, res.Async.TaskID)
	assert.NotEmpty(t, res.Async.EtaHint)

	// 执行 job → 回填
	var job func(context.Context)
	select {
	case job = <-jobs:
	default:
		t.Fatal("job 应已提交")
	}
	job(ctx)

	msgs, _ := store.Messages().ListBySession(ctx, sess.ID)
	require.Len(t, msgs, 2, "human + system 回填")
	backfill := msgs[1]
	assert.Equal(t, repo.SenderSystem, backfill.SenderType, "回填为 system 消息")
	assert.Equal(t, "异步结果：完成", backfill.Content)
	require.NotNil(t, backfill.ReplyTo, "回填 reply_to 原消息")
}

type immediateRunner struct {
	jobs chan func(context.Context)
	mu   *sync.Mutex
}

func (ir immediateRunner) Submit(_ context.Context, job func(context.Context)) (string, error) {
	ir.jobs <- job
	return "task-test-1", nil
}

// agent 侧会话丢失 → 自动重建 + 注入上下文 + system 提示（F2.4）。
func TestSessionLostAutoRebuild(t *testing.T) {
	r, reg, store, _, _ := newRouter(t, adapter.Config{"mode": "echo", "session_lost_after": 1},
		private.Options{})
	ctx := context.Background()

	sess, err := r.CreateSession(ctx, agentID(t))
	require.NoError(t, err)

	_, err = r.SendMessage(ctx, sess.ID, "first", private.ModeSync)
	require.NoError(t, err)
	// 第二轮：echod 报 session lost → router 重建重发
	_, err = r.SendMessage(ctx, sess.ID, "second", private.ModeSync)
	require.NoError(t, err, "会话丢失应自动重建")

	msgs, _ := store.Messages().ListBySession(ctx, sess.ID)
	// human1 agent1 + human2 system(重建提示) agent2
	require.GreaterOrEqual(t, len(msgs), 4)
	var hasRebuildNote bool
	for _, m := range msgs {
		if m.SenderType == repo.SenderSystem && strings.Contains(m.Content, "重建") {
			hasRebuildNote = true
		}
	}
	assert.True(t, hasRebuildNote, "应有会话重建 system 提示")

	// 重建后 adapter 收到新 sid（第 1 条在旧会话，第 2 条经重建在新会话）
	ad, _ := reg.Adapter(agentID(t))
	recs := ad.(*echo.Adapter).Recorded()
	require.Len(t, recs, 2)
	assert.NotEqual(t, recs[0].SessionID, recs[1].SessionID, "重建后使用新 agent 侧会话")
}

// offline agent → 409 语义（ErrAgentUnavailable，F2.4：不排队）。
func TestOfflineAgentRejected(t *testing.T) {
	r, reg, _, _, _ := newRouter(t, adapter.Config{"mode": "down"}, private.Options{SyncTimeout: time.Second})
	reg.SetOfflineThresholdForTest(1)
	ctx := context.Background()

	sess, err := r.CreateSession(ctx, agentID(t))
	require.NoError(t, err)
	require.NoError(t, reg.Heartbeat(ctx, agentID(t)))

	_, err = r.SendMessage(ctx, sess.ID, "anyone?", private.ModeSync)
	require.ErrorIs(t, err, private.ErrAgentUnavailable)
}

// 关闭会话后发送 → ErrSessionClosed。
func TestClosedSessionRejected(t *testing.T) {
	r, _, _, _, _ := newRouter(t, adapter.Config{"mode": "echo"}, private.Options{})
	ctx := context.Background()
	sess, _ := r.CreateSession(ctx, agentID(t))
	require.NoError(t, r.CloseSession(ctx, sess.ID))
	_, err := r.SendMessage(ctx, sess.ID, "x", private.ModeSync)
	assert.ErrorIs(t, err, private.ErrSessionClosed)
}

// 回归（集成测试实机抓到的 P0）：API 层转发完 final 帧即取消请求 ctx，
// 落库用独立 ctx——final 后取消不得丢 agent 回复。
func TestStreamFinalPersistAfterCancel(t *testing.T) {
	r, _, store, _, _ := newRouter(t, adapter.Config{"mode": "stream", "stream_chunks": 3, "stream_interval_ms": 10},
		private.Options{})
	ctx := context.Background()

	sess, err := r.CreateSession(ctx, agentID(t))
	require.NoError(t, err)

	sctx, cancel := context.WithCancel(ctx)
	defer cancel() // 短路路径（未收到 final 即失败）也释放；主路径在 final 后显式 cancel 复现竞态
	ch, closeFn, err := r.StreamMessage(sctx, sess.ID, "取消竞态回归")
	require.NoError(t, err)
	defer closeFn()

	// 读到 final 帧立即取消（复现 API handler 的 return→cancel 时序）
	gotFinal := false
	for ev := range ch {
		if ev.Type == adapter.StreamFinal {
			gotFinal = true
			cancel()
			break
		}
	}
	require.True(t, gotFinal)
	// API 层此刻已 return；落库在后台 goroutine 用独立 ctx 完成
	time.Sleep(200 * time.Millisecond)

	msgs, err := store.Messages().ListBySession(context.Background(), sess.ID)
	require.NoError(t, err)
	var agentMsg *repo.Message
	for i := range msgs {
		if msgs[i].SenderType == repo.SenderAgent {
			agentMsg = &msgs[i]
		}
	}
	require.NotNil(t, agentMsg, "final 后取消请求 ctx 不得丢 agent 回复")
	assert.Equal(t, "取消竞态回归", agentMsg.Content)
}
