// Package private 实现私聊会话路由（F2）：创建/续话、同步与流式双模式、
// 上下文窗口与摘要压缩、会话丢失自动重建、超时占位、异步回执协议。
package private

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/internal/id"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

// 常见错误。
var (
	ErrSessionClosed    = errors.New("session: closed")
	ErrAgentUnavailable = errors.New("session: agent unavailable") // offline/disabled（API → 409）
	ErrTimeout          = errors.New("session: reply timeout")
	ErrNoAsyncRunner    = errors.New("session: async runner not configured")
)

// Mode 发送模式（FR2.2）。
type Mode string

const (
	ModeSync   Mode = "sync"
	ModeStream Mode = "stream"
	ModeAsync  Mode = "async"
)

// Registry 是路由所需的注册层能力（registry.Impl 满足；测试可替身）。
type Registry interface {
	Acquire(ctx context.Context, agentID string) (release func(), err error)
	Adapter(agentID string) (adapter.AgentAdapter, error)
}

// AsyncRunner 异步执行器（orchestr builtin 实现；session 不依赖 orchestr 包）。
// Submit 返回 task_id 供回执协议；job 完成后由调用方回填结果。
type AsyncRunner interface {
	Submit(ctx context.Context, job func(ctx context.Context)) (taskID string, err error)
}

// Options 路由策略（F2.3，全部可注入）。
type Options struct {
	SyncTimeout          time.Duration                // 同步默认 300s（可配）
	StreamIdleTimeout    time.Duration                // 流式 60s 无 delta 判死
	ContextRounds        int                          // 无会话机制 agent 保留最近 K 轮（默认 10）
	SummaryTriggerRounds int                          // 超 20 轮触发摘要压缩
	Summarizer           func(rounds []string) string // 摘要生成（默认截断+标记；可换 agent 生成）
	Logger               *slog.Logger
	Now                  func() time.Time
}

