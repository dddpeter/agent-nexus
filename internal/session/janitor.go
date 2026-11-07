// Package session 承载会话层的后台维护组件；私聊路由在子包 private。
package session

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

// Janitor 会话回收循环（FR2.5）：周期扫描闲置会话并归档（closed_at 置位，历史保留可查），
// 会话内追加系统消息说明归档原因（聊天页可见）。
type Janitor struct {
	store     repo.Store
	bus       bus.Bus
	idleAfter time.Duration // 闲置阈值（默认 24h）
	interval  time.Duration // 扫描周期（默认 30min）
	log       *slog.Logger
	now       func() time.Time
}

func NewJanitor(store repo.Store, b bus.Bus, idleAfter, interval time.Duration, log *slog.Logger) *Janitor {
	if idleAfter <= 0 {
		idleAfter = 24 * time.Hour
	}
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Janitor{store: store, bus: b, idleAfter: idleAfter, interval: interval, log: log, now: time.Now}
}

// Run 阻塞式扫描循环；ctx 取消即退出（serve 信号联动）。
func (j *Janitor) Run(ctx context.Context) {
	t := time.NewTicker(j.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := j.Sweep(ctx); n > 0 {
				j.log.Info("闲置会话已归档", "count", n, "idle_after", j.idleAfter)
			}
		}
	}
}

// Sweep 单次扫描，返回归档数（测试与手动触发入口）。
func (j *Janitor) Sweep(ctx context.Context) int {
	now := j.now()
	ids, err := j.store.Sessions().CloseIdleBefore(ctx, now.Add(-j.idleAfter), now)
	if err != nil {
		j.log.Error("闲置会话扫描失败", "err", err)
		return 0
	}
	for _, id := range ids {
		j.archiveNotice(ctx, id, now)
	}
	return len(ids)
}

// archiveNotice 归档系统消息 + msg.system 事件（与 private.publishMessage 同契约）。
func (j *Janitor) archiveNotice(ctx context.Context, sessionID string, now time.Time) {
	content := fmt.Sprintf("会话闲置超过 %s，已自动归档（历史保留可查）", j.idleAfter)
	m := repo.Message{
		SessionID: sessionID, SenderType: repo.SenderSystem, SenderID: "nexus",
		Content: content, CreatedAt: now,
	}
	if err := j.store.Messages().Append(ctx, &m); err != nil {
		j.log.Error("归档消息写入失败", "session", sessionID, "err", err)
		return
	}
	agentID := ""
	if sess, err := j.store.Sessions().Get(ctx, sessionID); err == nil && sess.AgentID != nil {
		agentID = *sess.AgentID
	}
	payload := fmt.Sprintf(`{"session_id":%q,"agent_id":%q,"sender_type":"system","sender_id":"nexus","content":%q,"message_id":%d,"reply_to":""}`,
		sessionID, agentID, content, m.ID)
	_ = j.bus.Publish(ctx, bus.Event{Type: bus.TypeMsgSystem, Payload: []byte(payload)})
}
