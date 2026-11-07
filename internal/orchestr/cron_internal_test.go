// cron 内部回归（2026-11-07 评审）：targetsOffline 曾把 parallel 容器节点的空 AgentID
// 交给 AgentStatus 查询——恒 not-found 判 offline，含 parallel 节点的 chain cron 目标
// 被永久 skipped_offline（agent 全在线也一样）。
package orchestr

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

// statusReg 按表回答 AgentStatus；表外 id（含空串）一律 not-found。
type statusReg struct{ status map[string]string }

func (s *statusReg) Acquire(ctx context.Context, agentID string) (func(), error) {
	return func() {}, nil
}
func (s *statusReg) Adapter(agentID string) (adapter.AgentAdapter, error) {
	return nil, assert.AnError
}
func (s *statusReg) AgentStatus(ctx context.Context, agentID string) (string, error) {
	st, ok := s.status[agentID]
	if !ok {
		return "", assert.AnError
	}
	return st, nil
}

type nopBus struct{}

func (nopBus) Publish(ctx context.Context, e bus.Event) error { return nil }
func (nopBus) Subscribe(topics ...string) (<-chan bus.Event, func()) {
	return nil, func() {}
}
func (nopBus) CurrentSeq() int64   { return 0 }
func (nopBus) DroppedTotal() int64 { return 0 }
func (nopBus) Close() error        { return nil }

// 容器节点（AgentID 空 + Parallel 子节点）不得参与在线检查。
func TestTargetsOfflineSkipsParallelContainerNode(t *testing.T) {
	reg := &statusReg{status: map[string]string{"a1": "online", "a2": "online"}}
	o := &Builtin{reg: reg, log: slog.Default()}

	target := CronTarget{Type: "chain", Chain: &ChainRequest{
		Nodes: []ChainNode{{Parallel: []ChainNode{
			{AgentID: "a1", Input: "x"}, {AgentID: "a2", Input: "y"},
		}}},
	}}
	offline, agent := o.targetsOffline(context.Background(), target)
	assert.False(t, offline, "全部真实 agent 在线时不应判 offline（容器节点空 AgentID 曾被误判）")
	assert.Empty(t, agent)
}

// 真实 agent offline 仍须正确拦截（防修复过宽）。
func TestTargetsOfflineStillChecksRealAgents(t *testing.T) {
	reg := &statusReg{status: map[string]string{"a1": "online", "a2": "offline"}}
	o := &Builtin{reg: reg, log: slog.Default()}

	target := CronTarget{Type: "chain", Chain: &ChainRequest{
		Nodes: []ChainNode{
			{AgentID: "a1", Input: "x"},
			{Parallel: []ChainNode{{AgentID: "a2", Input: "y"}}},
		},
	}}
	offline, agent := o.targetsOffline(context.Background(), target)
	assert.True(t, offline)
	assert.Equal(t, "a2", agent)
}

// fireCron 端到端：parallel 容器节点 chain 目标应正常触发（LastStatus=ok 且建 task）。
func TestFireCronChainWithParallelNodeFires(t *testing.T) {
	store := repo.NewMemoryStore()
	reg := &statusReg{status: map[string]string{"a1": "online", "a2": "online"}}
	o := &Builtin{store: store, bus: nopBus{}, reg: reg, log: slog.Default(),
		cancels: make(map[string]context.CancelFunc)}

	target := CronTarget{Type: "chain", Chain: &ChainRequest{
		Nodes: []ChainNode{{Parallel: []ChainNode{
			{AgentID: "a1", Input: "x"}, {AgentID: "a2", Input: "y"},
		}}},
	}}
	raw, err := json.Marshal(target)
	require.NoError(t, err)
	job := repo.CronJob{ID: "cron-p", Name: "parallel-cron", CronExpr: "* * * * *",
		TargetJSON: string(raw), Enabled: true, CreatedAt: time.Now()}
	require.NoError(t, store.CronJobs().Create(context.Background(), job))

	jobs, err := store.CronJobs().List(context.Background())
	require.NoError(t, err)
	require.Len(t, jobs, 1)

	o.fireCron(context.Background(), jobs[0], time.Now())

	jobs, err = store.CronJobs().List(context.Background())
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "ok", jobs[0].LastStatus, "agent 全在线时不应记 skipped_offline")
	assert.NotEmpty(t, jobs[0].LastTaskID)

	tasks, err := store.Tasks().List(context.Background())
	require.NoError(t, err)
	assert.Len(t, tasks, 1, "应提交一个 chain 任务")
}

// bus 包导入锚点：Event 类型来自 bus（nopBus 实现约束）。
var _ bus.Bus = nopBus{}
