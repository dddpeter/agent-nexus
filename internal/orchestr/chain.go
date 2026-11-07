// chain.go 实现协作链 DAG 执行引擎（F4.1/4.2，detailed-design §4.3）：
// 顺序节点 {{prev}}/{{task.input}} 模板注入；Parallel 子节点 fan-out 并发 + 汇聚等待；
// 节点级重试（指数退避）与超时；节点状态/输出实时持久化 nodes_json + task.lifecycle 事件；
// cancel（ctx 贯穿到 agent 调用，CLI 型子进程被 F7 治理 kill）与 resume（从首个非 done 节点续跑）。
// 执行计划状态驱动：NodeState 同时承载定义与进度，resume 无需外部请求参数。
package orchestr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/id"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

// ChainRegistry chain 执行器所需注册层能力（registry.Impl 满足）。
type ChainRegistry interface {
	Acquire(ctx context.Context, agentID string) (release func(), err error)
	Adapter(agentID string) (adapter.AgentAdapter, error)
	// AgentStatus 探针读数（cron 触发前 offline 跳过用，U-F4-07）。
	AgentStatus(ctx context.Context, agentID string) (string, error)
}

// ChainNode 链节点定义（F4.1）。
type ChainNode struct {
	AgentID    string      `json:"agent_id"`
	Input      string      `json:"input"`
	TimeoutSec int         `json:"timeout_seconds,omitempty"`
	Retries    int         `json:"retries,omitempty"`
	Parallel   []ChainNode `json:"parallel,omitempty"` // fan-out：兄弟并发，全部完成才汇聚（F4.2）
}

// ChainRequest POST /api/tasks 的 chain 负载。
type ChainRequest struct {
	Nodes []ChainNode `json:"nodes"`
	Input string      `json:"input"`
}

// NodeState 节点运行态 + 定义（tasks.nodes_json 内嵌，F4.3）。
// 定义字段随状态持久化——resume 时可完整重建执行计划（含 parallel 子节点定义）。
type NodeState struct {
	ID         string `json:"id"` // 路径 id："0","1","1.0"（parallel 子节点）
	AgentID    string `json:"agent_id"`
	Status     string `json:"status"` // queued|running|done|failed
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	Retried    int    `json:"retried,omitempty"` // 实际已重试次数
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	// —— 定义（持久化以支持 resume）——
	Input      string      `json:"input,omitempty"`
	Retries    int         `json:"retries,omitempty"`
	TimeoutSec int         `json:"timeout_seconds,omitempty"`
	Parallel   []ChainNode `json:"parallel,omitempty"`
}

// prevTruncateLimit {{prev}} 注入阈值（U-F4-06：>64KB 注入摘要而非全文，原始输出在存档）。
const prevTruncateLimit = 64 * 1024

// ChainAgentError 节点 agent 调用失败（可重试类）。
type ChainAgentError struct{ Msg string }

func (e *ChainAgentError) Error() string { return e.Msg }

// SubmitChain 提交协作链：建 task（queued）→ goroutine 跑 DAG。返回 task id。
func (o *Builtin) SubmitChain(ctx context.Context, req ChainRequest) (string, error) {
	if len(req.Nodes) == 0 {
		return "", fmt.Errorf("orchestr: chain nodes required")
	}
	if err := validateNodes(req.Nodes); err != nil {
		return "", err
	}
	taskID := "task-" + id.New()
	states := initialStates(req.Nodes)
	raw, _ := json.Marshal(states)
	now := time.Now()
	task := repo.Task{
		ID: taskID, Input: ptrStr(req.Input), Status: repo.TaskQueued,
		NodesJSON: ptrStr(string(raw)), CreatedAt: &now,
	}
	if err := o.store.Tasks().Create(ctx, task); err != nil {
		return "", fmt.Errorf("orchestr: create chain task: %w", err)
	}
	o.publishLifecycle(taskID, repo.TaskQueued, fmt.Sprintf("chain %d nodes", len(req.Nodes)))

	runCtx, cancel := context.WithCancel(context.Background())
	o.mu.Lock()
	o.cancels[taskID] = cancel
	o.mu.Unlock()
	go func() {
		defer cancel()
		o.runChain(runCtx, taskID)
	}()
	return taskID, nil
}

