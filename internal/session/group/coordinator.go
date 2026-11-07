// Package group 实现群聊协调器状态机壳（F3 运行时，detailed-design F3.3）：
// 订阅人发言 → arbiter.Evaluate 路由 → 调用获准 agent → 回复经 agent 候选仲裁 → 发布；
// 全部决策落 arbiter.decision 事件（F3.5 回放调参）；Deferred 候选以 system 消息存档（仲裁可视化）。
// 群状态存内存，快照 30s 异步落库 groups.state_json，重启恢复（U-F6-05，丢失容忍）。
package group

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/arbiter"
	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/internal/id"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

// Registry 协调器所需的注册层能力（与 private.Registry 同形 + 名字解析）。
type Registry interface {
	Acquire(ctx context.Context, agentID string) (release func(), err error)
	Adapter(agentID string) (adapter.AgentAdapter, error)
	// AgentName 成员展示名（@ 令牌用注册名；人打的是名字不是 id）。
	AgentName(ctx context.Context, agentID string) (string, error)
}

// Options 协调器配置。
type Options struct {
	SyncTimeout      time.Duration // 单次 agent 调用超时（默认 300s）
	SnapshotInterval time.Duration // 快照落库周期（默认 30s，F3.3）
	SummaryMaxChars  int           // 摘要降级截断长度（默认 800）
	Logger           *slog.Logger
	Now              func() time.Time
}

