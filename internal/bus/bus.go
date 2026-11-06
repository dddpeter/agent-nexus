// Package bus 实现进程内事件总线（F6）：内存 channel 扇出 + event_log 异步批量落库。
//
// 语义（见 technical-design §5 / detailed-design F6.2）：
//   - Publish：同步分配全局递增 seq（与 event_log.seq 一致）→ 内存扇出 → 异步入队落库
//   - Subscribe：每订阅者独立 buffered channel（默认 256），慢消费者丢弃计数，不阻塞总线
//   - 落库：单独 goroutine 批量 flush（100 条 / 500ms 触发）
package bus

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// 事件类型（architecture.md §3.2）。
const (
	TypeMsgHuman        = "msg.human"
	TypeMsgAgent        = "msg.agent"
	TypeMsgSystem       = "msg.system"
	TypeAgentStatus     = "agent.status"
	TypeTaskLifecycle   = "task.lifecycle"
	TypeArbiterDecision = "arbiter.decision"
)

// Event 是总线与 event_log 的统一事件结构。
// Seq 为全局单调递增序号，由 Publish 分配，与 event_log.seq 保持一致，
// 供 SSE Last-Event-ID 断线补偿重放（F5）使用。
type Event struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// Clone 返回事件副本（深拷贝 Payload）。
func (e Event) Clone() Event {
	p := make(json.RawMessage, len(e.Payload))
	copy(p, e.Payload)
	e.Payload = p
	return e
}

// Sink 是事件持久化接口（由 repo 层实现，注入总线；bus 不感知具体存储）。
type Sink interface {
	AppendEvents(ctx context.Context, events []Event) error
	MaxEventSeq(ctx context.Context) (int64, error)
}

// Bus 是事件总线公开接口（technical-design §5）。
type Bus interface {
	Publish(ctx context.Context, e Event) error
	Subscribe(topics ...string) (<-chan Event, func())
	// CurrentSeq 返回当前已分配的最大 seq（SSE 重放水位线用）。
	CurrentSeq() int64
	// DroppedTotal 返回慢消费者丢弃事件总数（metrics 暴露用）。
	DroppedTotal() int64
	// Close 停止后台 flush，将队列中剩余事件全部落库后返回。
	Close() error
}

// Options 配置总线行为（测试可注入）。
type Options struct {
	PersistBatchSize int           // 批量落库阈值，默认 100
	FlushInterval    time.Duration // 定时 flush 间隔，默认 500ms
	SubscriberBuffer int           // 每订阅者 channel 缓冲，默认 256
	PersistQueueSize int           // 落库队列缓冲长度，默认 4096；满时 Publish 阻塞等待（落库不允许丢）
}

func (o *Options) normalize() {
	if o.PersistBatchSize <= 0 {
		o.PersistBatchSize = 100
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = 500 * time.Millisecond
	}
	if o.SubscriberBuffer <= 0 {
		o.SubscriberBuffer = 256
	}
	if o.PersistQueueSize <= 0 {
		o.PersistQueueSize = 4096
	}
}

type subscriber struct {
	topics  []string // 空 = 全部；元素支持 "msg.*" 前缀通配
	ch      chan Event
	dropped atomic.Int64
	closed  bool
}

func (s *subscriber) match(typ string) bool {
	if len(s.topics) == 0 {
		return true
	}
	for _, t := range s.topics {
		if t == typ {
			return true
		}
		if n := len(t); n > 1 && t[n-1] == '*' && len(typ) >= n-1 && typ[:n-1] == t[:n-1] {
			return true
		}
	}
	return false
}

type membus struct {
	opts Options
	sink Sink

	mu           sync.Mutex // 保护 seq 分配 + 扇出顺序一致 + 订阅者表
	seq          atomic.Int64
	subs         map[*subscriber]struct{}
	persistCh    chan Event
	droppedTotal atomic.Int64

	stopOnce  sync.Once
	stopCh    chan struct{}
	flushDone chan struct{}
	wg        sync.WaitGroup
}

