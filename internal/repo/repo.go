// Package repo 收敛全部 SQL 访问（technical-design §2：唯一 SQL 所在地）。
// 接口按聚合划分，提供 SQLite 实现与内存测试实现。
package repo

import (
	"context"
	"errors"
	"time"

	"github.com/dddpeter/agent-nexus/internal/bus"
)

// 常见错误。
var (
	ErrNotFound      = errors.New("repo: not found")
	ErrDuplicateName = errors.New("repo: duplicate agent name")
)

// Agent 状态（technical-design §3 agents.status 注释）。
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
	StatusBusy    = "busy"
	StatusRemoved = "removed"
)

// 会话类型。
const (
	KindPrivate = "private"
	KindGroup   = "group"
	KindTask    = "task"
)

// 消息发送者类型。
const (
	SenderHuman  = "human"
	SenderAgent  = "agent"
	SenderSystem = "system"
)

// Agent 是 agents 表的存储映照；config/capabilities 以 JSON 文本存储，
// 由 registry 层负责与 adapter.Config / adapter.Capabilities 互转（repo 不感知适配层）。
type Agent struct {
	ID               string
	Name             string
	Adapter          string
	ConfigJSON       string
	CapabilitiesJSON string
	Status           string
	LastHeartbeatAt  *time.Time
}

// Session 是 sessions 表的存储映照。
// 群会话（Kind=group）AgentID/ExternalRef/TopicID 语义见 technical-design §3 注释：
// agent_id 仅私聊/任务会话有值；群会话为 NULL（成员见 groups.members_json）。
type Session struct {
	ID          string
	AgentID     *string
	Kind        string
	ExternalRef *string
	TopicID     *string
	CreatedAt   time.Time
	ClosedAt    *time.Time
}

// Message 是 messages 表的存储映照（只做存档检索，实时流转走内存总线）。
type Message struct {
	ID         int64
	SessionID  string
	SenderType string // human|agent|system
	SenderID   string
	Content    string
	ReplyTo    *string
	CreatedAt  time.Time
}

// Group 是 groups 表的存储映照。
type Group struct {
	ID                string
	Name              string
	MembersJSON       string
	ArbiterConfigJSON string
}

// Task 是 tasks 表的存储映照（节点级状态一期内嵌 nodes_json）。
type Task struct {
	ID             string
	WorkflowID     *string
	AgentID        *string
	Input          *string
	Status         string // queued|running|done|failed|cancelled
	Result         *string
	Error          *string
	NodesJSON      *string
	TimeoutSeconds *int
	Retries        *int
	CreatedAt      *time.Time
	FinishedAt     *time.Time
}

// Task 状态机（detailed-design F4.3）。
const (
	TaskQueued    = "queued"
	TaskRunning   = "running"
	TaskDone      = "done"
	TaskFailed    = "failed"
	TaskCancelled = "cancelled"
)

// AgentFilter 列表过滤。
type AgentFilter struct {
	Status         string // 精确匹配；空 = 任意
	Adapter        string // 精确匹配；空 = 任意
	IncludeRemoved bool   // 默认排除 status=removed
}

// AgentRepo agents 聚合。
type AgentRepo interface {
	Create(ctx context.Context, a Agent) error
	Get(ctx context.Context, id string) (Agent, error)
	GetByName(ctx context.Context, name string) (Agent, error)
	List(ctx context.Context, f AgentFilter) ([]Agent, error)
	// Update 全量更新（name/adapter/创建后不可变字段以外均可变）。
	Update(ctx context.Context, a Agent) error
}

// SessionRepo sessions 聚合。
type SessionRepo interface {
	Create(ctx context.Context, s Session) error
	Get(ctx context.Context, id string) (Session, error)
	Update(ctx context.Context, s Session) error
	ListByAgent(ctx context.Context, agentID string) ([]Session, error)
}

// MessageRepo messages 聚合（异步写入，存档检索）。
type MessageRepo interface {
	// Append 写入并回填自增 ID。
	Append(ctx context.Context, m *Message) error
	ListBySession(ctx context.Context, sessionID string) ([]Message, error)
	// RecentBySession 按 created_at 倒序取最近 limit 条（索引 idx_messages_session）。
	RecentBySession(ctx context.Context, sessionID string, limit int) ([]Message, error)
}

// GroupRepo groups 聚合。
type GroupRepo interface {
	Put(ctx context.Context, g Group) error
	Get(ctx context.Context, id string) (Group, error)
	List(ctx context.Context) ([]Group, error)
}

// TaskRepo tasks 聚合。
type TaskRepo interface {
	Create(ctx context.Context, t Task) error
	Get(ctx context.Context, id string) (Task, error)
	Update(ctx context.Context, t Task) error
	List(ctx context.Context) ([]Task, error)
}

// EventRepo event_log 聚合；实现同时满足 bus.Sink（AppendEvents/MaxEventSeq）。
type EventRepo interface {
	// AppendEvents 批量追加；Event.Seq 显式写入（bus 在发布时已分配，保证与内存一致）。
	AppendEvents(ctx context.Context, events []bus.Event) error
	MaxEventSeq(ctx context.Context) (int64, error)
	// EventsAfter 返回 seq > afterSeq 的事件（升序，至多 limit 条）——SSE 断线补偿重放数据源。
	EventsAfter(ctx context.Context, afterSeq int64, limit int) ([]bus.Event, error)
}

// Store 聚合全部 repo（依赖注入入口）。
type Store interface {
	Agents() AgentRepo
	Sessions() SessionRepo
	Messages() MessageRepo
	Groups() GroupRepo
	Tasks() TaskRepo
	Events() EventRepo
	Close() error
}