func (o *Options) normalize() {
	if o.SyncTimeout <= 0 {
		o.SyncTimeout = 300 * time.Second
	}
	if o.SnapshotInterval <= 0 {
		o.SnapshotInterval = 30 * time.Second
	}
	if o.SummaryMaxChars <= 0 {
		o.SummaryMaxChars = 800
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// runtime 单群运行态。
type runtime struct {
	mu     sync.Mutex
	state  arbiter.GroupState
	g      repo.Group
	sessID string            // 群会话（kind=group）id，懒建
	names  map[string]string // 展示名/注册名 → 成员 id（@ 令牌解析用）
}

// Coordinator 群聊协调器。
type Coordinator struct {
	store repo.Store
	reg   Registry
	bus   bus.Bus
	opts  Options

	mu   sync.Mutex
	runs map[string]*runtime // groupID → runtime
}

// New 构造协调器；调用方须随后执行 LoadPersisted（重启恢复）并启动 Run（快照循环）。
func New(store repo.Store, reg Registry, b bus.Bus, opts Options) *Coordinator {
	opts.normalize()
	return &Coordinator{store: store, reg: reg, bus: b, opts: opts, runs: make(map[string]*runtime)}
}

// ---------- 群生命周期 ----------

// memberDTO 与 repo.Group.MembersJSON 的序列化形状（arbiter.Member 子集 + 名称展示）。
type memberDef struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	Trigger        string `json:"trigger,omitempty"`
	Priority       int    `json:"priority,omitempty"`
	MaxTurns       int    `json:"max_turns,omitempty"`
	InterruptLevel int    `json:"interrupt_level,omitempty"`
	Profile        string `json:"profile,omitempty"`
	Name           string `json:"name,omitempty"` // 展示名（human 成员）
}

// CreateGroup 建群（FR3.1）：成员 + 触发方式 + 预算/阈值（写入 arbiter_config_json 热可调）。
func (c *Coordinator) CreateGroup(ctx context.Context, name string, members []arbiter.Member, maxTurnsPerTopic, humanSilentThreshold int) (repo.Group, error) {
	if strings.TrimSpace(name) == "" {
		return repo.Group{}, fmt.Errorf("group: name required")
	}
	if len(members) == 0 {
		return repo.Group{}, fmt.Errorf("group: members required")
	}
	agentCount := 0
	for _, m := range members {
		if m.Type != arbiter.MemberHuman && m.Type != arbiter.MemberAgent {
			return repo.Group{}, fmt.Errorf("group: member %q type must be human|agent", m.ID)
		}
		if m.Type == arbiter.MemberAgent {
			agentCount++
		}
	}
	if agentCount == 0 {
		return repo.Group{}, fmt.Errorf("group: at least one agent member required")
	}

	gid := "grp-" + id.New()
	gs := arbiter.NewGroupState(gid, members)
	if maxTurnsPerTopic > 0 {
		gs.MaxTurnsPerTopic = maxTurnsPerTopic
	}
	if humanSilentThreshold > 0 {
		gs.HumanSilentThreshold = humanSilentThreshold
	}

	memDefs := make([]memberDef, len(members))
	names := make(map[string]string)
	for i, m := range members {
		display := m.ID
		if m.Type == arbiter.MemberAgent {
			if name, err := c.reg.AgentName(ctx, m.ID); err == nil && name != "" {
				display = name // agent 成员的 @ 令牌用注册名
			}
		}
		memDefs[i] = memberDef{ID: m.ID, Type: m.Type, Trigger: m.Trigger, Priority: m.Priority,
			MaxTurns: m.MaxTurns, InterruptLevel: m.InterruptLevel, Profile: m.Profile, Name: display}
		if display != "" {
			names[display] = m.ID
		}
	}
	memJSON, err := json.Marshal(memDefs)
	if err != nil {
		return repo.Group{}, err
	}
	cfgJSON, err := json.Marshal(map[string]int{
		"max_turns_per_topic":    gs.MaxTurnsPerTopic,
		"human_silent_threshold": gs.HumanSilentThreshold,
	})
	if err != nil {
		return repo.Group{}, err
	}
	g := repo.Group{ID: gid, Name: name, MembersJSON: string(memJSON), ArbiterConfigJSON: string(cfgJSON)}
	if err := c.store.Groups().Put(ctx, g); err != nil {
		return repo.Group{}, err
	}
	c.mu.Lock()
	c.runs[gid] = &runtime{state: gs, g: g, names: names}
	c.mu.Unlock()
	return g, nil
}

// runtimeOf 取运行态（无则从库装载）。
func (c *Coordinator) runtimeOf(ctx context.Context, groupID string) (*runtime, error) {
	c.mu.Lock()
	rt, ok := c.runs[groupID]
	c.mu.Unlock()
	if ok {
		return rt, nil
	}
	g, err := c.store.Groups().Get(ctx, groupID)
	if err != nil {
		return nil, err
	}
	rt, err = c.restoreRuntime(g)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.runs[groupID] = rt
	c.mu.Unlock()
	return rt, nil
}

// restoreRuntime 从库行重建运行态（状态快照优先，丢失容忍退回初始状态）。
func (c *Coordinator) restoreRuntime(g repo.Group) (*runtime, error) {
	var memDefs []memberDef
	if err := json.Unmarshal([]byte(g.MembersJSON), &memDefs); err != nil {
		return nil, fmt.Errorf("group %s: members_json corrupted: %w", g.ID, err)
	}
	members := make([]arbiter.Member, len(memDefs))
	for i, d := range memDefs {
		members[i] = arbiter.Member{ID: d.ID, Type: d.Type, Trigger: d.Trigger, Priority: d.Priority,
			MaxTurns: d.MaxTurns, InterruptLevel: d.InterruptLevel, Profile: d.Profile}
	}
	gs := arbiter.NewGroupState(g.ID, members)
	var thresholds struct {
		MaxTurnsPerTopic     int `json:"max_turns_per_topic"`
		HumanSilentThreshold int `json:"human_silent_threshold"`
	}
	if err := json.Unmarshal([]byte(g.ArbiterConfigJSON), &thresholds); err == nil {
		if thresholds.MaxTurnsPerTopic > 0 {
			gs.MaxTurnsPerTopic = thresholds.MaxTurnsPerTopic
		}
		if thresholds.HumanSilentThreshold > 0 {
			gs.HumanSilentThreshold = thresholds.HumanSilentThreshold
		}
	}
	if g.StateJSON != "" {
		var snap arbiter.GroupState
		if err := json.Unmarshal([]byte(g.StateJSON), &snap); err == nil && snap.GroupID == g.ID {
			// 成员/阈值以库行当前配置为准（快照可能滞后），运行态计数取快照
			snap.Members = gs.Members
			snap.MaxTurnsPerTopic = gs.MaxTurnsPerTopic
			snap.HumanSilentThreshold = gs.HumanSilentThreshold
			if snap.TurnsUsed == nil {
				snap.TurnsUsed = make(map[string]int)
			}
			gs = snap
		}
	}
	names := make(map[string]string)
	for _, d := range memDefs {
		if d.Name != "" {
			names[d.Name] = d.ID
		}
	}
	return &runtime{state: gs, g: g, names: names}, nil
}

// LoadPersisted 启动恢复（U-F6-05）：全部群装载运行态（快照优先，丢失容忍）。
func (c *Coordinator) LoadPersisted(ctx context.Context) error {
	groups, err := c.store.Groups().List(ctx)
	if err != nil {
		return err
	}
	for _, g := range groups {
		rt, err := c.restoreRuntime(g)
		if err != nil {
			c.opts.Logger.Error("group restore failed", "group", g.ID, "err", err)
			continue
		}
		c.mu.Lock()
		c.runs[g.ID] = rt
		c.mu.Unlock()
		c.opts.Logger.Info("group restored", "group_id", g.ID, "name", g.Name, "frozen", rt.state.Frozen)
	}
	return nil
}

// Run 快照落库循环（30s 异步，F3.3 / U-F6-05）；ctx 取消即退出。
func (c *Coordinator) Run(ctx context.Context) {
	t := time.NewTicker(c.opts.SnapshotInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.SnapshotAll(ctx)
		}
	}
}