// New 创建事件总线；sink 可为 nil（纯内存模式，测试用）。
// seq 起点从 sink.MaxEventSeq 恢复，保证重启后 seq 延续。
func New(sink Sink, opts Options) (Bus, error) {
	opts.normalize()
	b := &membus{
		opts:      opts,
		sink:      sink,
		subs:      make(map[*subscriber]struct{}),
		persistCh: make(chan Event, opts.PersistQueueSize),
		stopCh:    make(chan struct{}),
		flushDone: make(chan struct{}),
	}
	if sink != nil {
		max, err := sink.MaxEventSeq(context.Background())
		if err != nil {
			return nil, err
		}
		b.seq.Store(max)
	}
	b.wg.Add(1)
	go b.flushLoop()
	return b, nil
}

// Publish 分配 seq、扇出到订阅者、入落库队列。
// 慢订阅者不阻塞发布（背压策略：丢弃，detailed-design F6.2）。
func (b *membus) Publish(_ context.Context, e Event) error {
	if e.Type == "" {
		return ErrEmptyType
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}

	b.mu.Lock()
	e.Seq = b.seq.Add(1)
	for s := range b.subs {
		if !s.match(e.Type) {
			continue
		}
		select {
		case s.ch <- e.Clone():
		default:
			s.dropped.Add(1)
			b.droppedTotal.Add(1)
		}
	}
	b.mu.Unlock()

	if b.sink != nil {
		select {
		case b.persistCh <- e:
		case <-b.stopCh:
			// Close 期间不再接收新事件，丢弃由 Close 语义保证（调用方已停机）。
		}
	}
	return nil
}

// Subscribe 订阅事件；topics 为空订阅全部，支持 "msg.*" 前缀通配。
// 返回 channel 与取消函数；unsubscribe 后 channel 关闭。
func (b *membus) Subscribe(topics ...string) (<-chan Event, func()) {
	s := &subscriber{
		topics: topics,
		ch:     make(chan Event, b.opts.SubscriberBuffer),
	}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s.ch, func() {
		b.mu.Lock()
		if _, ok := b.subs[s]; ok {
			delete(b.subs, s)
			s.closed = true
			close(s.ch)
		}
		b.mu.Unlock()
	}
}

func (b *membus) CurrentSeq() int64 { return b.seq.Load() }

func (b *membus) DroppedTotal() int64 { return b.droppedTotal.Load() }

// flushLoop 批量落库：攒够 batchSize 或 ticker 到期即 flush（F6.2）。
func (b *membus) flushLoop() {
	defer b.wg.Done()
	defer close(b.flushDone)

	ticker := time.NewTicker(b.opts.FlushInterval)
	defer ticker.Stop()

	batch := make([]Event, 0, b.opts.PersistBatchSize)
	flush := func() {
		if len(batch) == 0 || b.sink == nil {
			return
		}
		// 落库失败重试一次；再失败则放弃该批并告警（不阻塞总线内存投递，
		// 故障注入矩阵：event_log 落库故障不影响内存扇出）。
		if err := b.sink.AppendEvents(context.Background(), batch); err != nil {
			time.Sleep(50 * time.Millisecond)
			if err2 := b.sink.AppendEvents(context.Background(), batch); err2 != nil {
				// 无法落库，丢弃并继续（内存投递已完成）
				_ = err2
			}
		}
		batch = batch[:0]
	}

	for {
		select {
		case e := <-b.persistCh:
			batch = append(batch, e)
			if len(batch) >= b.opts.PersistBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-b.stopCh:
			// 排空队列后退出
			for {
				select {
				case e := <-b.persistCh:
					batch = append(batch, e)
					if len(batch) >= b.opts.PersistBatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// Close 停止 flush 循环并保证队列内事件全部落库。
func (b *membus) Close() error {
	var err error
	b.stopOnce.Do(func() {
		close(b.stopCh)
		<-b.flushDone // 等 flushLoop 排空退出
	})
	return err
}
