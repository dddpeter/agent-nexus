package arbiter_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dddpeter/agent-nexus/internal/arbiter"
)

// 群夹具：主持人 + 三个 agent（A 相关性高/常规优先级，B 低相关/高优先级，C 备选）。
func fixtureGroup() arbiter.GroupState {
	members := []arbiter.Member{
		{ID: "host", Type: arbiter.MemberHuman, Trigger: arbiter.TriggerAny, Priority: 10},
		{ID: "agent-a", Type: arbiter.MemberAgent, Trigger: arbiter.TriggerKeyword, Priority: 1, InterruptLevel: 1,
			Profile: "Go 语言 并发 编程 channel"},
		{ID: "agent-b", Type: arbiter.MemberAgent, Trigger: arbiter.TriggerKeyword, Priority: 5, InterruptLevel: 1,
			Profile: "天气 闲聊 美食"},
		{ID: "agent-c", Type: arbiter.MemberAgent, Trigger: arbiter.TriggerKeyword, Priority: 1, InterruptLevel: 1,
			Profile: "Go 语言 并发 编程 channel"},
	}
	return arbiter.NewGroupState("g-test", members)
}

func humanMsg(content string, mentions ...string) arbiter.Incoming {
	return arbiter.Incoming{Kind: arbiter.IncomingHuman, Msg: arbiter.Message{
		ID: "m1", SenderID: "host", SenderType: arbiter.MemberHuman, Content: content, Mentions: mentions,
	}}
}

func agentMsg(id, content string) arbiter.Incoming {
	return arbiter.Incoming{Kind: arbiter.IncomingAgentCandidate, Msg: arbiter.Message{
		ID: "m2", SenderID: id, SenderType: arbiter.MemberAgent, Content: content,
	}}
}

// interruptMsg 显式插话申请（FR3.6）。
func interruptMsg(id, content string) arbiter.Incoming {
	in := agentMsg(id, content)
	in.InterruptRequest = true
	return in
}

func eval(t *testing.T, gs arbiter.GroupState, in arbiter.Incoming, recent []arbiter.Message) arbiter.Decision {
	t.Helper()
	return arbiter.Evaluate(context.Background(), arbiter.Input{Group: gs, Recent: recent, Incoming: in})
}