// SnapshotAll 全群快照落库；单群失败记日志不阻塞（丢失容忍）。ticker 与测试共用。
func (c *Coordinator) SnapshotAll(ctx context.Context) {
	c.mu.Lock()
	runs := make([]*runtime, 0, len(c.runs))
	for _, rt := range c.runs {
		runs = append(runs, rt)
	}
	c.mu.Unlock()
	for _, rt := range runs {
		rt.mu.Lock()
		raw, err := json.Marshal(rt.state)
		rt.mu.Unlock()
		if err != nil {
			continue
		}
		if err := c.store.Groups().SaveState(ctx, rt.g.ID, string(raw)); err != nil {
			c.opts.Logger.Error("group snapshot failed", "group", rt.g.ID, "err", err)
		}
	}
}

// SessionOf 群会话 id：确定性推导（gs-<groupID>），重启后无需内存态；群不存在报错。
func (c *Coordinator) SessionOf(_ context.Context, groupID string) (string, error) {
	c.mu.Lock()
	_, ok := c.runs[groupID]
	c.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("%w: group %s", repo.ErrNotFound, groupID)
	}
	return "gs-" + groupID, nil
}

// State 返回群当前运行态快照（API 详情用）。
func (c *Coordinator) State(ctx context.Context, groupID string) (arbiter.GroupState, error) {
	rt, err := c.runtimeOf(ctx, groupID)
	if err != nil {
		return arbiter.GroupState{}, err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return cloneState(rt.state), nil
}

// UpdateConfig 阈值热更新（FR3.7）：API 修改即时生效。
func (c *Coordinator) UpdateConfig(ctx context.Context, groupID string, maxTurnsPerTopic, humanSilentThreshold int) (arbiter.GroupState, error) {
	rt, err := c.runtimeOf(ctx, groupID)
	if err != nil {
		return arbiter.GroupState{}, err
	}
	rt.mu.Lock()
	if maxTurnsPerTopic > 0 {
		rt.state.MaxTurnsPerTopic = maxTurnsPerTopic
	}
	if humanSilentThreshold > 0 {
		rt.state.HumanSilentThreshold = humanSilentThreshold
	}
	var cfg map[string]int
	_ = json.Unmarshal([]byte(rt.g.ArbiterConfigJSON), &cfg)
	if cfg == nil {
		cfg = map[string]int{}
	}
	cfg["max_turns_per_topic"] = rt.state.MaxTurnsPerTopic
	cfg["human_silent_threshold"] = rt.state.HumanSilentThreshold
	raw, _ := json.Marshal(cfg)
	rt.g.ArbiterConfigJSON = string(raw)
	rt.mu.Unlock()
	if err := c.store.Groups().Put(ctx, rt.g); err != nil {
		return arbiter.GroupState{}, err
	}
	return c.State(ctx, groupID)
}

// cloneState 深拷贝群状态（map/slice 独立）。
func cloneState(s arbiter.GroupState) arbiter.GroupState {
	out := s
	out.TurnsUsed = make(map[string]int, len(s.TurnsUsed))
	for k, v := range s.TurnsUsed {
		out.TurnsUsed[k] = v
	}
	out.Members = append([]arbiter.Member(nil), s.Members...)
	return out
}

// ---------- 发言流水线 ----------

// SendResult 人发言处理结果（与 private.SendResult 同形，UI 复用）。
type SendResult struct {
	Human    *repo.Message
	Reply    *repo.Message // 获准 agent 的回复（未路由/被拒为 nil）
	Deferred []DeferredRecord
	Frozen   bool
}

// DeferredRecord 存档候选（仲裁可视化）。
type DeferredRecord struct {
	AgentID string `json:"agent_id"`
	Content string `json:"content"`
	Reason  string `json:"reason"`
}

// mentionsOf 解析 @ 令牌 → 成员 id：@<成员id> 或 @<展示名/注册名>（人打的是名字）。
func (c *Coordinator) mentionsOf(content string, rt *runtime) []string {
	rt.mu.Lock()
	members := append([]arbiter.Member(nil), rt.state.Members...)
	names := rt.names
	rt.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	for _, m := range members {
		if m.Type != arbiter.MemberAgent {
			continue
		}
		if strings.Contains(content, "@"+m.ID) {
			if !seen[m.ID] {
				out = append(out, m.ID)
				seen[m.ID] = true
			}
			continue
		}
		for name, mid := range names {
			if mid == m.ID && strings.Contains(content, "@"+name) {
				if !seen[m.ID] {
					out = append(out, m.ID)
					seen[m.ID] = true
				}
				break
			}
		}
	}
	return out
}

// membersOf 当前成员表快照。
func (c *Coordinator) membersOf(rt *runtime) []arbiter.Member {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]arbiter.Member(nil), rt.state.Members...)
}

