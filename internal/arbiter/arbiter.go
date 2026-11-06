// Package arbiter 实现发言仲裁器纯函数核心（F3）：输入（新消息、群状态、成员配置）→ 决策。
// 无任何 IO；全部阈值来自 GroupState（groups.arbiter_config_json 注入，API 可热更新）。
// 状态机壳（internal/session/group）负责订阅总线、状态快照与决策落事件。
package arbiter

import (
	"context"
	"sort"
	"strings"
)

// 成员类型。
const (
	MemberHuman = "human"
	MemberAgent = "agent"
)

// 触发方式。
const (
	TriggerAt      = "at"
	TriggerKeyword = "keyword"
	TriggerAny     = "any"
)

// Member 群成员配置（FR3.1）。
type Member struct {
	ID             string `json:"id"`
	Type           string `json:"type"` // human|agent
	Trigger        string `json:"trigger"`
	Priority       int    `json:"priority"`
	MaxTurns       int    `json:"max_turns,omitempty"` // 覆盖群级 max_turns_per_topic
	InterruptLevel int    `json:"interrupt_level"`     // 0=禁止插话 1=可申请 2=可直接插
	// Profile 成员关键词画像（能力/话题词面），相关性选择的数据源（F3.3）。
	Profile string `json:"profile,omitempty"`
}

// Message 仲裁输入消息（群内视角）。
type Message struct {
	ID         string   `json:"id"`
	SenderID   string   `json:"sender_id"`
	SenderType string   `json:"sender_type"`
	Content    string   `json:"content"`
	Mentions   []string `json:"mentions,omitempty"` // 上游解析好的 @ 列表
}

// GroupState 群状态（每群一份，内存持有；快照异步落库，F3.3）。
type GroupState struct {
	GroupID string  `json:"group_id"`
	Members []Member `json:"members"`
	TopicID string  `json:"topic_id"`
	Frozen  bool    `json:"frozen"`

	// 回合制状态
	TurnOccupied   bool   `json:"turn_occupied"`    // 本回合已有 agent 接话
	CurrentSpeaker string `json:"current_speaker"` // 本回合占用者

	// 预算与话题（按话题重置）
	TurnsUsed map[string]int `json:"turns_used"` // agent → 本话题已发言次数

	// 防死循环
	LastSpeakerID   string `json:"last_speaker_id"`
	LastSpeakerType string `json:"last_speaker_type"`
	HumanSilentRounds int  `json:"human_silent_rounds"` // 人连续未插话轮数

	// 阈值（全部热可调）
	MaxTurnsPerTopic     int `json:"max_turns_per_topic"`      // 默认 2
	HumanSilentThreshold int `json:"human_silent_threshold"`   // 默认 4
}

// DefaultMaxTurnsPerTopic / DefaultHumanSilentThreshold 保守默认值（F3.5 宁严勿松）。
const (
	DefaultMaxTurnsPerTopic     = 2
	DefaultHumanSilentThreshold = 4
)

// NewGroupState 构造带默认阈值的群状态。
func NewGroupState(groupID string, members []Member) GroupState {
	return GroupState{
		GroupID:              groupID,
		Members:              members,
		TurnsUsed:            make(map[string]int),
		MaxTurnsPerTopic:     DefaultMaxTurnsPerTopic,
		HumanSilentThreshold: DefaultHumanSilentThreshold,
	}
}

// AgentBudget 成员有效预算（成员覆盖 > 群级默认）。
func (g GroupState) AgentBudget(m Member) int {
	if m.MaxTurns > 0 {
		return m.MaxTurns
	}
	if g.MaxTurnsPerTopic > 0 {
		return g.MaxTurnsPerTopic
	}
	return DefaultMaxTurnsPerTopic
}

func (g GroupState) member(id string) (Member, bool) {
	for _, m := range g.Members {
		if m.ID == id {
			return m, true
		}
	}
	return Member{}, false
}

// IncomingKind 输入类型。
type IncomingKind string

