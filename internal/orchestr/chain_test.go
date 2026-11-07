package orchestr_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/adapter/echo"
	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/internal/orchestr"
	"github.com/dddpeter/agent-nexus/internal/registry"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

// flakySend 前 N 次 Send 失败、之后成功（U-F4-03/04 重试语义）。
type flakySend struct {
	mu    sync.Mutex
	fail  int
	calls int
}

func (f *flakySend) Info() adapter.AgentInfo {
	caps := adapter.DefaultCapabilities()
	return adapter.AgentInfo{ID: "flaky", Name: "flaky", Adapter: "flaky", Capabilities: caps}
}
func (f *flakySend) HealthCheck(ctx context.Context) error { return nil }
func (f *flakySend) CreateSession(ctx context.Context, o adapter.SessionOpts) (adapter.SessionID, error) {
	return "flaky-sess", nil
}
func (f *flakySend) Close(ctx context.Context, sid adapter.SessionID) error { return nil }
func (f *flakySend) Stream(ctx context.Context, sid adapter.SessionID, msg adapter.Message) (<-chan adapter.StreamEvent, func(), error) {
	return nil, nil, fmt.Errorf("flaky: no stream")
}
func (f *flakySend) Send(ctx context.Context, sid adapter.SessionID, msg adapter.Message) (adapter.Reply, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	shouldFail := call <= f.fail
	f.mu.Unlock()
	if shouldFail {
		return adapter.Reply{}, fmt.Errorf("flaky failure %d", call)
	}
	return adapter.Reply{Content: fmt.Sprintf("ok#%d:%s", call, msg.Content), Done: true}, nil
}

// hangSend 阻塞直至 ctx 结束（U-F4-05 超时 / U-F4-08 cancel；ctx 感知避免 goroutine 泄漏）。
type hangSend struct{}

func (h *hangSend) Info() adapter.AgentInfo {
	return adapter.AgentInfo{ID: "hang", Name: "hang", Adapter: "hang", Capabilities: adapter.DefaultCapabilities()}
}
func (h *hangSend) HealthCheck(ctx context.Context) error { return nil }
func (h *hangSend) CreateSession(ctx context.Context, o adapter.SessionOpts) (adapter.SessionID, error) {
	return "hang-sess", nil
}
func (h *hangSend) Close(ctx context.Context, sid adapter.SessionID) error { return nil }
func (h *hangSend) Stream(ctx context.Context, sid adapter.SessionID, msg adapter.Message) (<-chan adapter.StreamEvent, func(), error) {
	return nil, nil, fmt.Errorf("hang: no stream")
}
func (h *hangSend) Send(ctx context.Context, sid adapter.SessionID, msg adapter.Message) (adapter.Reply, error) {
	<-ctx.Done()
	return adapter.Reply{}, ctx.Err()
}

type staticFactory struct {
	name string
	ad   adapter.AgentAdapter
}

func (f *staticFactory) Name() string                                         { return f.name }
func (f *staticFactory) ConfigSchema() adapter.Schema                         { return adapter.Schema{} }
func (f *staticFactory) New(cfg adapter.Config) (adapter.AgentAdapter, error) { return f.ad, nil }

type captureBus struct {
	mu     sync.Mutex
	events []bus.Event
}

func (c *captureBus) Publish(_ context.Context, ev bus.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	return nil
}
func (c *captureBus) Subscribe(_ ...string) (<-chan bus.Event, func()) { return nil, func() {} }
func (c *captureBus) CurrentSeq() int64                                { return 0 }
func (c *captureBus) DroppedTotal() int64                              { return 0 }
func (c *captureBus) Close() error                                     { return nil }

type fx struct {
	ob     *orchestr.Builtin
	store  repo.Store
	reg    *registry.Impl
	agents map[string]string
	flaky  *flakySend
	hang   *hangSend
}