// SendHuman 人发言主流程（FR3.2/3.3）：
// 落库+发布 → 仲裁路由 → 调用 Target → 回复作 agent 候选再仲裁 → 落库+发布。
func (c *Coordinator) SendHuman(ctx context.Context, groupID, content, senderID string) (SendResult, error) {
	if strings.TrimSpace(content) == "" {
		return SendResult{}, fmt.Errorf("group: content required")
	}
	rt, err := c.runtimeOf(ctx, groupID)
	if err != nil {
		return SendResult{}, err
	}
	sessID, err := c.ensureGroupSession(ctx, rt)
	if err != nil {
		return SendResult{}, err
	}

	// 1) 人消息落库 + 发布
	human := repo.Message{
		SessionID: sessID, SenderType: repo.SenderHuman,
		SenderID: senderID, Content: content, CreatedAt: c.opts.Now(),
	}
	if err := c.store.Messages().Append(ctx, &human); err != nil {
		return SendResult{}, err
	}
	c.publishMessage(sessID, "", &human, "")

	// 2) 仲裁：人发言路由（@ 令牌 = 成员 id，mentionsOf 预解析）
	in := c.buildInput(rt, arbiter.Incoming{
		Kind: arbiter.IncomingHuman,
		Msg: arbiter.Message{
			ID: fmt.Sprintf("%d", human.ID), SenderID: senderID, SenderType: arbiter.MemberHuman,
			Content:  content,
			Mentions: c.mentionsOf(content, rt),
		},
	})
	d := arbiter.Evaluate(ctx, in)
	rt.mu.Lock()
	rt.state = arbiter.ApplyDecision(rt.state, in, d)
	rt.mu.Unlock()
	c.publishDecision(groupID, in, d)

	res := SendResult{Human: &human, Frozen: d.Allow == false && hasAction(d, arbiter.ActionFreeze)}

	// 3) 决策动作（解冻/话题重置/系统记录）
	if err := c.applyActions(ctx, rt, sessID, d, in); err != nil {
		c.opts.Logger.Error("group action failed", "group", groupID, "err", err)
	}
	if !d.Allow || d.Target == "" {
		return res, nil
	}

	// 4) 调用获准 agent（Invited：豁免插话通道约束）
	replyMsg, deferred, err := c.invokeAgent(ctx, rt, sessID, d.Target, content, human.ID, true, in)
	if err != nil {
		// F3.4：被路由 agent 失败 → system 记录，本回合释放
		c.appendSystem(ctx, sessID, d.Target, fmt.Sprintf("agent %s 调用失败：%v", c.memberName(rt, d.Target), err), strPtr(fmt.Sprintf("%d", human.ID)))
		rt.mu.Lock()
		rt.state.TurnOccupied = false
		rt.state.CurrentSpeaker = ""
		rt.mu.Unlock()
		return res, nil
	}
	res.Reply = replyMsg
	res.Deferred = deferred
	return res, nil
}