// ResumeChain 从失败节点继续（U-F4-04）：已完成节点输出复用为 prev，首个非 done 节点起重跑。
// 复用原 task 行；nodes_json 既是计划也是进度（定义内嵌，无需外部参数）。
func (o *Builtin) ResumeChain(ctx context.Context, taskID string) (string, error) {
	task, err := o.store.Tasks().Get(ctx, taskID)
	if err != nil {
		return "", err
	}
	if task.Status != repo.TaskFailed {
		return "", fmt.Errorf("orchestr: resume requires failed task, got %s", task.Status)
	}
	if task.NodesJSON == nil || *task.NodesJSON == "" {
		return "", fmt.Errorf("orchestr: task %s has no nodes to resume", taskID)
	}
	var states []NodeState
	if err := json.Unmarshal([]byte(*task.NodesJSON), &states); err != nil {
		return "", fmt.Errorf("orchestr: nodes_json corrupted: %w", err)
	}
	for i := range states {
		if states[i].Status != repo.TaskDone {
			states[i] = NodeState{ID: states[i].ID, AgentID: states[i].AgentID,
				Input: states[i].Input, Retries: states[i].Retries, TimeoutSec: states[i].TimeoutSec,
				Parallel: states[i].Parallel, Status: repo.TaskQueued}
		}
	}
	raw, _ := json.Marshal(states)
	now := time.Now()
	task.Status = repo.TaskQueued
	task.Error = nil
	task.FinishedAt = nil
	task.CreatedAt = &now
	task.NodesJSON = ptrStr(string(raw))
	if err := o.store.Tasks().Update(ctx, task); err != nil {
		return "", err
	}
	o.publishLifecycle(taskID, repo.TaskQueued, "resumed")

	runCtx, cancel := context.WithCancel(context.Background())
	o.mu.Lock()
	o.cancels[taskID] = cancel
	o.mu.Unlock()
	go func() {
		defer cancel()
		o.runChain(runCtx, taskID)
	}()
	return taskID, nil
}

func validateNodes(nodes []ChainNode) error {
	for i, n := range nodes {
		// parallel 容器节点仅 fan-out 不调用 agent，AgentID 可空
		if n.AgentID == "" && len(n.Parallel) == 0 {
			return fmt.Errorf("orchestr: node %d: agent_id required", i)
		}
		for j := range n.Parallel {
			if n.Parallel[j].AgentID == "" {
				return fmt.Errorf("orchestr: node %d.%d: agent_id required", i, j)
			}
		}
	}
	return nil
}

func initialStates(nodes []ChainNode) []NodeState {
	var out []NodeState
	for i, n := range nodes {
		out = append(out, NodeState{ID: fmt.Sprint(i), AgentID: n.AgentID, Status: repo.TaskQueued,
			Input: n.Input, Retries: n.Retries, TimeoutSec: n.TimeoutSec, Parallel: n.Parallel})
		for j := range n.Parallel {
			out = append(out, NodeState{ID: fmt.Sprintf("%d.%d", i, j), AgentID: n.Parallel[j].AgentID,
				Status: repo.TaskQueued, Input: n.Parallel[j].Input, Retries: n.Parallel[j].Retries,
				TimeoutSec: n.Parallel[j].TimeoutSec})
		}
	}
	return out
}

