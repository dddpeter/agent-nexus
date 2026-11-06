package bus_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSink 内存事件落库实现（repo 集成测试见 internal/repo 包）。
type fakeSink struct {
	mu     sync.Mutex
	events []bus.Event
	maxSeq atomic.Int64
}

func (f *fakeSink) AppendEvents(_ context.Context, events []bus.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, events...)
	for _, e := range events {
		if e.Seq > f.maxSeq.Load() {
			f.maxSeq.Store(e.Seq)
		}
	}
	return nil
}

func (f *fakeSink) MaxEventSeq(_ context.Context) (int64, error) {
	return f.maxSeq.Load(), nil
}

func (f *fakeSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

func publishN(t *testing.T, b bus.Bus, n int, typ string) {
	t.Helper()
	for i := 0; i < n; i++ {
		require.NoError(t, b.Publish(context.Background(), bus.Event{Type: typ, Payload: []byte(`{}`)}))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

// U-F6-01 发布 1000 事件 → event_log 全量落库（最终一致，500ms 内）。
func TestBusPersistAllEvents(t *testing.T) {
	sink := &fakeSink{}
	// 使用默认批量参数（100 条 / 500ms）验证生产语义。
	b, err := bus.New(sink, bus.Options{})
	require.NoError(t, err)

	start := time.Now()
	publishN(t, b, 1000, bus.TypeMsgAgent)
	publishDone := time.Now()

	deadline := time.Now().Add(3 * time.Second)
	for sink.count() < 1000 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	persistDone := time.Now()

	require.Equal(t, 1000, sink.count(), "全部事件最终落库")
	// 批量阈值触发 + 500ms ticker 兜底：最后一批应在 500ms 定时器内 flush。
	assert.Less(t, persistDone.Sub(publishDone), 700*time.Millisecond,
		"落库应在最后一个事件发布后 500ms（ticker 周期）内完成")
	assert.Less(t, publishDone.Sub(start), 2*time.Second, "发布不应显著阻塞")

	// seq 连续且从 1 开始
	seen := make(map[int64]bool, 1000)
	for _, e := range sink.events {
		require.False(t, seen[e.Seq], "seq 不重复")
		seen[e.Seq] = true
	}
	for i := int64(1); i <= 1000; i++ {
		require.True(t, seen[i], "seq %d 缺失", i)
	}
	require.NoError(t, b.Close())
}

// U-F6-02 慢订阅者（channel 满）→ 发布不阻塞，丢弃计数 +1。
func TestBusSlowSubscriberDropped(t *testing.T) {
	sink := &fakeSink{}
	b, err := bus.New(sink, bus.Options{SubscriberBuffer: 1})
	require.NoError(t, err)
	defer b.Close()

	ch, unsub := b.Subscribe()
	defer unsub()

	start := time.Now()
	publishN(t, b, 10, bus.TypeMsgHuman)
	elapsed := time.Since(start)

	// 发布不阻塞（背压 = 丢弃）
	assert.Less(t, elapsed, 500*time.Millisecond, "慢订阅者不得阻塞发布")
	// 缓冲 1 条 + 丢弃 9 条
	assert.Equal(t, int64(9), b.DroppedTotal(), "丢弃计数应为 9")

	// 未被丢弃的事件仍可读到（第 1 条）
	select {
	case e := <-ch:
		assert.Equal(t, bus.TypeMsgHuman, e.Type)
	default:
		t.Fatal("缓冲中的事件应可读")
	}
}

// U-F6-03 三个订阅者各自独立收到全量事件。
func TestBusThreeSubscribersAllEvents(t *testing.T) {
	b, err := bus.New(nil, bus.Options{})
	require.NoError(t, err)
	defer b.Close()

	ch1, u1 := b.Subscribe()
	ch2, u2 := b.Subscribe()
	ch3, u3 := b.Subscribe()
	defer u1()
	defer u2()
	defer u3()

	publishN(t, b, 100, bus.TypeMsgSystem)

	readAll := func(ch <-chan bus.Event) []bus.Event {
		var got []bus.Event
		for {
			select {
			case e := <-ch:
				got = append(got, e)
			case <-time.After(300 * time.Millisecond):
				return got
			}
		}
	}
	for i, ch := range []<-chan bus.Event{ch1, ch2, ch3} {
		got := readAll(ch)
		assert.Len(t, got, 100, "订阅者 %d 应收到全量 100 条", i+1)
		for j, e := range got {
			assert.Equal(t, int64(j+1), e.Seq, "订阅者 %d 事件应按 seq 有序", i+1)
		}
	}
}

// 主题过滤：空 = 全部；"msg.*" 前缀通配；精确匹配。
func TestBusTopicFilter(t *testing.T) {
	b, err := bus.New(nil, bus.Options{})
	require.NoError(t, err)
	defer b.Close()

	all, uAll := b.Subscribe()
	msgStar, uMsg := b.Subscribe("msg.*")
	exact, uExact := b.Subscribe(bus.TypeAgentStatus)
	defer uAll()
	defer uMsg()
	defer uExact()

	publishN(t, b, 1, bus.TypeMsgHuman)
	publishN(t, b, 1, bus.TypeMsgAgent)
	publishN(t, b, 1, bus.TypeAgentStatus)

	time.Sleep(100 * time.Millisecond)
	drain := func(ch <-chan bus.Event) int {
		n := 0
		for {
			select {
			case <-ch:
				n++
			default:
				return n
			}
		}
	}
	assert.Equal(t, 3, drain(all))
	assert.Equal(t, 2, drain(msgStar))
	assert.Equal(t, 1, drain(exact))
}

// 重启 seq 恢复：seq 从 sink.MaxEventSeq 继续，不回退不重复。
func TestBusSeqContinuesAfterRestart(t *testing.T) {
	sink := &fakeSink{}
	b1, err := bus.New(sink, bus.Options{FlushInterval: 10 * time.Millisecond})
	require.NoError(t, err)
	publishN(t, b1, 5, bus.TypeMsgSystem)
	require.NoError(t, b1.Close()) // Close 排空落库

	b2, err := bus.New(sink, bus.Options{FlushInterval: 10 * time.Millisecond})
	require.NoError(t, err)
	defer b2.Close()
	require.NoError(t, b2.Publish(context.Background(), bus.Event{Type: bus.TypeMsgSystem, Payload: []byte(`{}`)}))
	assert.Equal(t, int64(6), b2.CurrentSeq(), "新实例 seq 应从 5 续到 6")
	waitFor(t, func() bool { return sink.count() == 6 })
	require.Equal(t, 6, sink.count())
}

// Close 排空：关闭前未 flush 的批量事件必须全部落库。
func TestBusCloseFlushesPending(t *testing.T) {
	sink := &fakeSink{}
	b, err := bus.New(sink, bus.Options{FlushInterval: time.Hour, PersistBatchSize: 1000})
	require.NoError(t, err)
	publishN(t, b, 42, bus.TypeTaskLifecycle)
	assert.Equal(t, 0, sink.count(), "未到批量阈值且 ticker 未触发，应尚未落库")
	require.NoError(t, b.Close())
	assert.Equal(t, 42, sink.count(), "Close 应排空全部待落库事件")
}