func hasAction(d arbiter.Decision, kind arbiter.ActionKind) bool {
	for _, a := range d.Actions {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

func deferredOf(d arbiter.Decision, agentID string) *arbiter.DeferredMsg {
	for i := range d.Deferred {
		if d.Deferred[i].Msg.SenderID == agentID {
			return &d.Deferred[i]
		}
	}
	return nil
}

// U-F3-01 人 @A → 仅 A 获准；B、C 的候选被 Deferred。
func TestMentionRouting_U_F3_01(t *testing.T) {
	gs := fixtureGroup()

	// 人 @agent-a
	d := eval(t, gs, humanMsg("帮我看看这个问题 @agent-a", "agent-a"), nil)
	require.True(t, d.Allow)
	assert.Equal(t, "agent-a", d.Target, "@ 谁谁接话")

	// A 接话后，B/C 候选被 Deferred（回合占用）
	gs.TurnOccupied = true
	gs.CurrentSpeaker = "agent-a"
	db := eval(t, gs, agentMsg("agent-b", "我也要说"), nil)
	require.False(t, db.Allow)
	require.NotNil(t, deferredOf(db, "agent-b"), "B 候选应 Deferred")
	dc := eval(t, gs, agentMsg("agent-c", "我也要说"), nil)
	require.False(t, dc.Allow)
	require.NotNil(t, deferredOf(dc, "agent-c"), "C 候选应 Deferred")
}

// U-F3-02 人未 @ → 相关性×priority 加权最高者接话（纯 priority 不当选）；并列时最近未发言者优先。
func TestRelevancePriority_U_F3_02(t *testing.T) {
	gs := fixtureGroup()

	recent := []arbiter.Message{
		{ID: "r1", SenderType: arbiter.MemberHuman, Content: "讨论一下 Go 语言并发编程的最佳实践"},
	}

	// agent-a 与话题高相关（词面重叠），agent-b 优先级更高但不相关
	d := eval(t, gs, humanMsg("Go 并发编程里 channel 怎么用"), recent)
	require.True(t, d.Allow)
	assert.Equal(t, "agent-a", d.Target, "相关性主导：词面重叠者当选，纯 priority 者落选")

	// 不相关消息：无人当选（宁可不说话）——注：天气类消息会命中 B 的画像属正常路由
	d = eval(t, gs, humanMsg("量子引力波探测的最新进展"), recent)
	assert.False(t, d.Allow, "无相关候选不应路由")

	// 并列（同相关同优先级 A/C）：最近未发言者（TurnsUsed 小者）优先
	gs2 := fixtureGroup()
	gs2.TurnsUsed["agent-a"] = 1 // A 已发言过
	recent2 := []arbiter.Message{
		{ID: "r1", SenderType: arbiter.MemberHuman, Content: "Go 语言 并发 编程 实践"},
	}
	d = eval(t, gs2, humanMsg("Go 语言并发编程实践问题"), recent2)
	require.True(t, d.Allow)
	assert.Equal(t, "agent-c", d.Target, "并列时最近未发言者优先")
}

// U-F3-03 回合占用：A 接话未结束，B 候选 → Deferred。
func TestTurnOccupancy_U_F3_03(t *testing.T) {
	gs := fixtureGroup()
	gs.TurnOccupied = true
	gs.CurrentSpeaker = "agent-a"

	d := eval(t, gs, agentMsg("agent-b", "插一嘴"), nil)
	require.False(t, d.Allow)
	def := deferredOf(d, "agent-b")
	require.NotNil(t, def)
	assert.Equal(t, "turn_occupied", def.Reason)
}

// U-F3-04 预算：A 本话题已发言 2 次（默认预算）→ 再次候选被 Deferred + 降权。
func TestBudget_U_F3_04(t *testing.T) {
	gs := fixtureGroup()
	gs.TurnsUsed["agent-a"] = 2 // 默认预算 2
	// 无回合占用、上一位是人（不触发连环守卫）
	gs.LastSpeakerType = arbiter.MemberHuman

	d := eval(t, gs, agentMsg("agent-a", "我还想说"), nil)
	require.False(t, d.Allow)
	def := deferredOf(d, "agent-a")
	require.NotNil(t, def)
	assert.Equal(t, "budget_exceeded", def.Reason)
	assert.True(t, hasAction(d, arbiter.ActionDownweight), "超预算应降权")

	// ApplyDecision 后优先级下降
	applied := arbiter.ApplyDecision(gs, arbiter.Input{Group: gs, Incoming: agentMsg("agent-a", "我还想说")}, d)
	for _, m := range applied.Members {
		if m.ID == "agent-a" {
			assert.Equal(t, 1, m.Priority, "降权后 priority 5→… 不适用：A 原 priority=1 保持下限")
		}
	}
}

// U-F3-05 防连环：上一条为 agent 发言且本轮无人插话 → 其他 agent 候选全部拒绝。
func TestChainGuard_U_F3_05(t *testing.T) {
	gs := fixtureGroup()
	gs.LastSpeakerType = arbiter.MemberAgent
	gs.LastSpeakerID = "agent-a"
	// 本轮无人：TurnOccupied=false（上一轮已收尾）

	for _, id := range []string{"agent-b", "agent-c"} {
		d := eval(t, gs, agentMsg(id, "接着说"), nil)
		require.False(t, d.Allow, "%s 不得被触发（防连环）", id)
		def := deferredOf(d, id)
		require.NotNil(t, def)
		assert.Equal(t, "chain_guard", def.Reason)
	}
}

// U-F3-06 静默 4 轮 → Freeze Action + 摘要 Action + NotifyOwner。
func TestSilenceFreeze_U_F3_06(t *testing.T) {
	gs := fixtureGroup()
	gs.HumanSilentRounds = 4 // 人连续 4 轮未插话

	d := eval(t, gs, agentMsg("agent-a", "继续输出"), nil)
	require.False(t, d.Allow)
	assert.True(t, hasAction(d, arbiter.ActionFreeze), "应含 Freeze")
	assert.True(t, hasAction(d, arbiter.ActionGenerateSummary), "应含摘要")
	assert.True(t, hasAction(d, arbiter.ActionNotifyOwner), "应含 @主持人")

	// 应用后群冻结
	applied := arbiter.ApplyDecision(gs, arbiter.Input{Group: gs, Incoming: agentMsg("agent-a", "x")}, d)
	assert.True(t, applied.Frozen)
	assert.Equal(t, 0, applied.HumanSilentRounds, "冻结后静默计数清零")
}

// U-F3-07 话题重置（显式 /topic）→ 预算计数归零。
func TestTopicReset_U_F3_07(t *testing.T) {
	gs := fixtureGroup()
	gs.TurnsUsed["agent-a"] = 2
	gs.TurnsUsed["agent-b"] = 1

	d := eval(t, gs, humanMsg("/topic 新话题：部署方案"), nil)
	require.False(t, d.Allow, "/topic 是控制命令，不路由")
	require.True(t, hasAction(d, arbiter.ActionResetTopic))

	applied := arbiter.ApplyDecision(gs, arbiter.Input{Group: gs, Incoming: humanMsg("/topic 新话题")}, d)
	assert.Equal(t, map[string]int{}, applied.TurnsUsed, "预算计数应归零")

	// 归零后同预算 agent 可再次发言
	gs2 := applied
	gs2.LastSpeakerType = arbiter.MemberHuman
	d2 := eval(t, gs2, agentMsg("agent-a", "新话题我来"), nil)
	assert.True(t, d2.Allow, "话题重置后预算恢复")
}

// U-F3-08 插话：InterruptLevel=2 直接放行；=1 且无回合占用放行，有占用转站内信；=0 恒转站内信。
func TestInterruptLevels_U_F3_08(t *testing.T) {
	// IL=2：有回合占用也直接放行
	gs := fixtureGroup()
	gs.Members = append(gs.Members[:0],
		gs.Members[0],
		arbiter.Member{ID: "agent-l2", Type: arbiter.MemberAgent, Trigger: arbiter.TriggerKeyword, Priority: 1, InterruptLevel: 2},
	)
	gs.TurnOccupied = true
	gs.CurrentSpeaker = "agent-a"
	d := eval(t, gs, interruptMsg("agent-l2", "紧急插话"), nil)
	assert.True(t, d.Allow, "IL=2 应直接放行")

	// IL=1 且无占用 → 放行
	gs2 := fixtureGroup()
	gs2.Members = append(gs2.Members,
		arbiter.Member{ID: "agent-l1", Type: arbiter.MemberAgent, Trigger: arbiter.TriggerKeyword, Priority: 1, InterruptLevel: 1},
	)
	gs2.TurnOccupied = false
	d = eval(t, gs2, interruptMsg("agent-l1", "申请插话"), nil)
	assert.True(t, d.Allow, "IL=1 无占用应放行")

	// IL=1 且有占用 → 转站内信（NotifyOwner）
	gs3 := fixtureGroup()
	gs3.Members = append(gs3.Members,
		arbiter.Member{ID: "agent-l1", Type: arbiter.MemberAgent, Trigger: arbiter.TriggerKeyword, Priority: 1, InterruptLevel: 1},
	)
	gs3.TurnOccupied = true
	gs3.CurrentSpeaker = "agent-a"
	d = eval(t, gs3, interruptMsg("agent-l1", "申请插话"), nil)
	assert.False(t, d.Allow)
	assert.True(t, hasAction(d, arbiter.ActionNotifyOwner), "IL=1 有占用应转站内信")
	assert.Empty(t, d.Deferred, "转站内信不是 Deferred")

	// IL=0 → 恒转站内信
	gs4 := fixtureGroup()
	gs4.Members = append(gs4.Members,
		arbiter.Member{ID: "agent-l0", Type: arbiter.MemberAgent, Trigger: arbiter.TriggerKeyword, Priority: 1, InterruptLevel: 0},
	)
	d = eval(t, gs4, interruptMsg("agent-l0", "想说话"), nil)
	assert.False(t, d.Allow)
	assert.True(t, hasAction(d, arbiter.ActionNotifyOwner), "IL=0 恒转站内信")
	// IL=0 常规候选（非插话申请）同样不得自发声
	d2 := eval(t, gs4, agentMsg("agent-l0", "自说自话"), nil)
	assert.False(t, d2.Allow)
	assert.True(t, hasAction(d2, arbiter.ActionNotifyOwner))
}

// U-F3-09 冻结状态：仅主持人发言解冻；agent 候选一律 Deferred。
func TestFrozenState_U_F3_09(t *testing.T) {
	gs := fixtureGroup()
	gs.Frozen = true

	// agent 候选一律 Deferred
	for _, id := range []string{"agent-a", "agent-b", "agent-c"} {
		d := eval(t, gs, agentMsg(id, "我还能说吗"), nil)
		require.False(t, d.Allow)
		def := deferredOf(d, id)
		require.NotNil(t, def, "%s 应被 Deferred", id)
		assert.Equal(t, "group_frozen", def.Reason)
	}

	// 普通人发言不解冻（不路由）
	d := eval(t, gs, humanMsg("大家继续"), nil)
	assert.False(t, d.Allow)

	// 仅 /unfreeze 解冻
	d = eval(t, gs, humanMsg("/unfreeze"), nil)
	require.True(t, hasAction(d, arbiter.ActionUnfreeze))
	applied := arbiter.ApplyDecision(gs, arbiter.Input{Group: gs, Incoming: humanMsg("/unfreeze")}, d)
	assert.False(t, applied.Frozen)
}

// U-F3-10 空内容发言 → 不占预算。
func TestEmptyContentNoBudget_U_F3_10(t *testing.T) {
	gs := fixtureGroup()
	gs.LastSpeakerType = arbiter.MemberHuman

	d := eval(t, gs, agentMsg("agent-a", "   "), nil)
	assert.True(t, d.Allow, "空内容放行（边界）")
	assert.True(t, hasAction(d, arbiter.ActionSkipBudget), "不占预算")
	assert.True(t, hasAction(d, arbiter.ActionRecordSystem), "记 system 事件")

	applied := arbiter.ApplyDecision(gs, arbiter.Input{Group: gs, Incoming: agentMsg("agent-a", "")}, d)
	assert.Equal(t, 0, applied.TurnsUsed["agent-a"], "空内容不消耗预算")
	assert.Equal(t, "agent-a", applied.CurrentSpeaker, "仍占用回合（避免抢话）")
}

// 回归：人插话重置静默计数与回合占用（状态转移）。
func TestHumanInterjectResets(t *testing.T) {
	gs := fixtureGroup()
	gs.HumanSilentRounds = 3
	gs.TurnOccupied = true
	gs.CurrentSpeaker = "agent-a"

	d := eval(t, gs, humanMsg("我来接一句"), nil)
	applied := arbiter.ApplyDecision(gs, arbiter.Input{Group: gs, Incoming: humanMsg("我来接一句")}, d)
	assert.Equal(t, 0, applied.HumanSilentRounds, "人插话清零静默计数")
	assert.False(t, applied.TurnOccupied, "人插话释放回合")
	assert.Equal(t, arbiter.MemberHuman, applied.LastSpeakerType)
}

// 纯函数性质：同输入同输出、不改输入（无 IO 回归安全网基础）。
func TestPurity(t *testing.T) {
	gs := fixtureGroup()
	gs.TurnsUsed["agent-a"] = 1
	in := arbiter.Input{Group: gs, Recent: nil, Incoming: agentMsg("agent-a", "确定性")}

	d1 := arbiter.Evaluate(context.Background(), in)
	d2 := arbiter.Evaluate(context.Background(), in)
	assert.Equal(t, d1, d2, "同输入同输出")

	// 输入状态不被修改
	assert.Equal(t, 1, gs.TurnsUsed["agent-a"])
	assert.False(t, gs.Frozen)
	// ApplyDecision 不改原状态
	applied := arbiter.ApplyDecision(gs, in, d1)
	assert.Equal(t, 1, gs.TurnsUsed["agent-a"], "原状态不可变")
	if d1.Allow {
		assert.Equal(t, 2, applied.TurnsUsed["agent-a"], "新状态预算+1")
	}
}