// runChain DAG 主流程：顺序执行顶层节点；失败即整链 failed（已完成节点结果保留，F4.4）。
func (o *Builtin) runChain(ctx context.Context, taskID string) {
	o.markChainRunning(taskID)
	task, err := o.store.Tasks().Get(context.Background(), taskID)
	if err != nil {
		o.log.Error("chain: load task", "task", taskID, "err", err)
		return
	}
	var states []NodeState
	if task.NodesJSON != nil {
		if err := json.Unmarshal([]byte(*task.NodesJSON), &states); err != nil {
			o.log.Error("chain: nodes_json corrupted", "task", taskID, "err", err)
			return
		}
	}
	prev := ""
	if task.Input != nil {
		prev = *task.Input // 首节点 {{prev}}/{{task.input}} 同源（任务初始输入）
	}
	failed := false
	var failErr error

	for i := range states {
		st := states[i]
		if strings.Contains(st.ID, ".") {
			continue // parallel 子节点由父节点调度
		}
		if st.Status == repo.TaskDone {
			prev = st.Output // resume：已完成节点输出复用（U-F4-04）
			continue
		}
		def := ChainNode{AgentID: st.AgentID, Input: st.Input, Retries: st.Retries,
			TimeoutSec: st.TimeoutSec, Parallel: st.Parallel}
		out, err := o.runNode(ctx, taskID, st.ID, def, prev)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return // cancelled：Cancel 已落状态（U-F4-08）
			}
			failed = true
			failErr = err
			break
		}
		prev = out
	}

	task, err = o.store.Tasks().Get(context.Background(), taskID)
	if err != nil {
		o.log.Error("chain: reload task", "task", taskID, "err", err)
		return
	}
	now := time.Now()
	if task.Status == repo.TaskCancelled {
		return
	}
	if failed {
		task.Status = repo.TaskFailed
		msg := failErr.Error()
		task.Error = &msg
		o.publishLifecycle(taskID, repo.TaskFailed, msg)
	} else {
		task.Status = repo.TaskDone
		res := prev
		task.Result = &res
		o.publishLifecycle(taskID, repo.TaskDone, "")
	}
	task.FinishedAt = &now
	if err := o.store.Tasks().Update(context.Background(), task); err != nil {
		o.log.Error("chain: final update", "task", taskID, "err", err)
	}
	o.mu.Lock()
	delete(o.cancels, taskID)
	o.mu.Unlock()
}

// runNode 单节点执行（含 parallel fan-out / 重试 / 超时），返回节点输出。
func (o *Builtin) runNode(ctx context.Context, taskID, nodePath string, node ChainNode, prev string) (string, error) {
	if len(node.Parallel) > 0 {
		return o.runParallel(ctx, taskID, nodePath, node, prev)
	}
	input := renderTemplate(node.Input, prev)
	out, err := o.invokeWithRetry(ctx, taskID, nodePath, node, input)
	if err != nil {
		return "", err
	}
	// 下一节点的 {{prev}}：>64KB 时注入摘要（原始全量已在 nodes_json 存档，U-F4-06）
	return truncatePrev(out, prevTruncateLimit), nil
}

// runParallel fan-out：兄弟节点并发执行，汇聚等待全部终态；任一失败整节点失败（U-F4-02）。
func (o *Builtin) runParallel(ctx context.Context, taskID, nodePath string, node ChainNode, prev string) (string, error) {
	type parResult struct {
		idx int
		out string
		err error
	}
	results := make(chan parResult, len(node.Parallel))
	var wg sync.WaitGroup
	for j := range node.Parallel {
		wg.Add(1)
		go func(j int) {
			defer wg.Done()
			sub := node.Parallel[j]
			input := renderTemplate(sub.Input, prev)
			out, err := o.invokeWithRetry(ctx, taskID, fmt.Sprintf("%s.%d", nodePath, j), sub, input)
			results <- parResult{idx: j, out: out, err: err}
		}(j)
	}
	go func() { wg.Wait(); close(results) }()

	outputs := make([]string, len(node.Parallel))
	var firstErr error
	for r := range results {
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		if r.idx < len(outputs) {
			outputs[r.idx] = r.out
		}
	}
	if firstErr != nil {
		return "", firstErr
	}
	return truncatePrev(strings.Join(outputs, "\n"), prevTruncateLimit), nil
}