// invokeAgent 调用 agent 并将回复经候选仲裁落库发布；返回发布的 agent 消息与存档候选。
// invited=true 表示被人路由指名（豁免插话通道）；false 为主动候选（一期仅内部摘要调用使用）。
func (c *Coordinator) invokeAgent(ctx context.Context, rt *runtime, sessID, agentID, content string, replyTo int64, invited bool, humanIn arbiter.Input) (*repo.Message, []DeferredRecord, error) {
	release, err := c.reg.Acquire(ctx, agentID)
	if err != nil {
		return nil, nil, fmt.Errorf("acquire %s: %w", agentID, err)
	}
	defer release()
	ad, err := c.reg.Adapter(agentID)
	if err != nil {
		return nil, nil, err
	}

	digest := c.groupDigest(ctx, sessID)
	sctx, cancel := context.WithTimeout(ctx, c.opts.SyncTimeout)
	defer cancel()
	reply, err := ad.Send(sctx, adapter.SessionID("grp-"+agentID), adapter.Message{Content: content, ContextDigest: digest})
	if err != nil {
		return nil, nil, err
	}

	// agent 回复作候选仲裁（FR3.3：agent 发言必须经仲裁）
	cand := arbiter.Input{
		Group:  c.snapshotState(rt),
		Recent: c.recentArbiterMessages(ctx, sessID),
		Incoming: arbiter.Incoming{
			Kind:    arbiter.IncomingAgentCandidate,
			Msg:     arbiter.Message{SenderID: agentID, SenderType: arbiter.MemberAgent, Content: reply.Content},
			Invited: invited,
		},
	}
	d := arbiter.Evaluate(ctx, cand)
	rt.mu.Lock()
	rt.state = arbiter.ApplyDecision(rt.state, cand, d)
	rt.mu.Unlock()
	c.publishDecision(rt.g.ID, cand, d)
	_ = c.applyActions(ctx, rt, sessID, d, cand)

	if !d.Allow {
		var records []DeferredRecord
		for _, df := range d.Deferred {
			records = append(records, DeferredRecord{AgentID: df.Msg.SenderID, Content: df.Msg.Content, Reason: df.Reason})
			c.appendSystem(ctx, sessID, df.Msg.SenderID,
				fmt.Sprintf("[deferred:%s] %s：%s", df.Reason, c.memberName(rt, df.Msg.SenderID), truncate(df.Msg.Content, 300)), nil)
		}
		return nil, records, nil
	}

	agentMsg := repo.Message{
		SessionID: sessID, SenderType: repo.SenderAgent, SenderID: agentID,
		Content: reply.Content, ReplyTo: strPtr(fmt.Sprintf("%d", replyTo)), CreatedAt: c.opts.Now(),
	}
	if err := c.store.Messages().Append(ctx, &agentMsg); err != nil {
		return nil, nil, err
	}
	c.publishMessage(sessID, agentID, &agentMsg, fmt.Sprintf("%d", replyTo))
	return &agentMsg, nil, nil
}

