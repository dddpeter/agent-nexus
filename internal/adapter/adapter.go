// Package adapter 定义系统唯一核心抽象 AgentAdapter（architecture.md §3.1）
// 及各实现共享的数据结构、ConfigSchema 声明式表单模型。
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrModeNotSupported agent 不支持所请求的发送模式（Send/Stream 与其 mode 不匹配）。
// 语义为请求与能力不匹配（HTTP 409），而非服务端内部错误。
var ErrModeNotSupported = errors.New("adapter: mode not supported")

// SessionID 是 agent 侧会话标识。
type SessionID string

// Capabilities 能力声明（注册时协商，technical-design §4.1）。
type Capabilities struct {
	CanGroupChat   bool `json:"can_group_chat" yaml:"can_group_chat"`
	CanStream      bool `json:"can_stream" yaml:"can_stream"`
	MaxConcurrency int  `json:"max_concurrency" yaml:"max_concurrency"` // >0；0 非法（F1.5）
	// TypicalLatency 序列化为人类可读时长（"1.5s"）。
	TypicalLatency Duration `json:"typical_latency" yaml:"typical_latency"`
	// InterruptLevel: 0=禁止插话 1=可申请 2=可直接插。
	InterruptLevel int `json:"interrupt_level" yaml:"interrupt_level"`
}

// DefaultCapabilities 返回保守默认值（宁严勿松）。
func DefaultCapabilities() Capabilities {
	return Capabilities{
		MaxConcurrency: 1,
		TypicalLatency: Duration(30 * time.Second),
		InterruptLevel: 1,
	}
}

// MergeDefaults 用 non-zero 覆盖值合并出最终能力（注册 spec 可部分覆盖）。
func (c Capabilities) MergeDefaults(d Capabilities) Capabilities {
	out := d
	if c.CanGroupChat {
		out.CanGroupChat = true
	}
	if c.CanStream {
		out.CanStream = true
	}
	if c.MaxConcurrency > 0 {
		out.MaxConcurrency = c.MaxConcurrency
	}
	if c.TypicalLatency > 0 {
		out.TypicalLatency = c.TypicalLatency
	}
	if c.InterruptLevel > 0 {
		out.InterruptLevel = c.InterruptLevel
	}
	return out
}

// Duration 是 JSON/YAML 序列化为时长字符串的 time.Duration（"1.5s"）。
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%q", d.D().String())), nil
}
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("adapter: invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// UnmarshalYAML 支持 config.yaml 中 "10s" 形式。
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("adapter: invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Attachment 附件。
type Attachment struct {
	Name     string `json:"name"`
	MIMEType string `json:"mime_type,omitempty"`
	Data     []byte `json:"data,omitempty"`
	URL      string `json:"url,omitempty"`
}

// Message 发往 agent 的消息；ContextDigest 携带平台侧注入的上下文摘要（F2/F4 联动）。
type Message struct {
	Content       string       `json:"content"`
	Attachments   []Attachment `json:"attachments,omitempty"`
	ContextDigest string       `json:"context_digest,omitempty"`
}

// Reply agent 回复；SessionRef 为 agent 侧会话标识（续话映射回填用，F2.3）。
type Reply struct {
	Content    string `json:"content"`
	SessionRef string `json:"session_ref,omitempty"`
	Done       bool   `json:"done"`
}

// StreamEventType 流式事件类型。
type StreamEventType string

const (
	StreamDelta StreamEventType = "delta"
	StreamFinal StreamEventType = "final"
	StreamError StreamEventType = "error"
)

// StreamEvent 统一流事件（delta / final / error）。
type StreamEvent struct {
	Type StreamEventType `json:"type"`
	Data string          `json:"data"`
	// ErrType 仅 Type=StreamError 时给出分类（timeout / agent_error / canceled）。
	ErrType string `json:"err_type,omitempty"`
}

// SessionOpts 建会话选项。
type SessionOpts struct {
	TopicID       string `json:"topic_id,omitempty"`
	ContextDigest string `json:"context_digest,omitempty"`
}

// AgentInfo agent 身份与能力声明。
type AgentInfo struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Adapter      string       `json:"adapter"`
	Capabilities Capabilities `json:"capabilities"`
}

// AgentAdapter 是系统唯一核心抽象：新接 agent = 一个 adapter 文件，平台零改动。
// 实现不假设 agent 在本机（一期本地进程驱动，二期 A2A HTTP 即远程）。
type AgentAdapter interface {
	Info() AgentInfo
	CreateSession(ctx context.Context, opts SessionOpts) (SessionID, error)
	Send(ctx context.Context, sid SessionID, msg Message) (Reply, error)
	Stream(ctx context.Context, sid SessionID, msg Message) (<-chan StreamEvent, func(), error)
	Close(ctx context.Context, sid SessionID) error
	// HealthCheck 探活：CLI 型执行探活命令，HTTP 型 GET health（F1.3 心跳器）。
	HealthCheck(ctx context.Context) error
}

// ErrSessionLost 表示 agent 侧会话标识失效（F2.4：自动重建 + 注入上下文摘要）。
var ErrSessionLost = errors.New("adapter: agent-side session lost")

// AdapterFactory 是 registry 聚合的适配器工厂（F1.3）：
// 注册即实例化 adapter，配置校验失败直接拒绝注册。
type AdapterFactory interface {
	// Name adapter 类型名（agents.adapter 列取值）。
	Name() string
	// ConfigSchema 声明式配置表单（注册向导动态表单数据源，FR1.1a）。
	ConfigSchema() Schema
	// New 实例化 adapter；配置非法返回 *FieldError（含字段名，U-F1-02）。
	New(cfg Config) (AgentAdapter, error)
}
