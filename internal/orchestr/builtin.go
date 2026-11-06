// Package orchestr 定义编排层 Scheduler 接口与内置降级实现（technical-design §7）。
//
// 一期不接入 Temporal（部署铁律）；builtin 为 goroutine 调度 + tasks 表记录，
// 无持久化恢复：重启后遗留 running 任务标记 failed（与文档声明的降级差异一致）。
package orchestr

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/internal/id"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

// Job 一次可调度执行的工作单元（一期为私聊异步回报；任务链 DAG 为 M4 范围）。
type Job struct {
	AgentID string
	Timeout time.Duration // 0 = 不限（调用方自行约束）
	Fn      func(ctx context.Context)
}

// TaskStatus 任务状态视图。
type TaskStatus struct {
	ID    string `json:"id"`
	State string `json:"state"` // queued|running|done|failed|cancelled
	Error string `json:"error,omitempty"`
}

// Scheduler 编排层接口（technical-design §7：Submit/Cancel/Status；Temporal 实现同签名，二期接入）。
type Scheduler interface {
	Submit(ctx context.Context, job Job) (workflowID string, err error)
	Cancel(ctx context.Context, workflowID string) error
	Status(ctx context.Context, workflowID string) (TaskStatus, error)
}

// Builtin 内置降级调度器：goroutine + tasks 表（无恢复能力，重启 running → failed）。
type Builtin struct {
	store repo.Store
	bus   bus.Bus
	log   *slog.Logger

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// NewBuiltin 创建内置调度器并清理上次遗留任务（降级语义：running → failed）。
func NewBuiltin(store repo.Store, b bus.Bus, log *slog.Logger) *Builtin {
	if log == nil {
		log = slog.Default()
	}
	ob := &Builtin{store: store, bus: b, log: log, cancels: make(map[string]context.CancelFunc)}
	ob.failStaleRunning()
	return ob
}

func (o *Builtin) failStaleRunning() {
	ctx := context.Background()
	tasks, err := o.store.Tasks().List(ctx)
	if err != nil {
		o.log.Error("builtin: list stale tasks failed", "err", err)
		return
	}
	now := time.Now()
	for _, t := range tasks {
		if t.Status != repo.TaskRunning && t.Status != repo.TaskQueued {
			continue
		}
		msg := "builtin scheduler restarted: no recovery (degraded mode)"
		t.Status = repo.TaskFailed
		t.Error = &msg
		t.FinishedAt = &now
		if err := o.store.Tasks().Update(ctx, t); err != nil {
			o.log.Error("builtin: mark stale task failed", "task", t.ID, "err", err)
		} else {
			o.log.Warn("builtin: stale task marked failed", "task", t.ID)
			o.publishLifecycle(t.ID, repo.TaskFailed, msg)
		}
	}
}

var _ Scheduler = (*Builtin)(nil)

// Submit 排队执行：tasks 表 queued → running → done|failed。
func (o *Builtin) Submit(ctx context.Context, job Job) (string, error) {
	workflowID := "wfb-" + id.New()
	now := time.Now()
	agentID := job.AgentID
	task := repo.Task{
		ID: workflowID, WorkflowID: &workflowID, AgentID: &agentID,
		Status: repo.TaskQueued, CreatedAt: &now,
	}
	if job.Timeout > 0 {
		secs := int(job.Timeout.Seconds())
		task.TimeoutSeconds = &secs
	}
	if err := o.store.Tasks().Create(ctx, task); err != nil {
		return "", fmt.Errorf("orchestr builtin: create task: %w", err)
	}
	o.publishLifecycle(workflowID, repo.TaskQueued, "")

	runCtx, cancel := context.WithCancel(context.Background())
	o.mu.Lock()
	o.cancels[workflowID] = cancel
	o.mu.Unlock()

	go o.run(runCtx, cancel, task, job)
	return workflowID, nil
}

func (o *Builtin) run(ctx context.Context, cancel context.CancelFunc, task repo.Task, job Job) {
	defer cancel()
	if job.Timeout > 0 {
		var c context.CancelFunc
		ctx, c = context.WithTimeout(ctx, job.Timeout)
		defer c()
	}
	fresh, err := o.store.Tasks().Get(context.Background(), task.ID)
	if err != nil {
		o.log.Error("builtin: load task", "task", task.ID, "err", err)
		return
	}
	now := time.Now()
	fresh.Status = repo.TaskRunning
	fresh.CreatedAt = &now
	if err := o.store.Tasks().Update(context.Background(), fresh); err != nil {
		o.log.Error("builtin: mark running", "task", task.ID, "err", err)
	}
	o.publishLifecycle(task.ID, repo.TaskRunning, "")

	job.Fn(ctx)

	final, _ := o.store.Tasks().Get(context.Background(), task.ID)
	fin := time.Now()
	if final.Status == repo.TaskCancelled {
		return // 执行中被取消：Cancel 已落状态
	}
	final.Status = repo.TaskDone
	final.FinishedAt = &fin
	if err := o.store.Tasks().Update(context.Background(), final); err != nil {
		o.log.Error("builtin: mark done", "task", task.ID, "err", err)
	}
	o.publishLifecycle(task.ID, repo.TaskDone, "")

	o.mu.Lock()
	delete(o.cancels, task.ID)
	o.mu.Unlock()
}

// Cancel 取消运行中任务（best-effort：ctx 取消由 job 自行响应）。
func (o *Builtin) Cancel(ctx context.Context, workflowID string) error {
	o.mu.Lock()
	cancel := o.cancels[workflowID]
	o.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	task, err := o.store.Tasks().Get(ctx, workflowID)
	if err != nil {
		return err
	}
	now := time.Now()
	task.Status = repo.TaskCancelled
	task.FinishedAt = &now
	if err := o.store.Tasks().Update(ctx, task); err != nil {
		return err
	}
	o.publishLifecycle(workflowID, repo.TaskCancelled, "canceled by user")
	return nil
}

// Status 查询任务状态。
func (o *Builtin) Status(ctx context.Context, workflowID string) (TaskStatus, error) {
	task, err := o.store.Tasks().Get(ctx, workflowID)
	if err != nil {
		return TaskStatus{}, err
	}
	st := TaskStatus{ID: task.ID, State: task.Status}
	if task.Error != nil {
		st.Error = *task.Error
	}
	return st, nil
}

func (o *Builtin) publishLifecycle(taskID, state, msg string) {
	payload := fmt.Sprintf(`{"task_id":%q,"state":%q,"message":%q}`, taskID, state, msg)
	_ = o.bus.Publish(context.Background(), bus.Event{Type: bus.TypeTaskLifecycle, Payload: []byte(payload)})
}