// applyActions 执行决策动作（FR3.4/3.5）。
func (c *Coordinator) applyActions(ctx context.Context, rt *runtime, sessID string, d arbiter.Decision, in arbiter.Input) error {
	for _, a := range d.Actions {
		switch a.Kind {
		case arbiter.ActionUnfreeze:
			c.appendSystem(ctx, sessID, "nexus", "群聊已解冻（主持人操作）", nil)
		case arbiter.ActionFreeze:
			c.appendSystem(ctx, sessID, "nexus", fmt.Sprintf("群聊已冻结：%s（仅主持人 /unfreeze 可解冻）", a.Reason), nil)
		case arbiter.ActionResetTopic:
			c.appendSystem(ctx, sessID, "nexus", "话题已重置，发言预算归零", nil)
		case arbiter.ActionRecordSystem:
			// 边界记录（冻结态人消息存档、空内容等）——消息本体已落库，这里仅补事件语义
		case arbiter.ActionNotifyOwner:
			c.appendSystem(ctx, sessID, "nexus",
				fmt.Sprintf("@主持人：%s（%s）", c.memberName(rt, a.AgentID), a.Reason), nil)
		case arbiter.ActionGenerateSummary:
			c.generateSummary(ctx, rt, sessID)
		}
	}
	return nil
}

// generateSummary 摘要生成（F3.3）：调用群内预算未超的第一个 agent，失败降级为截断最近消息。
func (c *Coordinator) generateSummary(ctx context.Context, rt *runtime, sessID string) {
	gs := c.snapshotState(rt)
	var candidate string
	for _, m := range gs.Members {
		if m.Type == arbiter.MemberAgent && gs.TurnsUsed[m.ID] < gs.AgentBudget(m) {
			candidate = m.ID
			break
		}
	}
	recent := c.recentRawMessages(ctx, sessID, 20)
	if candidate == "" || len(recent) == 0 {
		c.appendSystem(ctx, sessID, "nexus", "[摘要] "+truncate(strings.Join(recent, " / "), c.opts.SummaryMaxChars), nil)
		return
	}
	release, err := c.reg.Acquire(ctx, candidate)
	if err != nil {
		c.appendSystem(ctx, sessID, "nexus", "[摘要] "+truncate(strings.Join(recent, " / "), c.opts.SummaryMaxChars), nil)
		return
	}
	defer release()
	ad, err := c.reg.Adapter(candidate)
	if err != nil {
		c.appendSystem(ctx, sessID, "nexus", "[摘要] "+truncate(strings.Join(recent, " / "), c.opts.SummaryMaxChars), nil)
		return
	}
	sctx, cancel := context.WithTimeout(ctx, c.opts.SyncTimeout)
	defer cancel()
	reply, err := ad.Send(sctx, adapter.SessionID("grp-"+candidate),
		adapter.Message{Content: "请用不超过 200 字总结以上群聊讨论的要点与结论", ContextDigest: c.groupDigest(ctx, sessID)})
	if err != nil || strings.TrimSpace(reply.Content) == "" {
		c.appendSystem(ctx, sessID, "nexus", "[摘要] "+truncate(strings.Join(recent, " / "), c.opts.SummaryMaxChars), nil)
		return
	}
	// 摘要经仲裁（空内容不占预算语义自动覆盖；正常摘要作为该 agent 的候选发言）
	cand := arbiter.Input{
		Group:  c.snapshotState(rt),
		Recent: c.recentArbiterMessages(ctx, sessID),
		Incoming: arbiter.Incoming{
			Kind: arbiter.IncomingAgentCandidate,
			Msg:  arbiter.Message{SenderID: candidate, SenderType: arbiter.MemberAgent, Content: "[摘要] " + reply.Content},
		},
	}
	d := arbiter.Evaluate(ctx, cand)
	rt.mu.Lock()
	rt.state = arbiter.ApplyDecision(rt.state, cand, d)
	rt.mu.Unlock()
	c.publishDecision(rt.g.ID, cand, d)
	if d.Allow {
		c.appendSystem(ctx, sessID, "nexus", "[摘要] "+reply.Content, nil)
	}
}