func newFx(t *testing.T, flakyFailures int) *fx {
	t.Helper()
	store := repo.NewMemoryStore()
	b := &captureBus{}
	flaky := &flakySend{fail: flakyFailures}
	reg, err := registry.New(store, b, []adapter.AdapterFactory{
		echo.NewFactory(),
		&staticFactory{name: "flaky", ad: flaky},
		&staticFactory{name: "hang", ad: &hangSend{}},
	}, registry.Options{HeartbeatInterval: time.Hour, Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	require.NoError(t, err)
	t.Cleanup(reg.Close)

	ob := orchestr.NewBuiltin(store, b, reg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	f := &fx{ob: ob, store: store, reg: reg, agents: map[string]string{}, flaky: flaky, hang: &hangSend{}}
	for _, name := range []string{"a1", "a2", "a3"} {
		a, err := reg.Register(context.Background(), registry.AgentSpec{Name: name, Adapter: "echod", Config: adapter.Config{"mode": "echo"}})
		require.NoError(t, err)
		f.agents[name] = a.ID
	}
	fl, err := reg.Register(context.Background(), registry.AgentSpec{Name: "flaky-1", Adapter: "flaky", Config: adapter.Config{}})
	require.NoError(t, err)
	f.agents["flaky-1"] = fl.ID
	hg, err := reg.Register(context.Background(), registry.AgentSpec{Name: "hang-1", Adapter: "hang", Config: adapter.Config{}})
	require.NoError(t, err)
	f.agents["hang-1"] = hg.ID
	return f
}

func (f *fx) waitTask(t *testing.T, id string, states ...string) repo.Task {
	t.Helper()
	allowed := map[string]bool{}
	for _, s := range states {
		allowed[s] = true
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		task, err := f.store.Tasks().Get(context.Background(), id)
		require.NoError(t, err)
		if allowed[task.Status] {
			return task
		}
		time.Sleep(20 * time.Millisecond)
	}
	task, _ := f.store.Tasks().Get(context.Background(), id)
	t.Fatalf("task %s 未在 10s 内到达 %v，当前 %s", id, states, task.Status)
	return task
}

func nodeStates(t *testing.T, task repo.Task) []orchestr.NodeState {
	t.Helper()
	require.NotNil(t, task.NodesJSON)
	var states []orchestr.NodeState
	require.NoError(t, json.Unmarshal([]byte(*task.NodesJSON), &states))
	return states
}

func agentNode(name, input string, retries int) orchestr.ChainNode {
	return orchestr.ChainNode{AgentID: name, Input: input, Retries: retries}
}

// U-F4-01：三节点链 A→B→C，C 的输入含 B 输出（{{prev}} 渲染正确）。
func TestChainPrevRendering_U_F4_01(t *testing.T) {
	f := newFx(t, 0)
	id, err := f.ob.SubmitChain(context.Background(), orchestr.ChainRequest{Nodes: []orchestr.ChainNode{
		agentNode(f.agents["a1"], "step-one", 0),
		agentNode(f.agents["a2"], "second -> {{prev}}", 0),
		agentNode(f.agents["a3"], "third got: {{prev}}", 0),
	}})
	require.NoError(t, err)
	task := f.waitTask(t, id, repo.TaskDone)

	states := nodeStates(t, task)
	// echod echo 回显输入：n1 输出 = "second -> step-one"，n2 输出含之
	assert.Contains(t, states[2].Output, "second -> step-one", "C 的输入应含 B 输出")
	require.NotNil(t, task.Result)
	assert.Contains(t, *task.Result, "third got: second -> step-one")
}

// U-F4-02：fan-out 2 分支 + 汇聚 → 两侧均完成才 done。
func TestChainParallelFanIn_U_F4_02(t *testing.T) {
	f := newFx(t, 0)
	id, err := f.ob.SubmitChain(context.Background(), orchestr.ChainRequest{Nodes: []orchestr.ChainNode{
		agentNode(f.agents["a1"], "kick", 0),
		{Input: "split", Parallel: []orchestr.ChainNode{
			agentNode(f.agents["a2"], "left {{prev}}", 0),
			agentNode(f.agents["a3"], "right {{prev}}", 0),
		}},
		agentNode(f.agents["a1"], "join: {{prev}}", 0),
	}})
	require.NoError(t, err)
	task := f.waitTask(t, id, repo.TaskDone)

	states := nodeStates(t, task)
	require.Len(t, states, 5) // n0 + n1 + n1.0 + n1.1 + n2
	assert.Equal(t, repo.TaskDone, states[2].Status)
	assert.Equal(t, repo.TaskDone, states[3].Status)
	require.NotNil(t, task.Result)
	assert.Contains(t, *task.Result, "left kick")
	assert.Contains(t, *task.Result, "right kick")
}

// U-F4-03：节点失败重试（前 2 次失败第 3 次成功）→ retries=3 整链 done。
func TestChainRetryRecovers_U_F4_03(t *testing.T) {
	f := newFx(t, 2)
	id, err := f.ob.SubmitChain(context.Background(), orchestr.ChainRequest{Nodes: []orchestr.ChainNode{
		agentNode(f.agents["flaky-1"], "retry me", 3),
	}})
	require.NoError(t, err)
	task := f.waitTask(t, id, repo.TaskDone)
	states := nodeStates(t, task)
	assert.Equal(t, repo.TaskDone, states[0].Status)
	assert.Equal(t, 2, states[0].Retried, "应记录实际重试次数")
}

// U-F4-04：重试耗尽 → failed，已完成节点结果保留；resume 从失败节点继续（输入模板完整）。
func TestChainRetryExhaustedAndResume_U_F4_04(t *testing.T) {
	f := newFx(t, 99)
	id, err := f.ob.SubmitChain(context.Background(), orchestr.ChainRequest{Nodes: []orchestr.ChainNode{
		agentNode(f.agents["a1"], "first-ok", 0),
		agentNode(f.agents["flaky-1"], "never works {{prev}}", 1),
		agentNode(f.agents["a3"], "never reached", 0),
	}})
	require.NoError(t, err)
	task := f.waitTask(t, id, repo.TaskFailed)
	require.NotNil(t, task.Error)
	assert.Contains(t, *task.Error, "failed after 1 retries")

	states := nodeStates(t, task)
	assert.Equal(t, repo.TaskDone, states[0].Status, "已完成节点结果保留")
	assert.Equal(t, repo.TaskFailed, states[1].Status)
	assert.Equal(t, repo.TaskQueued, states[2].Status, "未触达节点 queued")

	// 恢复 flaky → resume 从失败节点继续 → done（resume 后 flaky Send 成功回显输入）
	f.flaky.mu.Lock()
	f.flaky.fail = 0
	f.flaky.calls = 0
	f.flaky.mu.Unlock()
	id2, err := f.ob.ResumeChain(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, id, id2, "resume 复用原 task 行")
	task2 := f.waitTask(t, id2, repo.TaskDone)
	states2 := nodeStates(t, task2)
	assert.Equal(t, repo.TaskDone, states2[0].Status, "已完成节点不重跑（输出保留）")
	assert.Equal(t, repo.TaskDone, states2[1].Status)
	assert.Contains(t, states2[1].Output, "never works first-ok", "resume 后 {{prev}} 仍指向前序输出")
	assert.Equal(t, repo.TaskDone, states2[2].Status)
}

// U-F4-05：节点超时 → 按超时失败（ctx 感知 hang agent；CLI 型子进程 kill 由 F7 治理联动）。
func TestChainNodeTimeout_U_F4_05(t *testing.T) {
	f := newFx(t, 0)
	id, err := f.ob.SubmitChain(context.Background(), orchestr.ChainRequest{Nodes: []orchestr.ChainNode{
		{AgentID: f.agents["hang-1"], Input: "x", TimeoutSec: 1, Retries: 0},
	}})
	require.NoError(t, err)
	task := f.waitTask(t, id, repo.TaskFailed)
	states := nodeStates(t, task)
	assert.Equal(t, repo.TaskFailed, states[0].Status)
	assert.Contains(t, states[0].Error, "deadline exceeded", "超时应以 deadline exceeded 失败")
}

// U-F4-06：{{prev}} 输出 >64KB → 注入摘要而非全文，原始输出在 nodes_json 存档。
func TestChainPrevTruncation_U_F4_06(t *testing.T) {
	f := newFx(t, 0)
	big := strings.Repeat("X", 70*1024)
	id, err := f.ob.SubmitChain(context.Background(), orchestr.ChainRequest{Nodes: []orchestr.ChainNode{
		agentNode(f.agents["a1"], big, 0),                 // echo 回显 70KB → n0 输出 70KB
		agentNode(f.agents["a2"], "got {{prev}} tail", 0), // {{prev}} 应为摘要
	}})
	require.NoError(t, err)
	task := f.waitTask(t, id, repo.TaskDone)

	states := nodeStates(t, task)
	assert.GreaterOrEqual(t, len(states[0].Output), 70*1024, "原始输出在 nodes_json 存档")
	assert.Less(t, len(states[1].Output), 8*1024, "注入 {{prev}} 的应为摘要")
	assert.Contains(t, states[1].Output, "[prev truncated")
}

// U-F4-07：cron 1min 周期 + agent offline → 跳过记录；恢复后正常触发。
func TestCronOfflineSkipAndRecover_U_F4_07(t *testing.T) {
	f := newFx(t, 0)
	down, err := f.reg.Register(context.Background(), registry.AgentSpec{Name: "down-cron", Adapter: "echod", Config: adapter.Config{"mode": "down"}})
	require.NoError(t, err)
	ok, err := f.reg.Register(context.Background(), registry.AgentSpec{Name: "ok-cron", Adapter: "echod", Config: adapter.Config{"mode": "echo"}})
	require.NoError(t, err)

	_, err = f.ob.CreateCronJob(context.Background(), "cron-down", "* * * * *",
		orchestr.CronTarget{Type: "agent", AgentID: down.ID, Input: "tick"})
	require.NoError(t, err)
	_, err = f.ob.CreateCronJob(context.Background(), "cron-ok", "* * * * *",
		orchestr.CronTarget{Type: "agent", AgentID: ok.ID, Input: "tick-ok"})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.ob.RunCron(ctx, 2*time.Second)

	// 各等一个整分钟边界触发（最多 ~80s）
	deadline := time.Now().Add(80 * time.Second)
	var sawSkip, sawOK bool
	for time.Now().Before(deadline) {
		jobs, _ := f.ob.ListCronJobs(context.Background())
		for _, j := range jobs {
			if j.Name == "cron-down" && j.LastStatus == "skipped_offline" {
				sawSkip = true
			}
			if j.Name == "cron-ok" && j.LastStatus == "ok" && j.LastTaskID != "" {
				sawOK = true
			}
		}
		if sawSkip && sawOK {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	assert.True(t, sawSkip, "offline 触发应跳过并记录 skipped_offline")
	assert.True(t, sawOK, "健康 agent 触发应正常提交任务")
}

// U-F4-08：运行中 cancel → cancelled（节点 ctx 中止，确定性：首节点 hang 5s）。
func TestChainCancel_U_F4_08(t *testing.T) {
	f := newFx(t, 0)
	id, err := f.ob.SubmitChain(context.Background(), orchestr.ChainRequest{Nodes: []orchestr.ChainNode{
		{AgentID: f.agents["hang-1"], Input: "long", TimeoutSec: 5},
		agentNode(f.agents["a1"], "never", 0),
	}})
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond) // 让首节点进入 running
	require.NoError(t, f.ob.Cancel(context.Background(), id))
	task := f.waitTask(t, id, repo.TaskCancelled)
	assert.Equal(t, repo.TaskCancelled, task.Status)

	// 终态守卫：重复 cancel 报错而非改写
	err = f.ob.Cancel(context.Background(), id)
	assert.Error(t, err, "已取消任务再次 cancel 应被拒绝")
}

// U-F4-09：engine=builtin 降级——全部用例即 builtin 实现；重启只清 running/queued，不误伤终态。
func TestBuiltinDegradedMode_U_F4_09(t *testing.T) {
	f := newFx(t, 0)
	id, _ := f.ob.SubmitChain(context.Background(), orchestr.ChainRequest{Nodes: []orchestr.ChainNode{
		agentNode(f.agents["a1"], "done-earlier", 0),
	}})
	done := f.waitTask(t, id, repo.TaskDone)
	_ = orchestr.NewBuiltin(f.store, &captureBus{}, f.reg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	after, _ := f.store.Tasks().Get(context.Background(), done.ID)
	assert.Equal(t, repo.TaskDone, after.Status, "终态任务不被重启清理误伤")
}