const (
	IncomingHuman         IncomingKind = "human"          // 人发言（路由决策）
	IncomingAgentCandidate IncomingKind = "agent_candidate" // agent 候选（准入决策）
)

// Input 仲裁输入（detailed-design F3.3）。
type Input struct {
	Group    GroupState // 成员、预算、冻结标志
	Recent   []Message  // 本话题最近消息（相关性计算用）
	Incoming Incoming
}

// Incoming 新消息。
type Incoming struct {
	Kind IncomingKind
	Msg  Message
	// Invited=true：被人 @/相关性路由指名的应答者，豁免插话通道约束，走常规仲裁。
	Invited bool
	// InterruptRequest=true：agent 主动发起的插话申请（FR3.6），按 InterruptLevel 通道判定。
	InterruptRequest bool
}

// ActionKind 决策附带动作。
type ActionKind string

const (
	ActionFreeze          ActionKind = "freeze"           // 冻结群聊
	ActionUnfreeze        ActionKind = "unfreeze"         // 解冻
	ActionNotifyOwner     ActionKind = "notify_owner"     // @主持人/站内信
	ActionGenerateSummary ActionKind = "generate_summary" // 生成讨论摘要
	ActionResetTopic      ActionKind = "reset_topic"      // 话题重置（预算归零）
	ActionDownweight      ActionKind = "downweight"       // 超预算降权
	ActionRecordSystem    ActionKind = "record_system"    // 空内容等边界记录 system 事件
	ActionSkipBudget      ActionKind = "skip_budget"      // 不占用预算（空内容）
)

// Action 决策动作。
type Action struct {
	Kind    ActionKind `json:"kind"`
	AgentID string     `json:"agent_id,omitempty"`
	Reason  string     `json:"reason,omitempty"`
}

// DeferredMsg 存档候选（未获准消息，FR3.3）。
type DeferredMsg struct {
	Msg    Message `json:"msg"`
	Reason string  `json:"reason"`
}

// Decision 仲裁决策（technical-design §6）。
type Decision struct {
	Allow    bool          `json:"allow"`
	Target   string        `json:"target,omitempty"` // 人发言时获准接话的 agent
	Deferred []DeferredMsg `json:"deferred,omitempty"`
	Actions  []Action      `json:"actions,omitempty"`
}

// Evaluate 仲裁纯函数核心。无 IO；ctx 仅为签名契约（设计文档）保留。
//
// 处理流水线（F3.3）：
//
//	静默计数（人 N 轮未插话）→ Freeze + 摘要 + @人
//	冻结 → agent 候选一律 Deferred；人仅 /unfreeze 解冻
//	人发言 → @ 路由（唯一响应者）或 相关性×priority 选择 → Target
//	agent 候选 → InterruptLevel 通道 → 回合占用 → 链式计数（防连环）→ 预算 → 准入
func Evaluate(_ context.Context, in Input) Decision {
	gs := in.Group

	// 0) 静默检查：人连续 silent ≥ N 轮 → 强制冻结 + 摘要 + @主持人（U-F3-06）
	if !gs.Frozen && gs.HumanSilentThreshold > 0 && gs.HumanSilentRounds >= gs.HumanSilentThreshold {
		return Decision{
			Allow: false,
			Deferred: deferOne(in, "frozen_by_silence"),
			Actions: []Action{
				{Kind: ActionFreeze, Reason: "human silent rounds reached threshold"},
				{Kind: ActionGenerateSummary, Reason: "freeze summary"},
				{Kind: ActionNotifyOwner, Reason: "silent freeze, owner attention required"},
			},
		}
	}

	switch in.Incoming.Kind {
	case IncomingHuman:
		return evaluateHuman(gs, in)
	case IncomingAgentCandidate:
		return evaluateAgentCandidate(gs, in)
	default:
		return Decision{Allow: false, Deferred: deferOne(in, "unknown incoming kind")}
	}
}

func deferOne(in Input, reason string) []DeferredMsg {
	if in.Incoming.Kind == IncomingAgentCandidate {
		return []DeferredMsg{{Msg: in.Incoming.Msg, Reason: reason}}
	}
	return nil
}

