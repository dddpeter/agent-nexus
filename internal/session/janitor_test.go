package session

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

type captureBus struct {
	events []bus.Event
}

func (c *captureBus) Publish(_ context.Context, ev bus.Event) error {
	c.events = append(c.events, ev)
	return nil
}
func (c *captureBus) Subscribe(_ ...string) (<-chan bus.Event, func()) {
	return nil, func() {}
}
func (c *captureBus) CurrentSeq() int64   { return 0 }
func (c *captureBus) DroppedTotal() int64 { return 0 }
func (c *captureBus) Close() error        { return nil }

// FR2.5：Sweep 仅归档闲置会话；归档消息落库 + msg.system 事件；活跃会话不受影响。
func TestJanitorSweep(t *testing.T) {
	store := repo.NewMemoryStore()
	b := &captureBus{}
	j := NewJanitor(store, b, 24*time.Hour, 30*time.Minute, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	ctx := context.Background()
	now := time.Now()

	// 旧会话：创建于 48h 前，末条消息 30h 前 → 闲置
	oldSess := repo.Session{ID: "s-old", Kind: repo.KindPrivate, CreatedAt: now.Add(-48 * time.Hour)}
	require_NoError(t, store.Sessions().Create(ctx, oldSess))
	require_NoError(t, store.Messages().Append(ctx, &repo.Message{
		SessionID: "s-old", SenderType: repo.SenderHuman, Content: "hi", CreatedAt: now.Add(-30 * time.Hour),
	}))
	// 活跃会话：消息 1h 前 → 不动
	newSess := repo.Session{ID: "s-new", Kind: repo.KindPrivate, CreatedAt: now.Add(-2 * time.Hour)}
	require_NoError(t, store.Sessions().Create(ctx, newSess))
	require_NoError(t, store.Messages().Append(ctx, &repo.Message{
		SessionID: "s-new", SenderType: repo.SenderHuman, Content: "fresh", CreatedAt: now.Add(-1 * time.Hour),
	}))
	// 无消息旧会话：回退 created_at 判定 → 闲置
	bareSess := repo.Session{ID: "s-bare", Kind: repo.KindPrivate, CreatedAt: now.Add(-48 * time.Hour)}
	require_NoError(t, store.Sessions().Create(ctx, bareSess))
	// 已关闭会话：不再重复归档
	closed := now.Add(-48 * time.Hour)
	closedSess := repo.Session{ID: "s-closed", Kind: repo.KindPrivate, CreatedAt: closed, ClosedAt: &closed}
	require_NoError(t, store.Sessions().Create(ctx, closedSess))

	n := j.Sweep(ctx)
	if n != 2 {
		t.Fatalf("Sweep 应归档 2 个闲置会话，实际 %d", n)
	}

	for _, id := range []string{"s-old", "s-bare"} {
		s, err := store.Sessions().Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if s.ClosedAt == nil {
			t.Fatalf("会话 %s 应被归档", id)
		}
	}
	if s, _ := store.Sessions().Get(ctx, "s-new"); s.ClosedAt != nil {
		t.Fatal("活跃会话不应被归档")
	}

	// 归档系统消息已落库（每个闲置会话 1 条）
	msgs, _ := store.Messages().ListBySession(ctx, "s-old")
	if len(msgs) != 2 || msgs[1].SenderType != repo.SenderSystem {
		t.Fatalf("归档会话应追加系统消息，实际 %+v", msgs)
	}

	// msg.system 事件发布
	if len(b.events) != 2 {
		t.Fatalf("应发布 2 条 msg.system 事件，实际 %d", len(b.events))
	}
	var payload struct {
		SessionID  string `json:"session_id"`
		SenderType string `json:"sender_type"`
	}
	if err := json.Unmarshal(b.events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SenderType != "system" || (payload.SessionID != "s-old" && payload.SessionID != "s-bare") {
		t.Fatalf("事件负载不符合契约: %s", b.events[0].Payload)
	}
	if b.events[0].Type != bus.TypeMsgSystem {
		t.Fatalf("事件类型应为 msg.system，实际 %s", b.events[0].Type)
	}

	// 二次 Sweep：已归档会话不再重复处理
	if n := j.Sweep(ctx); n != 0 {
		t.Fatalf("二次 Sweep 应为 0，实际 %d", n)
	}
}

func require_NoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