func (o *Options) normalize() {
	if o.SyncTimeout <= 0 {
		o.SyncTimeout = 300 * time.Second
	}
	if o.StreamIdleTimeout <= 0 {
		o.StreamIdleTimeout = 60 * time.Second
	}
	if o.ContextRounds <= 0 {
		o.ContextRounds = 10
	}
	if o.SummaryTriggerRounds <= 0 {
		o.SummaryTriggerRounds = 20
	}
	if o.Summarizer == nil {
		o.Summarizer = truncateSummarizer
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// SummaryMark 摘要标记（agent 摘要与截断降级共用）。
const SummaryMark = "[summary]"

// TruncateSummarizer 导出的截断降级实现（serve 装配层 agent 摘要失败时复用，F2.3）。
func TruncateSummarizer(rounds []string) string { return truncateSummarizer(rounds) }

// truncateSummarizer 默认摘要：截断降级（标记 + 最近内容），失败不阻塞（F2.3：失败则截断）。
func truncateSummarizer(rounds []string) string {
	const summaryMark = SummaryMark
	var sb strings.Builder
	sb.WriteString(summaryMark + " 前文摘要（截断模式）: ")
	total := 0
	for i := len(rounds) - 1; i >= 0; i-- {
		r := rounds[i]
		if total+len(r) > 2000 {
			break
		}
		sb.WriteString(r)
		sb.WriteString(" / ")
		total += len(r) + 3
	}
	return sb.String()
}

// Router 私聊路由器。
type Router struct {
	store repo.Store
	reg   Registry
	bus   bus.Bus
	opts  Options
	async AsyncRunner // 可选：mode=async（SetAsyncRunner 注入）

	mu sync.Mutex // 保护 appendMessage 的会话写入串行化（按 session 细化锁可后续优化）
}

// New 创建路由。
func New(store repo.Store, reg Registry, b bus.Bus, opts Options) *Router {
	opts.normalize()
	return &Router{store: store, reg: reg, bus: b, opts: opts}
}

// AsyncReceipt 异步回执协议（F2.3）：{type:"accepted", task_id, eta_hint}。
type AsyncReceipt struct {
	Type    string `json:"type"`
	TaskID  string `json:"task_id"`
	EtaHint string `json:"eta_hint"`
}

// SendResult 同步/异步发送结果。
type SendResult struct {
	// Reply agent 回复（超时为占位消息行，sender_type=system）。
	Reply    *repo.Message
	TimedOut bool
	Async    *AsyncReceipt // mode=async 时非空
}

// CreateSession 创建私聊会话（FR2.1）。
func (r *Router) CreateSession(ctx context.Context, agentID string) (repo.Session, error) {
	if _, err := r.reg.Adapter(agentID); err != nil {
		return repo.Session{}, fmt.Errorf("session: agent %s: %w", agentID, err)
	}
	ag := agentID
	now := r.opts.Now()
	sess := repo.Session{
		ID:        id.New(),
		AgentID:   &ag,
		Kind:      repo.KindPrivate,
		CreatedAt: now,
	}
	if err := r.store.Sessions().Create(ctx, sess); err != nil {
		return repo.Session{}, err
	}
	return sess, nil
}

// SendMessage 发消息主流程（F2.3）：读 session → Acquire → Send/Stream → 落库 → 事件。
func (r *Router) SendMessage(ctx context.Context, sessionID, content string, mode Mode) (SendResult, error) {
	sess, err := r.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return SendResult{}, err
	}
	if sess.Kind != repo.KindPrivate {
		return SendResult{}, fmt.Errorf("session %s: kind %s 不支持私聊发送", sessionID, sess.Kind)
	}
	if sess.ClosedAt != nil {
		return SendResult{}, ErrSessionClosed
	}

	switch mode {
	case ModeAsync:
		return r.sendAsync(ctx, sess, content)
	case ModeStream:
		return SendResult{}, fmt.Errorf("session: stream 模式请用 StreamMessage")
	case ModeSync:
		return r.sendSync(ctx, sess, content)
	default:
		return SendResult{}, fmt.Errorf("session: unknown mode %q", mode)
	}
}

// sendSync 同步模式：等 Reply（带超时，默认 300s）。
func (r *Router) sendSync(ctx context.Context, sess repo.Session, content string) (SendResult, error) {
	agentID := *sess.AgentID

	// 并发闸门 + offline 判定（F2.4：offline 409 不排队；agent 不存在 404）
	release, err := r.reg.Acquire(ctx, agentID)
	if err != nil {
		return SendResult{}, fmt.Errorf("%w: %w", ErrAgentUnavailable, err)
	}
	defer release()

	ad, err := r.reg.Adapter(agentID)
	if err != nil {
		return SendResult{}, err
	}

	// 上下文摘要基于「当前消息之前」的历史（U-F2-06/07 语义）
	digest := r.buildContextDigest(ctx, sess.ID)

	// 人工消息先落库（reply_to 目标）
	human := repo.Message{
		SessionID: sess.ID, SenderType: repo.SenderHuman, SenderID: "user",
		Content: content, CreatedAt: r.opts.Now(),
	}
	if err := r.appendMessage(ctx, &human); err != nil {
		return SendResult{}, err
	}
	r.publishMessage(sess.ID, agentID, &human, "")

	reply, err := r.sendWithSessionContinuity(ctx, ad, sess, digest, content)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTimeout) {
			return r.timeoutPlaceholder(ctx, sess, agentID, &human)
		}
		// agent 失败：记录 system 错误消息并返回
		r.appendSystem(ctx, sess.ID, agentID, "agent error: "+err.Error(), strPtr(fmt.Sprintf("%d", human.ID)))
		return SendResult{}, err
	}

	agentMsg := repo.Message{
		SessionID: sess.ID, SenderType: repo.SenderAgent, SenderID: agentID,
		Content: reply.Content, ReplyTo: strPtr(fmt.Sprintf("%d", human.ID)),
		CreatedAt: r.opts.Now(),
	}
	if err := r.appendMessage(ctx, &agentMsg); err != nil {
		return SendResult{}, err
	}
	r.publishMessage(sess.ID, agentID, &agentMsg, fmt.Sprintf("%d", human.ID))
	return SendResult{Reply: &agentMsg}, nil
}

