// Package echo 提供内置测试用假 agent "echod"（M0 验收：注册收发回显）。
//
// 进程内形态（本文件）：可脚本化故障注入（echo/fixed/delay/hang/stream/down），
// 供 registry/session 单测使用；CLI 形态见 cmd/echod（供 picoclaw 范式与 F7 进程治理测试），
// HTTP 形态见 echodhttp（供 hermes 范式测试）。
package echo

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/id"
)

// Mode 行为模式（test-plan §1 fake agent：固定回复/回显/延迟/中途退出/伪流式等）。
const (
	ModeEcho   = "echo"   // 回显消息内容（默认）
	ModeFixed  = "fixed"  // 固定回复 cfg.reply
	ModeDelay  = "delay"  // 延迟 delay_ms 后回显（超时用例）
	ModeHang   = "hang"   // 永不回复（挂起；尊重 ctx 取消）
	ModeStream = "stream" // 伪流式：分片 delta + final
	ModeDown   = "down"   // 探活失败（心跳用例）
)

// Recorded 单次收到的消息记录（U-F2-05 续话断言用）。
type Recorded struct {
	SessionID  adapter.SessionID
	Content    string
	ExternalID string // 无状态模式下平台传入的 external_ref
	Digest     string // 收到的上下文摘要（U-F2-06/07 断言用）
	At         time.Time
}

// Factory 是 echod 的 AdapterFactory（注册到 registry）。
type Factory struct{}

// NewFactory 构造 echod 工厂。
func NewFactory() *Factory { return &Factory{} }

func (f *Factory) Name() string { return "echod" }

func (f *Factory) ConfigSchema() adapter.Schema {
	return adapter.Schema{Fields: []adapter.SchemaField{
		{Name: "mode", Label: "行为模式", Type: adapter.FieldString, Required: true, Default: ModeEcho,
			Options: []string{ModeEcho, ModeFixed, ModeDelay, ModeHang, ModeStream, ModeDown},
			Help:    "测试假 agent 行为脚本：echo=回显 fixed=固定回复 delay=延迟 hang=挂起 stream=伪流式 down=探活失败"},
		{Name: "reply", Label: "固定回复内容", Type: adapter.FieldString, Help: "mode=fixed 时的回复文本"},
		{Name: "delay_ms", Label: "延迟毫秒数", Type: adapter.FieldInt, Default: 0, Help: "mode=delay/stream 时的延迟"},
		{Name: "stream_chunks", Label: "流式分片数", Type: adapter.FieldInt, Default: 3, Help: "mode=stream 时 delta 分片数量"},
		{Name: "stream_interval_ms", Label: "分片间隔毫秒", Type: adapter.FieldInt, Default: 0,
			Help: "mode=stream 时相邻 delta 的间隔（0=连发）；>0 模拟真实打字机节奏"},
		{Name: "api_key", Label: "API Key", Type: adapter.FieldString, Secret: true, Help: "密钥字段（脱敏回显测试用，功能上未使用）"},
		{Name: "stateless", Label: "无会话机制", Type: adapter.FieldBool, Default: false,
			Help: "模拟无会话机制 agent：忽略 session id，平台须注入上下文摘要"},
		{Name: "session_lost_after", Label: "会话失效轮数", Type: adapter.FieldInt, Default: 0,
			Help: ">0 时第 N 轮 Send 返回 ErrSessionLost（F2.4 自动重建用例）"},
	}}
}

// New 实例化 echod adapter。
func (f *Factory) New(cfg adapter.Config) (adapter.AgentAdapter, error) {
	normalized, err := adapter.CoerceConfig(f.ConfigSchema(), cfg)
	if err != nil {
		return nil, err
	}
	return &Adapter{
		id:        id.New(),
		cfg:       normalized,
		sessions:  make(map[adapter.SessionID]bool),
		recvCount: make(map[adapter.SessionID]int),
	}, nil
}

// Adapter 进程内 echod 实现。
type Adapter struct {
	id  string
	cfg adapter.Config
	mu  sync.Mutex
	// sessions: platform 侧已建立的 agent 会话；recvCount: 每 session 已收消息数
	sessions  map[adapter.SessionID]bool
	recvCount map[adapter.SessionID]int
	records   []Recorded
}

var _ adapter.AgentAdapter = (*Adapter)(nil)

func (a *Adapter) Info() adapter.AgentInfo {
	caps := adapter.DefaultCapabilities()
	caps.CanGroupChat = true
	caps.TypicalLatency = adapter.Duration(50 * time.Millisecond)
	if a.mode() == ModeStream {
		caps.CanStream = true
		caps.DisableSync = true // 伪流式模式不支持同步 Send（U-F2-03）
	}
	return adapter.AgentInfo{ID: a.id, Name: "echod", Adapter: "echod", Capabilities: caps}
}

func (a *Adapter) mode() string { return adapter.GetStr(a.cfg, "mode", ModeEcho) }
func (a *Adapter) delay() time.Duration {
	return time.Duration(adapter.GetInt(a.cfg, "delay_ms", 0)) * time.Millisecond
}

// HealthCheck 探活：mode=down 返回错误，其余立即通过。
func (a *Adapter) HealthCheck(_ context.Context) error {
	if a.mode() == ModeDown {
		return fmt.Errorf("echod: probe failed (mode=down)")
	}
	return nil
}

func (a *Adapter) CreateSession(_ context.Context, _ adapter.SessionOpts) (adapter.SessionID, error) {
	sid := adapter.SessionID("echod-sess-" + id.New())
	a.mu.Lock()
	a.sessions[sid] = true
	a.mu.Unlock()
	return sid, nil
}