// invokeWithRetry 节点级重试（指数退避 200ms<<n）+ 超时；状态实时落 nodes_json。
func (o *Builtin) invokeWithRetry(ctx context.Context, taskID, nodePath string, node ChainNode, input string) (string, error) {
	maxRetries := node.Retries
	if node.TimeoutSec <= 0 {
		node.TimeoutSec = 300
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(200<<(attempt-1)) * time.Millisecond
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(backoff):
			}
		}
		o.updateNodeState(taskID, nodePath, func(s *NodeState) {
			s.Status = repo.TaskRunning
			s.Retried = attempt
			if s.StartedAt == "" {
				s.StartedAt = time.Now().UTC().Format(time.RFC3339)
			}
		})
		o.publishLifecycle(taskID, repo.TaskRunning, fmt.Sprintf("node %s attempt %d", nodePath, attempt+1))

		nctx, cancel := context.WithTimeout(ctx, time.Duration(node.TimeoutSec)*time.Second)
		out, err := o.invokeAgent(nctx, node.AgentID, input)
		cancel()
		if err == nil {
			o.updateNodeState(taskID, nodePath, func(s *NodeState) {
				s.Status = repo.TaskDone
				s.Output = out
				s.FinishedAt = time.Now().UTC().Format(time.RFC3339)
			})
			return out, nil
		}
		lastErr = err
		o.updateNodeState(taskID, nodePath, func(s *NodeState) {
			if attempt == maxRetries {
				s.Status = repo.TaskFailed
				s.Error = err.Error()
				s.FinishedAt = time.Now().UTC().Format(time.RFC3339)
			}
		})
		o.log.Warn("chain node failed", "task", taskID, "node", nodePath, "attempt", attempt+1, "err", err)
	}
	return "", &ChainAgentError{Msg: fmt.Sprintf("node %s (%s) failed after %d retries: %v", nodePath, node.AgentID, maxRetries, lastErr)}
}

// invokeAgent 单次 agent 调用（Acquire 闸门 + 同步 Send；ctx 贯穿保证 cancel/timeout 可中止）。
func (o *Builtin) invokeAgent(ctx context.Context, agentID, input string) (string, error) {
	release, err := o.reg.Acquire(ctx, agentID)
	if err != nil {
		return "", &ChainAgentError{Msg: fmt.Sprintf("acquire %s: %v", agentID, err)}
	}
	defer release()
	ad, err := o.reg.Adapter(agentID)
	if err != nil {
		return "", &ChainAgentError{Msg: err.Error()}
	}
	reply, err := ad.Send(ctx, adapter.SessionID("chain-"+agentID), adapter.Message{Content: input})
	if err != nil {
		return "", &ChainAgentError{Msg: fmt.Sprintf("send %s: %v", agentID, err)}
	}
	return reply.Content, nil
}

// updateNodeState 原子更新 nodes_json 中指定节点（nodeMu 串行化 parallel 并发写）。
func (o *Builtin) updateNodeState(taskID, nodePath string, mutate func(*NodeState)) {
	o.nodeMu.Lock()
	defer o.nodeMu.Unlock()
	task, err := o.store.Tasks().Get(context.Background(), taskID)
	if err != nil || task.NodesJSON == nil {
		return
	}
	var states []NodeState
	if err := json.Unmarshal([]byte(*task.NodesJSON), &states); err != nil {
		return
	}
	for i := range states {
		if states[i].ID == nodePath {
			mutate(&states[i])
			break
		}
	}
	raw, err := json.Marshal(states)
	if err != nil {
		return
	}
	task.NodesJSON = ptrStr(string(raw))
	_ = o.store.Tasks().Update(context.Background(), task)
}

func (o *Builtin) markChainRunning(taskID string) {
	task, err := o.store.Tasks().Get(context.Background(), taskID)
	if err != nil {
		return
	}
	now := time.Now()
	task.Status = repo.TaskRunning
	if task.CreatedAt == nil {
		task.CreatedAt = &now
	}
	_ = o.store.Tasks().Update(context.Background(), task)
	o.publishLifecycle(taskID, repo.TaskRunning, "")
}

// renderTemplate 渲染 {{prev}}/{{task.input}}；prev 超限注入摘要（U-F4-06）。
func renderTemplate(tpl, prev string) string {
	out := strings.ReplaceAll(tpl, "{{task.input}}", truncatePrev(prev, prevTruncateLimit))
	out = strings.ReplaceAll(out, "{{prev}}", truncatePrev(prev, prevTruncateLimit))
	return out
}

// truncatePrev 超 limit 截断 + 摘要标记（原始输出由 nodes_json.output 存档）。
func truncatePrev(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	head := s[:2048]
	return fmt.Sprintf("%s\n[prev truncated: showing first 2KB of %d bytes; full output archived in node result]", head, len(s))
}

func ptrStr(s string) *string { return &s }