// Unfreeze 主持人解冻（FR3.4 / F3.4）。
func (c *Coordinator) Unfreeze(ctx context.Context, groupID string) error {
	rt, err := c.runtimeOf(ctx, groupID)
	if err != nil {
		return err
	}
	sessID, err := c.ensureGroupSession(ctx, rt)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	rt.state.Frozen = false
	rt.state.HumanSilentRounds = 0
	rt.mu.Unlock()
	c.appendSystem(ctx, sessID, "nexus", "群聊已解冻（主持人操作）", nil)
	return nil
}

// ResetTopic 显式话题切换（FR3.4）。
func (c *Coordinator) ResetTopic(ctx context.Context, groupID, topicID string) error {
	rt, err := c.runtimeOf(ctx, groupID)
	if err != nil {
		return err
	}
	sessID, err := c.ensureGroupSession(ctx, rt)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	rt.state.TopicID = topicID
	rt.state.TurnsUsed = make(map[string]int)
	rt.state.HumanSilentRounds = 0
	rt.mu.Unlock()
	c.appendSystem(ctx, sessID, "nexus", fmt.Sprintf("话题已切换至 %s，发言预算归零", topicID), nil)
	return nil
}

// ---------- 内部工具 ----------

// ensureGroupSession 群会话（确定性 id：gs-<groupID>）——重启后不丢历史链接。
// 懒建随机 id 曾导致重启后 runtime 丢失 sessID 映射，下一条消息开新会话、历史查不到。
func (c *Coordinator) ensureGroupSession(ctx context.Context, rt *runtime) (string, error) {
	rt.mu.Lock()
	sid := rt.sessID
	rt.mu.Unlock()
	if sid != "" {
		return sid, nil
	}
	fixed := "gs-" + rt.g.ID
	if _, err := c.store.Sessions().Get(ctx, fixed); err == nil {
		rt.mu.Lock()
		rt.sessID = fixed
		rt.mu.Unlock()
		return fixed, nil
	}
	sess := repo.Session{ID: fixed, Kind: repo.KindGroup, CreatedAt: c.opts.Now()}
	if err := c.store.Sessions().Create(ctx, sess); err != nil {
		// 并发竞态：已存在即复用
		if _, gerr := c.store.Sessions().Get(ctx, fixed); gerr == nil {
			rt.mu.Lock()
			rt.sessID = fixed
			rt.mu.Unlock()
			return fixed, nil
		}
		return "", err
	}
	rt.mu.Lock()
	rt.sessID = fixed
	rt.mu.Unlock()
	return fixed, nil
}

func (c *Coordinator) snapshotState(rt *runtime) arbiter.GroupState {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return cloneState(rt.state)
}

// buildInput 组装仲裁输入（Recent = 本群最近消息的仲裁视角）。
func (c *Coordinator) buildInput(rt *runtime, incoming arbiter.Incoming) arbiter.Input {
	ctx := context.Background()
	rt.mu.Lock()
	sessID := rt.sessID
	rt.mu.Unlock()
	var recent []arbiter.Message
	if sessID != "" {
		recent = c.recentArbiterMessages(ctx, sessID)
	}
	rt.mu.Lock()
	gs := cloneState(rt.state)
	rt.mu.Unlock()
	return arbiter.Input{Group: gs, Recent: recent, Incoming: incoming}
}

// recentArbiterMessages 群会话最近 20 条 → 仲裁消息视图。
func (c *Coordinator) recentArbiterMessages(ctx context.Context, sessID string) []arbiter.Message {
	msgs, err := c.store.Messages().RecentBySession(ctx, sessID, 20)
	if err != nil {
		return nil
	}
	out := make([]arbiter.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, arbiter.Message{
			ID: fmt.Sprintf("%d", m.ID), SenderID: m.SenderID, SenderType: m.SenderType, Content: m.Content,
		})
	}
	return out
}