// sendWithSessionContinuity 续话映射 + 会话丢失重建（F2.3 / F2.4）。
// digest 为发送前预构建的上下文摘要（无会话机制 agent 注入）。
func (r *Router) sendWithSessionContinuity(ctx context.Context, ad adapter.AgentAdapter, sess repo.Session, digest, content string) (adapter.Reply, error) {
	sctx, cancel := context.WithTimeout(ctx, r.opts.SyncTimeout)
	defer cancel()

	msg := adapter.Message{Content: content}
	// 无会话机制 agent：拼上下文摘要（external_ref 恒空 ⇒ 每次注入）
	if sess.ExternalRef == nil || *sess.ExternalRef == "" {
		msg.ContextDigest = digest
	}

	sid := r.sidOf(sess, ad)
	reply, err := ad.Send(sctx, sid, msg)
	if err == nil {
		r.backfillExternalRef(ctx, sess, reply.SessionRef)
		return reply, nil
	}
	if errors.Is(err, adapter.ErrSessionLost) {
		// 自动重建会话 + 注入上下文摘要 + system 提示（F2.4）
		newSid, cerr := ad.CreateSession(ctx, adapter.SessionOpts{ContextDigest: msg.ContextDigest})
		if cerr != nil {
			return adapter.Reply{}, fmt.Errorf("session rebuild failed: %w", cerr)
		}
		reply, err = ad.Send(sctx, newSid, msg)
		if err != nil {
			return adapter.Reply{}, err
		}
		r.backfillExternalRef(ctx, sess, reply.SessionRef)
		r.appendSystem(ctx, sess.ID, *sess.AgentID, "agent 侧会话已失效，已自动重建并注入上下文摘要", nil)
		return reply, nil
	}
	return adapter.Reply{}, err
}

// sidOf 计算本次发送使用的 agent 侧会话标识；无 external_ref 时先建会话。
func (r *Router) sidOf(sess repo.Session, ad adapter.AgentAdapter) adapter.SessionID {
	if sess.ExternalRef != nil && *sess.ExternalRef != "" {
		return adapter.SessionID(*sess.ExternalRef)
	}
	// 尚未建立映射：用平台 session id 让 agent 侧登记（Reply.SessionRef 回填）
	return adapter.SessionID(sess.ID)
}

// backfillExternalRef 首次回复从 Reply.SessionRef 回填续话映射（F2.3）。
func (r *Router) backfillExternalRef(ctx context.Context, sess repo.Session, ref string) {
	if ref == "" {
		return // 无会话机制 agent：保持空 ⇒ 后续每次注入上下文
	}
	if sess.ExternalRef != nil && *sess.ExternalRef == ref {
		return
	}
	fresh, err := r.store.Sessions().Get(ctx, sess.ID)
	if err != nil {
		return
	}
	fresh.ExternalRef = &ref
	if err := r.store.Sessions().Update(ctx, fresh); err != nil {
		r.opts.Logger.Error("backfill external_ref failed", "session", sess.ID, "err", err)
	}
	sess.ExternalRef = &ref
}