// evaluateHuman 人发言路由。
func evaluateHuman(gs GroupState, in Input) Decision {
	msg := in.Incoming.Msg

	// 冻结态：仅主持人 /unfreeze 解冻；其余不路由（消息照常存档由壳处理）
	if gs.Frozen {
		if strings.HasPrefix(msg.Content, "/unfreeze") {
			return Decision{Allow: false, Actions: []Action{{Kind: ActionUnfreeze, Reason: "moderator unfreeze"}}}
		}
		return Decision{Allow: false, Deferred: nil,
			Actions: []Action{{Kind: ActionRecordSystem, Reason: "group frozen, human message archived"}}}
	}

	// /topic 显式话题切换 → 预算归零（U-F3-07）
	if strings.HasPrefix(msg.Content, "/topic") {
		return Decision{Allow: false,
			Actions: []Action{{Kind: ActionResetTopic, Reason: "explicit /topic"}}}
	}

	// @ 路由：唯一响应者（U-F3-01）
	if len(msg.Mentions) > 0 {
		for _, mention := range msg.Mentions {
			if m, ok := gs.member(mention); ok && m.Type == MemberAgent {
				return Decision{Allow: true, Target: m.ID}
			}
		}
		// @ 了不存在/非 agent 成员：不路由
		return Decision{Allow: false}
	}

	// 相关性×priority 加权选择（U-F3-02）
	target := pickByRelevance(gs, msg.Content, in.Recent)
	if target == "" {
		return Decision{Allow: false} // 无相关候选：宁可不说话（F3.5）
	}
	return Decision{Allow: true, Target: target}
}

// pickByRelevance 相关性×priority 最高者；纯 priority（相关性 0）不得当选；
// 并列时最近未发言者（TurnsUsed 最小）优先，再并列取成员表顺序。
func pickByRelevance(gs GroupState, content string, recent []Message) string {
	scorer := DefaultScorer
	type scored struct {
		member Member
		score  float64
	}
	var cands []scored
	for _, m := range gs.Members {
		if m.Type != MemberAgent {
			continue
		}
		if m.Trigger == TriggerAt {
			continue // @触发型不参与自由选择
		}
		rel := scorer.Score(content, m.Profile)
		if rel <= 0 {
			continue // 纯 priority 不当选
		}
		prio := m.Priority
		if prio <= 0 {
			prio = 1
		}
		cands = append(cands, scored{member: m, score: rel * float64(prio)})
	}
	if len(cands) == 0 {
		return ""
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		// 并列：最近未发言者优先（TurnsUsed 小者）
		return gs.TurnsUsed[cands[i].member.ID] < gs.TurnsUsed[cands[j].member.ID]
	})
	return cands[0].member.ID
}

