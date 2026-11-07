package group_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/adapter/echo"
	"github.com/dddpeter/agent-nexus/internal/arbiter"
	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/internal/registry"
	"github.com/dddpeter/agent-nexus/internal/repo"
	"github.com/dddpeter/agent-nexus/internal/session/group"
)

type captureBus struct {
	mu     sync.Mutex
	events []bus.Event
}

func (c *captureBus) Publish(_ context.Context, ev bus.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	return nil
}
func (c *captureBus) Subscribe(_ ...string) (<-chan bus.Event, func()) { return nil, func() {} }
func (c *captureBus) CurrentSeq() int64                                { return 0 }
func (c *captureBus) DroppedTotal() int64                              { return 0 }
func (c *captureBus) Close() error                                     { return nil }

func (c *captureBus) of(t string) []bus.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []bus.Event
	for _, e := range c.events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

type fixture struct {
	coord *group.Coordinator
	reg   *registry.Impl
	store repo.Store
	bus   *captureBus
	// agentIDs 注册后的真实 agent id（registry nanoid；群成员 id 必须用它，Acquire 按 id 查）
	agentIDs map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := repo.NewMemoryStore()
	b := &captureBus{}
	reg, err := registry.New(store, b, []adapter.AdapterFactory{echo.NewFactory()},
		registry.Options{HeartbeatInterval: time.Hour, Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	require.NoError(t, err)
	t.Cleanup(reg.Close)

	// 两个 echod agent 作为群成员（echo 模式回显，便于断言）
	a1, err := reg.Register(context.Background(), registry.AgentSpec{Name: "db-agent", Adapter: "echod", Config: adapter.Config{"mode": "echo"}})
	require.NoError(t, err)
	a2, err := reg.Register(context.Background(), registry.AgentSpec{Name: "fe-agent", Adapter: "echod", Config: adapter.Config{"mode": "echo"}})
	require.NoError(t, err)

	coord := group.New(store, reg, b, group.Options{Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	return &fixture{coord: coord, reg: reg, store: store, bus: b,
		agentIDs: map[string]string{"db-agent": a1.ID, "fe-agent": a2.ID}}
}

func (f *fixture) dbFeMembers() []arbiter.Member {
	return []arbiter.Member{
		{ID: f.agentIDs["db-agent"], Type: arbiter.MemberAgent, Trigger: arbiter.TriggerAny, Priority: 1, Profile: "数据库 PostgreSQL 索引 SQL 优化"},
		{ID: f.agentIDs["fe-agent"], Type: arbiter.MemberAgent, Trigger: arbiter.TriggerAny, Priority: 1, Profile: "前端 Vue 界面 样式 组件"},
	}
}

func groupEvents(t *testing.T, b *captureBus, typ string) []map[string]any {
	t.Helper()
	evs := b.of(typ)
	out := make([]map[string]any, 0, len(evs))
	for _, e := range evs {
		var m map[string]any
		require.NoError(t, json.Unmarshal(e.Payload, &m))
		out = append(out, m)
	}
	return out
}

// FR3.1/3.2：建群 + @ 路由唯一响应 + 回复落库（sender=agent, reply_to 指向人消息）。
func TestCreateGroupAndAtRouting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	g, err := f.coord.CreateGroup(ctx, "评审群", f.dbFeMembers(), 0, 0)
	require.NoError(t, err)

	res, err := f.coord.SendHuman(ctx, g.ID, "@db-agent 看一下这条 SQL", "human-1")
	require.NoError(t, err)
	require.NotNil(t, res.Reply)
	assert.Equal(t, f.agentIDs["db-agent"], res.Reply.SenderID)
	assert.Equal(t, "@db-agent 看一下这条 SQL", res.Reply.Content, "echod 回显人消息")

	// 落库形状：human + agent（reply_to 指向 human）
	msgs, err := f.store.Messages().ListBySession(ctx, res.Reply.SessionID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, repo.SenderHuman, msgs[0].SenderType)
	assert.Equal(t, repo.SenderAgent, msgs[1].SenderType)
	require.NotNil(t, msgs[1].ReplyTo)
	assert.Equal(t, msgs[0].ID, mustAtoi64(t, *msgs[1].ReplyTo))

	// 决策全量落事件（F3.5）：人路由 + agent 准入各一条
	decs := groupEvents(t, f.bus, "arbiter.decision")
	require.GreaterOrEqual(t, len(decs), 2)
	assert.Equal(t, g.ID, decs[0]["group_id"])
}

func mustAtoi64(t *testing.T, s string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(s, 10, 64)
	require.NoError(t, err)
	return v
}

// FR3.2：无 @ → 相关性×priority 路由（成员画像词面重叠）。
func TestRelevanceRouting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	g, err := f.coord.CreateGroup(ctx, "混合群", f.dbFeMembers(), 0, 0)
	require.NoError(t, err)

	res, err := f.coord.SendHuman(ctx, g.ID, "数据库索引应该怎么建", "human-1")
	require.NoError(t, err)
	require.NotNil(t, res.Reply)
	assert.Equal(t, f.agentIDs["db-agent"], res.Reply.SenderID, "画像匹配 db-agent")
}

// FR3.3/3.4：预算耗尽 → 回复候选 Deferred 存档（system 消息可见）。
func TestBudgetDeferred(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// max_turns_per_topic=1：第一轮回复占满预算，第二轮候选被 Deferred
	g, err := f.coord.CreateGroup(ctx, "预算群", []arbiter.Member{
		{ID: f.agentIDs["db-agent"], Type: arbiter.MemberAgent, Trigger: arbiter.TriggerAny, Priority: 5, Profile: "数据库"},
	}, 1, 0)
	require.NoError(t, err)

	res1, err := f.coord.SendHuman(ctx, g.ID, "@db-agent 第一轮", "human-1")
	require.NoError(t, err)
	require.NotNil(t, res1.Reply, "预算内回复应发布")

	res2, err := f.coord.SendHuman(ctx, g.ID, "@db-agent 第二轮", "human-1")
	require.NoError(t, err)
	assert.Nil(t, res2.Reply, "超预算候选应 Deferred")
	require.Len(t, res2.Deferred, 1)
	assert.Equal(t, "budget_exceeded", res2.Deferred[0].Reason)

	// Deferred 存档可见（system 消息含原因与内容）
	msgs, _ := f.store.Messages().ListBySession(ctx, res2.Human.SessionID)
	require.NotEmpty(t, msgs)
	var found bool
	for _, m := range msgs {
		if m.SenderType == repo.SenderSystem && strings.Contains(m.Content, "[deferred:budget_exceeded]") {
			found = true
		}
	}
	assert.True(t, found, "Deferred 候选应以 system 消息存档")

	// 降权动作生效（FR3.3 downweight）
	st, err := f.coord.State(ctx, g.ID)
	require.NoError(t, err)
	for _, m := range st.Members {
		if m.ID == f.agentIDs["db-agent"] {
			assert.Less(t, m.Priority, 5, "超预算降权应生效")
		}
	}
}

// FR3.4：/topic 显式切换 → 预算归零；Unfreeze API 主持 人解冻。
func TestTopicResetAndUnfreeze(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	g, err := f.coord.CreateGroup(ctx, "话题群", []arbiter.Member{
		{ID: f.agentIDs["db-agent"], Type: arbiter.MemberAgent, Trigger: arbiter.TriggerAny, Priority: 5, Profile: "数据库"},
	}, 1, 0)
	require.NoError(t, err)

	// 占满预算
	_, err = f.coord.SendHuman(ctx, g.ID, "@db-agent 第一轮", "h")
	require.NoError(t, err)
	res, err := f.coord.SendHuman(ctx, g.ID, "@db-agent 第二轮", "h")
	require.NoError(t, err)
	assert.Nil(t, res.Reply)

	// /topic 重置 → 预算归零恢复发言
	_, err = f.coord.SendHuman(ctx, g.ID, "/topic 新话题", "h")
	require.NoError(t, err)
	st, err := f.coord.State(ctx, g.ID)
	require.NoError(t, err)
	assert.Empty(t, st.TurnsUsed[f.agentIDs["db-agent"]], "/topic 后预算归零")

	res3, err := f.coord.SendHuman(ctx, g.ID, "@db-agent 新话题第一轮", "h")
	require.NoError(t, err)
	require.NotNil(t, res3.Reply, "重置后可再发言")

	// 主持人解冻（未冻结时调用无害）
	require.NoError(t, f.coord.Unfreeze(ctx, g.ID))
}

// U-F6-05：快照落库 → 新协调器实例（模拟重启）恢复运行态计数。
func TestSnapshotRestore(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	g, err := f.coord.CreateGroup(ctx, "快照群", f.dbFeMembers(), 2, 4)
	require.NoError(t, err)
	_, err = f.coord.SendHuman(ctx, g.ID, "@db-agent 记一轮", "h")
	require.NoError(t, err)

	st1, err := f.coord.State(ctx, g.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, st1.TurnsUsed[f.agentIDs["db-agent"]])

	// 手动触发快照（等价 30s ticker 的一跳）
	f.coord.SnapshotAll(ctx)

	// 模拟重启：新协调器同 store 恢复
	coord2 := group.New(f.store, f.reg, f.bus, group.Options{Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	require.NoError(t, coord2.LoadPersisted(ctx))
	st2, err := coord2.State(ctx, g.ID)
	require.NoError(t, err)
	assert.Equal(t, st1.TurnsUsed, st2.TurnsUsed, "重启后预算计数恢复")
	assert.Equal(t, st1.MaxTurnsPerTopic, st2.MaxTurnsPerTopic)
	assert.Equal(t, st1.Frozen, st2.Frozen)

	// 恢复后的群可继续发言（计数延续：max_turns=2 用掉 1，还能说 1 轮）
	res, err := coord2.SendHuman(ctx, g.ID, "@db-agent 恢复后再来一轮", "h")
	require.NoError(t, err)
	require.NotNil(t, res.Reply)
}

// F3.4：被路由 agent 调用失败 → system 记录，回合释放，人可重发。
func TestAgentFailureReleasesTurn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// down 模式 agent（探活失败注册为 offline；Acquire 拒绝）
	down, err := f.reg.Register(ctx, registry.AgentSpec{Name: "down-agent", Adapter: "echod", Config: adapter.Config{"mode": "down"}})
	require.NoError(t, err)

	g, err := f.coord.CreateGroup(ctx, "故障群", []arbiter.Member{
		{ID: down.ID, Type: arbiter.MemberAgent, Trigger: arbiter.TriggerAny, Priority: 1},
	}, 0, 0)
	require.NoError(t, err)

	res, err := f.coord.SendHuman(ctx, g.ID, "@down-agent 你在吗", "h")
	require.NoError(t, err, "agent 失败不冒泡为请求错误")
	assert.Nil(t, res.Reply)

	// system 记录了失败
	msgs, _ := f.store.Messages().ListBySession(ctx, res.Human.SessionID)
	var sysMsg string
	for _, m := range msgs {
		if m.SenderType == repo.SenderSystem && strings.Contains(m.Content, "调用失败") {
			sysMsg = m.Content
		}
	}
	assert.NotEmpty(t, sysMsg, "agent 失败应有 system 记录")

	// 回合已释放：下一轮 @ 正常进入仲裁（不再 turn_occupied）
	st, err := f.coord.State(ctx, g.ID)
	require.NoError(t, err)
	assert.False(t, st.TurnOccupied)
}