// buildContextDigest 上下文窗口策略（F2.3）：最近 K 轮（默认 10）+ 超阈值摘要压缩（默认 20 轮触发）。
func (r *Router) buildContextDigest(ctx context.Context, sessionID string) string {
	msgs, err := r.store.Messages().ListBySession(ctx, sessionID)
	if err != nil || len(msgs) == 0 {
		return ""
	}
	var rounds []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			rounds = append(rounds, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	for _, m := range msgs {
		switch m.SenderType {
		case repo.SenderHuman:
			if len(cur) > 0 {
				flush()
			}
			cur = append(cur, "user: "+m.Content)
		case repo.SenderAgent:
			cur = append(cur, "agent: "+m.Content)
			flush()
		}
	}
	flush()
	if len(rounds) == 0 {
		return ""
	}
	var sb strings.Builder
	if len(rounds) >= r.opts.SummaryTriggerRounds {
		// 摘要触发（U-F2-07）：历史达到阈值（默认 20 轮）即压缩为标记摘要 + 最近 K 轮
		sb.WriteString(r.opts.Summarizer(rounds[:len(rounds)-r.opts.ContextRounds]))
		sb.WriteString("\n")
		rounds = rounds[len(rounds)-r.opts.ContextRounds:]
	}
	if len(rounds) > r.opts.ContextRounds {
		rounds = rounds[len(rounds)-r.opts.ContextRounds:]
	}
	for _, rd := range rounds {
		sb.WriteString(rd)
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// timeoutPlaceholder 超时占位（F2.4）：占位消息 + 事件；session 保留可重发。
func (r *Router) timeoutPlaceholder(ctx context.Context, sess repo.Session, agentID string, human *repo.Message) (SendResult, error) {
	placeholder := repo.Message{
		SessionID: sess.ID, SenderType: repo.SenderSystem, SenderID: "system",
		Content: fmt.Sprintf("[timeout] agent 未在 %s 内回复，会话保留可重发", r.opts.SyncTimeout),
		ReplyTo: strPtr(fmt.Sprintf("%d", human.ID)), CreatedAt: r.opts.Now(),
	}
	if err := r.appendMessage(ctx, &placeholder); err != nil {
		return SendResult{TimedOut: true}, fmt.Errorf("%w: persist placeholder: %v", ErrTimeout, err)
	}
	r.publishMessage(sess.ID, agentID, &placeholder, fmt.Sprintf("%d", human.ID))
	return SendResult{Reply: &placeholder, TimedOut: true}, ErrTimeout
}

// sendAsync 异步回执协议（F2.3 / U-F2-08）：立即 accepted 回执，完成后 system 回填。
func (r *Router) sendAsync(ctx context.Context, sess repo.Session, content string) (SendResult, error) {
	agentID := *sess.AgentID
	if r.async == nil {
		return SendResult{}, ErrNoAsyncRunner
	}
	human := repo.Message{
		SessionID: sess.ID, SenderType: repo.SenderHuman, SenderID: "user",
		Content: content, CreatedAt: r.opts.Now(),
	}
	if err := r.appendMessage(ctx, &human); err != nil {
		return SendResult{}, err
	}
	r.publishMessage(sess.ID, agentID, &human, "")

	etaHint := "unknown"
	if ad, err := r.reg.Adapter(agentID); err == nil {
		etaHint = ad.Info().Capabilities.TypicalLatency.D().String()
	}

	taskID, err := r.async.Submit(ctx, func(jobCtx context.Context) {
		r.runAsyncJob(jobCtx, sess, agentID, content, &human)
	})
	if err != nil {
		return SendResult{}, fmt.Errorf("session: submit async job: %w", err)
	}
	return SendResult{Async: &AsyncReceipt{Type: "accepted", TaskID: taskID, EtaHint: etaHint}}, nil
}

// runAsyncJob 长任务执行体：完成后回填 sender_type=system、reply_to=原消息 的消息（F2.3）。
func (r *Router) runAsyncJob(ctx context.Context, sess repo.Session, agentID, content string, human *repo.Message) {
	release, err := r.reg.Acquire(ctx, agentID)
	if err != nil {
		r.appendSystem(ctx, sess.ID, agentID,
			fmt.Sprintf("[async failed] agent unavailable: %v", err), strPtr(fmt.Sprintf("%d", human.ID)))
		return
	}
	defer release()
	ad, err := r.reg.Adapter(agentID)
	if err != nil {
		r.appendSystem(ctx, sess.ID, agentID, "[async failed] "+err.Error(), strPtr(fmt.Sprintf("%d", human.ID)))
		return
	}
	// 异步任务不受交互超时约束：用较长兜底（同步超时 × 6）
	actx, cancel := context.WithTimeout(ctx, r.opts.SyncTimeout*6)
	defer cancel()
	msg := adapter.Message{Content: content}
	if sess.ExternalRef == nil || *sess.ExternalRef == "" {
		msg.ContextDigest = r.buildContextDigest(ctx, sess.ID)
	}
	reply, err := ad.Send(actx, r.sidOf(sess, ad), msg)
	if err != nil {
		r.appendSystem(ctx, sess.ID, agentID, "[async error] "+err.Error(), strPtr(fmt.Sprintf("%d", human.ID)))
		return
	}
	r.backfillExternalRef(ctx, sess, reply.SessionRef)
	backfill := repo.Message{
		SessionID: sess.ID, SenderType: repo.SenderSystem, SenderID: agentID,
		Content: reply.Content, ReplyTo: strPtr(fmt.Sprintf("%d", human.ID)),
		CreatedAt: r.opts.Now(),
	}
	if err := r.appendMessage(ctx, &backfill); err != nil {
		r.opts.Logger.Error("async backfill persist failed", "session", sess.ID, "err", err)
		return
	}
	r.publishMessage(sess.ID, agentID, &backfill, fmt.Sprintf("%d", human.ID))
}

// SetAsyncRunner 注入异步执行器（api 装配层调用；orchestr builtin 提供实现）。
func (r *Router) SetAsyncRunner(runner AsyncRunner) { r.async = runner }

// CloseSession 关闭会话（FR2.5；回填时 session 已关闭 → 结果只落库、事件标记 orphaned，由回填方处理）。
func (r *Router) CloseSession(ctx context.Context, sessionID string) error {
	sess, err := r.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return err
	}
	now := r.opts.Now()
	sess.ClosedAt = &now
	return r.store.Sessions().Update(ctx, sess)
}

// ---------- 内部工具 ----------

func (r *Router) appendMessage(ctx context.Context, m *repo.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.store.Messages().Append(ctx, m)
}

func (r *Router) appendSystem(ctx context.Context, sessionID, agentID, content string, replyTo *string) {
	m := repo.Message{
		SessionID: sessionID, SenderType: repo.SenderSystem, SenderID: "system",
		Content: content, ReplyTo: replyTo, CreatedAt: r.opts.Now(),
	}
	if err := r.appendMessage(ctx, &m); err != nil {
		r.opts.Logger.Error("persist system message failed", "session", sessionID, "err", err)
		return
	}
	r.publishMessage(sessionID, agentID, &m, deref(replyTo))
}

func (r *Router) publishMessage(sessionID, agentID string, m *repo.Message, replyTo string) {
	payload := fmt.Sprintf(`{"session_id":%q,"agent_id":%q,"sender_type":%q,"sender_id":%q,"content":%q,"message_id":%d,"reply_to":%q}`,
		sessionID, agentID, m.SenderType, m.SenderID, truncate(m.Content, 4000), m.ID, replyTo)
	typ := bus.TypeMsgSystem
	switch m.SenderType {
	case repo.SenderHuman:
		typ = bus.TypeMsgHuman
	case repo.SenderAgent:
		typ = bus.TypeMsgAgent
	}
	_ = r.bus.Publish(context.Background(), bus.Event{Type: typ, Payload: []byte(payload)})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func strPtr(s string) *string { return &s }

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// StreamMessage 流式模式（FR2.2）：伪流式/真流式统一为 StreamEvent 通道；
// 60s（可配）无 delta 判死发 error 事件（U-F2-04）；final 到达后落库 + 事件。
func (r *Router) StreamMessage(ctx context.Context, sessionID, content string) (<-chan adapter.StreamEvent, func(), error) {
	sess, err := r.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if sess.Kind != repo.KindPrivate {
		return nil, nil, fmt.Errorf("session %s: kind %s 不支持私聊流式", sessionID, sess.Kind)
	}
	if sess.ClosedAt != nil {
		return nil, nil, ErrSessionClosed
	}
	agentID := *sess.AgentID
	release, err := r.reg.Acquire(ctx, agentID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrAgentUnavailable, err)
	}
	ad, err := r.reg.Adapter(agentID)
	if err != nil {
		release()
		return nil, nil, err
	}

	digest := r.buildContextDigest(ctx, sess.ID)
	human := repo.Message{
		SessionID: sess.ID, SenderType: repo.SenderHuman, SenderID: "user",
		Content: content, CreatedAt: r.opts.Now(),
	}
	if err := r.appendMessage(ctx, &human); err != nil {
		release()
		return nil, nil, err
	}
	r.publishMessage(sess.ID, agentID, &human, "")

	msg := adapter.Message{Content: content}
	if sess.ExternalRef == nil || *sess.ExternalRef == "" {
		msg.ContextDigest = digest
	}

	src, cancelSrc, err := ad.Stream(ctx, r.sidOf(sess, ad), msg)
	if err != nil {
		release()
		return nil, nil, err
	}

	out := make(chan adapter.StreamEvent, 64)
	var closed onceClose
	closed.fn = func() {
		cancelSrc()
		release()
	}
	go func() {
		defer close(out)
		defer closed.call()
		idle := time.NewTimer(r.opts.StreamIdleTimeout)
		defer idle.Stop()
		var assembled strings.Builder
		for {
			select {
			case ev, ok := <-src:
				if !ok {
					return
				}
				if !idle.Reset(r.opts.StreamIdleTimeout) {
					// timer 已触发过的罕见竞态：重建
					idle = time.NewTimer(r.opts.StreamIdleTimeout)
				}
				switch ev.Type {
				case adapter.StreamDelta:
					assembled.WriteString(ev.Data)
					send(out, ev)
				case adapter.StreamFinal:
					if assembled.Len() == 0 {
						assembled.WriteString(ev.Data) // 无 delta 的 final 自带全量
					}
					send(out, ev)
					// 落库用独立 ctx：API 层转发完 final 帧即 return 并取消请求 ctx，
					// 此时本 goroutine 才开始落库——曾因复用请求 ctx 丢 agent 回复（集成测试实机抓到）
					pctx, pcancel := context.WithTimeout(context.Background(), 15*time.Second)
					r.finalizeStream(pctx, sess, agentID, &human, assembled.String())
					pcancel()
					return
				case adapter.StreamError:
					send(out, ev)
					return
				}

			case <-idle.C:
				// 60s 无 delta 判死（U-F2-04）
				send(out, adapter.StreamEvent{Type: adapter.StreamError,
					Data: fmt.Sprintf("stream idle for %s, abort", r.opts.StreamIdleTimeout), ErrType: "timeout"})
				sctx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
				r.appendSystem(sctx, sess.ID, agentID,
					fmt.Sprintf("[stream timeout] %s 无增量输出", r.opts.StreamIdleTimeout),
					strPtr(fmt.Sprintf("%d", human.ID)))
				scancel()
				return
			case <-ctx.Done():
				send(out, adapter.StreamEvent{Type: adapter.StreamError, Data: ctx.Err().Error(), ErrType: "canceled"})
				return
			}
		}
	}()
	return out, closed.call, nil
}

// finalizeStream final 后落库 agent 回复（U-F2-01 同构）。
func (r *Router) finalizeStream(ctx context.Context, sess repo.Session, agentID string, human *repo.Message, content string) {
	agentMsg := repo.Message{
		SessionID: sess.ID, SenderType: repo.SenderAgent, SenderID: agentID,
		Content: content, ReplyTo: strPtr(fmt.Sprintf("%d", human.ID)),
		CreatedAt: r.opts.Now(),
	}
	if err := r.appendMessage(ctx, &agentMsg); err != nil {
		r.opts.Logger.Error("persist stream final failed", "session", sess.ID, "err", err)
		return
	}
	r.publishMessage(sess.ID, agentID, &agentMsg, fmt.Sprintf("%d", human.ID))
}

func send(ch chan<- adapter.StreamEvent, ev adapter.StreamEvent) {
	select {
	case ch <- ev:
	default: // 下游（SSE per-connection）消费慢：丢弃 delta 保实时（与总线背压策略一致）
	}
}

// onceClose 一次性取消包装。
type onceClose struct {
	once sync.Once
	fn   func()
}

func (o *onceClose) call() { o.once.Do(func() { o.fn() }) }