// sleepRespectCtx 延迟但尊重取消。
func sleepRespectCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (a *Adapter) Send(ctx context.Context, sid adapter.SessionID, msg adapter.Message) (adapter.Reply, error) {
	if err := a.HealthCheck(ctx); err != nil {
		return adapter.Reply{}, err
	}
	// 会话失效注入（F2.4）
	if n := adapter.GetInt(a.cfg, "session_lost_after", 0); n > 0 {
		a.mu.Lock()
		count := a.recvCount[sid]
		a.mu.Unlock()
		if count >= n {
			return adapter.Reply{}, fmt.Errorf("%w: echod session %s expired after %d turns", adapter.ErrSessionLost, sid, n)
		}
	}
	a.record(sid, msg)

	switch a.mode() {
	case ModeEcho, ModeFixed, ModeDelay:
		if err := sleepRespectCtx(ctx, a.delay()); err != nil {
			return adapter.Reply{}, err
		}
		content := msg.Content
		if a.mode() == ModeFixed {
			content = adapter.GetStr(a.cfg, "reply", "[fixed]")
		}
		ref := a.externalRefFor(sid)
		a.mu.Lock()
		a.recvCount[sid]++
		a.mu.Unlock()
		return adapter.Reply{Content: content, SessionRef: string(ref), Done: true}, nil
	case ModeHang:
		<-ctx.Done()
		return adapter.Reply{}, ctx.Err()
	default:
		return adapter.Reply{}, fmt.Errorf("echod: mode %q does not support Send: %w", a.mode(), adapter.ErrModeNotSupported)
	}
}

// Stream 伪流式：延迟后按分片输出 delta，最后 final（U-F2-03）。
func (a *Adapter) Stream(ctx context.Context, sid adapter.SessionID, msg adapter.Message) (<-chan adapter.StreamEvent, func(), error) {
	if a.mode() != ModeStream {
		return nil, nil, fmt.Errorf("echod: mode %q does not support Stream: %w", a.mode(), adapter.ErrModeNotSupported)
	}
	a.record(sid, msg)
	ch := make(chan adapter.StreamEvent, 16)
	stop := make(chan struct{})
	var once sync.Once
	cancel := func() { once.Do(func() { close(stop) }) }
	go func() {
		defer close(ch)
		if err := sleepRespectCtx(ctx, a.delay()); err != nil {
			select {
			case ch <- adapter.StreamEvent{Type: adapter.StreamError, Data: err.Error(), ErrType: "canceled"}:
			case <-stop:
			}
			return
		}
		content := msg.Content
		if r := adapter.GetStr(a.cfg, "reply", ""); r != "" {
			content = r
		}
		chunks := adapter.GetInt(a.cfg, "stream_chunks", 3)
		if chunks < 2 {
			chunks = 2 // 至少 2 delta（U-F2-03 断言 ≥2 delta + 1 final）
		}
		runes := []rune(content)
		per := (len(runes) + chunks - 1) / chunks
		if per == 0 {
			per = 1
		}
		for i := 0; i < len(runes); i += per {
			select {
			case <-stop:
				return
			default:
			}
			if iv := adapter.GetInt(a.cfg, "stream_interval_ms", 0); iv > 0 {
				if err := sleepRespectCtx(ctx, time.Duration(iv)*time.Millisecond); err != nil {
					return
				}
			}
			end := i + per
			if end > len(runes) {
				end = len(runes)
			}
			select {
			case ch <- adapter.StreamEvent{Type: adapter.StreamDelta, Data: string(runes[i:end])}:
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
		a.mu.Lock()
		a.recvCount[sid]++
		a.mu.Unlock()
		select {
		case ch <- adapter.StreamEvent{Type: adapter.StreamFinal, Data: content}:
		case <-stop:
		case <-ctx.Done():
		}
	}()
	return ch, cancel, nil
}

func (a *Adapter) Close(_ context.Context, sid adapter.SessionID) error {
	a.mu.Lock()
	delete(a.sessions, sid)
	delete(a.recvCount, sid)
	a.mu.Unlock()
	return nil
}

// externalRefFor 无状态模式返回空（平台侧拼上下文），有状态返回 sid。
func (a *Adapter) externalRefFor(sid adapter.SessionID) adapter.SessionID {
	if adapter.GetBool(a.cfg, "stateless", false) {
		return ""
	}
	return sid
}

func (a *Adapter) record(sid adapter.SessionID, msg adapter.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, Recorded{
		SessionID: sid, Content: msg.Content, Digest: msg.ContextDigest, At: time.Now(),
	})
}

// Recorded 返回收到的全部消息记录（测试断言用）。
func (a *Adapter) Recorded() []Recorded {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Recorded(nil), a.records...)
}

// LastDigest 返回最近一次收到的上下文摘要。
func (a *Adapter) LastDigest() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.records) - 1; i >= 0; i-- {
		if a.records[i].Digest != "" {
			return a.records[i].Digest
		}
	}
	return ""
}

// HasSession 断言 agent 侧是否已知该会话（续话断言用）。
func (a *Adapter) HasSession(sid adapter.SessionID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[sid]
}

// ReplyOf 便捷构造（echod CLI/HTTP 形态共用）。
func ReplyOf(content, sessionRef string) string {
	return fmt.Sprintf(`{"reply":%q,"session":%q}`, strings.TrimSpace(content), sessionRef)
}