// evaluateAgentCandidate agent 候选准入。
func evaluateAgentCandidate(gs GroupState, in Input) Decision {
	msg := in.Incoming.Msg
	m, ok := gs.member(msg.SenderID)
	if !ok || m.Type != MemberAgent {
		return Decision{Allow: false, Deferred: deferOne(in, "not an agent member")}
	}

	// 冻结 → 一律 Deferred（U-F3-09）
	if gs.Frozen {
		return Decision{Allow: false, Deferred: deferOne(in, "group_frozen")}
	}

	// 空内容发言 → 放行但不占预算（U-F3-10 / F3.4）
	if strings.TrimSpace(msg.Content) == "" {
		return Decision{Allow: true, Actions: []Action{
			{Kind: ActionSkipBudget, AgentID: m.ID, Reason: "empty content does not consume budget"},
			{Kind: ActionRecordSystem, AgentID: m.ID, Reason: "agent produced empty content"},
		}}
	}

	// 插话通道（FR3.6 / U-F3-08）：仅作用于非受邀发言
	if !in.Incoming.Invited {
		switch m.InterruptLevel {
		case 0:
			// 恒转站内信（未被指名即不得发声）
			return Decision{Allow: false, Actions: []Action{
				{Kind: ActionNotifyOwner, AgentID: m.ID, Reason: "interrupt level 0 → direct message"},
			}}
		case 2:
			// 直接放行（越过回合/连环约束，仍受冻结约束）
			return Decision{Allow: true, Actions: []Action{
				{Kind: ActionRecordSystem, AgentID: m.ID, Reason: "interrupt level 2 direct pass"},
			}}
		case 1:
			// 需仲裁批准：显式插话申请 + 有回合占用 → 转站内信；其余落入常规流水线
			if in.Incoming.InterruptRequest && gs.TurnOccupied && gs.CurrentSpeaker != m.ID {
				return Decision{Allow: false, Actions: []Action{
					{Kind: ActionNotifyOwner, AgentID: m.ID, Reason: "interrupt requested while turn occupied → direct message"},
				}}
			}
		}
	}

	// 回合占用：本回合已有接话者 → Deferred（U-F3-03）
	if gs.TurnOccupied && gs.CurrentSpeaker != m.ID {
		return Decision{Allow: false, Deferred: deferOne(in, "turn_occupied")}
	}

	// 防连环：上一条发言者是 agent 且本轮无人（human 未插话）→ 拒绝（U-F3-05）
	if gs.LastSpeakerType == MemberAgent && !gs.TurnOccupied {
		return Decision{Allow: false, Deferred: deferOne(in, "chain_guard")}
	}

	// 预算：本话题发言次数达上限 → Deferred + 降权（U-F3-04）
	if gs.TurnsUsed[msg.SenderID] >= gs.AgentBudget(m) {
		return Decision{Allow: false, Deferred: deferOne(in, "budget_exceeded"),
			Actions: []Action{{Kind: ActionDownweight, AgentID: m.ID, Reason: "budget exceeded, downweight"}}}
	}

	return Decision{Allow: true}
}

// ApplyDecision 纯状态转移：壳应用决策到群状态（返回新状态，不修改原值）。
func ApplyDecision(gs GroupState, in Input, d Decision) GroupState {
	out := gs
	out.TurnsUsed = make(map[string]int, len(gs.TurnsUsed))
	for k, v := range gs.TurnsUsed {
		out.TurnsUsed[k] = v
	}
	out.Members = append([]Member(nil), gs.Members...)

	msg := in.Incoming.Msg
	switch in.Incoming.Kind {
	case IncomingHuman:
		// 人插话：回合释放、静默计数清零
		out.HumanSilentRounds = 0
		out.TurnOccupied = false
		out.CurrentSpeaker = ""
		out.LastSpeakerID = msg.SenderID
		out.LastSpeakerType = MemberHuman
	case IncomingAgentCandidate:
		if d.Allow {
			out.LastSpeakerID = msg.SenderID
			out.LastSpeakerType = MemberAgent
			out.TurnOccupied = true
			out.CurrentSpeaker = msg.SenderID
			skip := false
			for _, a := range d.Actions {
				if a.Kind == ActionSkipBudget {
					skip = true
				}
			}
			if !skip {
				out.TurnsUsed[msg.SenderID]++
			}
		} else {
			// 未获准：静默轮计数只在「本应轮到人但人没说话」时增长 —— 由壳按回合推进；
			// 这里在候选被拒且上一条为 agent 时累加（人仍未插话）
			if gs.LastSpeakerType == MemberAgent {
				out.HumanSilentRounds = gs.HumanSilentRounds + 1
			}
		}
	}

	for _, a := range d.Actions {
		switch a.Kind {
		case ActionFreeze:
			out.Frozen = true
			out.HumanSilentRounds = 0
		case ActionUnfreeze:
			out.Frozen = false
		case ActionResetTopic:
			out.TurnsUsed = make(map[string]int)
			out.HumanSilentRounds = 0
		case ActionDownweight:
			for i := range out.Members {
				if out.Members[i].ID == a.AgentID && out.Members[i].Priority > 1 {
					out.Members[i].Priority--
				}
			}
		}
	}
	return out
}