// recentRawMessages 群会话最近 N 条原文（摘要降级用），格式 "sender: content"。
func (c *Coordinator) recentRawMessages(ctx context.Context, sessID string, n int) []string {
	msgs, err := c.store.Messages().RecentBySession(ctx, sessID, n)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m.SenderType == repo.SenderSystem {
			continue
		}
		who := m.SenderID
		if m.SenderType == repo.SenderHuman {
			who = "user"
		}
		out = append(out, who+": "+truncate(m.Content, 200))
	}
	return out
}

// groupDigest 群上下文摘要（无会话机制 agent 注入；与 private.buildContextDigest 同风格）。
func (c *Coordinator) groupDigest(ctx context.Context, sessID string) string {
	msgs, err := c.store.Messages().RecentBySession(ctx, sessID, 30)
	if err != nil || len(msgs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[群聊上下文摘要]\n")
	for _, m := range msgs {
		if m.SenderType == repo.SenderSystem {
			continue
		}
		who := m.SenderID
		if m.SenderType == repo.SenderHuman {
			who = "user"
		}
		b.WriteString(who + ": " + truncate(m.Content, 400) + "\n")
	}
	return b.String()
}

func (c *Coordinator) appendSystem(ctx context.Context, sessID, senderID, content string, replyTo *string) {
	m := repo.Message{
		SessionID: sessID, SenderType: repo.SenderSystem, SenderID: senderID,
		Content: content, ReplyTo: replyTo, CreatedAt: c.opts.Now(),
	}
	if err := c.store.Messages().Append(ctx, &m); err != nil {
		c.opts.Logger.Error("group system message failed", "session", sessID, "err", err)
		return
	}
	c.publishMessage(sessID, "", &m, derefStr(replyTo))
}

func (c *Coordinator) publishMessage(sessionID, agentID string, m *repo.Message, replyTo string) {
	payload := fmt.Sprintf(`{"session_id":%q,"agent_id":%q,"sender_type":%q,"sender_id":%q,"content":%q,"message_id":%d,"reply_to":%q,"group_id":%q}`,
		sessionID, agentID, m.SenderType, m.SenderID, truncate(m.Content, 4000), m.ID, replyTo, c.groupOf(sessionID))
	typ := bus.TypeMsgSystem
	switch m.SenderType {
	case repo.SenderHuman:
		typ = bus.TypeMsgHuman
	case repo.SenderAgent:
		typ = bus.TypeMsgAgent
	}
	_ = c.bus.Publish(context.Background(), bus.Event{Type: typ, Payload: []byte(payload)})
}

// groupOf 由会话 id 反查群 id（runtime 反向索引；未命中为空）。
func (c *Coordinator) groupOf(sessID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for gid, rt := range c.runs {
		rt.mu.Lock()
		sid := rt.sessID
		rt.mu.Unlock()
		if sid == sessID {
			return gid
		}
	}
	return ""
}

// publishDecision 决策全量落事件（F3.5 回放调参）。
func (c *Coordinator) publishDecision(groupID string, in arbiter.Input, d arbiter.Decision) {
	raw, err := json.Marshal(map[string]any{
		"group_id":      groupID,
		"incoming":      in.Incoming,
		"decision":      d,
		"turn_occupied": c.decisionStateView(in, d),
	})
	if err != nil {
		return
	}
	_ = c.bus.Publish(context.Background(), bus.Event{Type: bus.TypeArbiterDecision, Payload: raw})
}

func (c *Coordinator) decisionStateView(in arbiter.Input, d arbiter.Decision) map[string]any {
	return map[string]any{
		"frozen":        in.Group.Frozen,
		"turn_occupied": in.Group.TurnOccupied,
		"target":        d.Target,
	}
}

// memberName 成员展示名（names 反查注册名；无映射回退 id）。
func (c *Coordinator) memberName(rt *runtime, memberID string) string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for name, mid := range rt.names {
		if mid == memberID {
			return name
		}
	}
	return memberID
}

func hasAction(d arbiter.Decision, kind arbiter.ActionKind) bool {
	for _, a := range d.Actions {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func strPtr(s string) *string { return &s }

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
