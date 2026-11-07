// cron.go 实现定时任务（F4.3 / U-F4-07）：5 字段 cron 表达式（分 时 日 月 周，支持 * , - /）
// + 调度循环（tick 粒度可注入，测试用）；触发 = 提交 chain/单 agent 任务；
// agent offline → 跳过本次并记录（last_status=skipped_offline，F4.4）；重启加载 enabled 任务恢复调度。
package orchestr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/dddpeter/agent-nexus/internal/id"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

// CronTarget 定时触发目标。
type CronTarget struct {
	Type    string        `json:"type"` // "agent" | "chain"
	AgentID string        `json:"agent_id,omitempty"`
	Input   string        `json:"input,omitempty"`
	Chain   *ChainRequest `json:"chain,omitempty"`
}

// CronJob 对外视图（repo.CronJob + 结构化 target）。
type CronJobView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CronExpr   string     `json:"cron_expr"`
	Target     CronTarget `json:"target"`
	Enabled    bool       `json:"enabled"`
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
	LastStatus string     `json:"last_status,omitempty"`
	LastTaskID string     `json:"last_task_id,omitempty"`
}

// ParseCron 解析 5 字段 cron 表达式 → 各字段集合；返回错误含字段名。
// 语法：* 任意；逗号列表；a-b 区间；a/b 步进（含 */n）。月/周日名不支持（保持最小实现）。
func ParseCron(expr string) (minutes, hours, doms, months, dows map[int]bool, err error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, nil, nil, nil, nil, fmt.Errorf("cron: 需要 5 字段（分 时 日 月 周），收到 %d 个", len(fields))
	}
	ranges := []struct {
		min, max int
		name     string
	}{{0, 59, "分钟"}, {0, 23, "小时"}, {1, 31, "日"}, {1, 12, "月"}, {0, 6, "周"}}
	sets := make([]map[int]bool, 5)
	for i, f := range fields {
		s, err := parseCronField(f, ranges[i].min, ranges[i].max)
		if err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("cron: %s 字段 %q: %w", ranges[i].name, f, err)
		}
		sets[i] = s
	}
	return sets[0], sets[1], sets[2], sets[3], sets[4], nil
}

func parseCronField(f string, min, max int) (map[int]bool, error) {
	out := map[int]bool{}
	for _, part := range strings.Split(f, ",") {
		step := 1
		body := part
		if i := strings.Index(part, "/"); i >= 0 {
			body = part[:i]
			s, err := strconv.Atoi(part[i+1:])
			if err != nil || s <= 0 {
				return nil, fmt.Errorf("步进非法")
			}
			step = s
		}
		lo, hi := min, max
		switch {
		case body == "*":
		case strings.Contains(body, "-"):
			bounds := strings.SplitN(body, "-", 2)
			a, err1 := strconv.Atoi(bounds[0])
			b, err2 := strconv.Atoi(bounds[1])
			if err1 != nil || err2 != nil || a > b || a < min || b > max {
				return nil, fmt.Errorf("区间非法")
			}
			lo, hi = a, b
		default:
			v, err := strconv.Atoi(body)
			if err != nil || v < min || v > max {
				return nil, fmt.Errorf("值越界或非法")
			}
			lo, hi = v, v
		}
		for v := lo; v <= hi; v += step {
			out[v] = true
		}
	}
	return out, nil
}

// NextAfter 计算 expr 在 after 之后的下一次触发时刻（分钟粒度）。
func NextAfter(expr string, after time.Time) (time.Time, error) {
	mins, hrs, doms, mons, dows, err := ParseCron(expr)
	if err != nil {
		return time.Time{}, err
	}
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(0, 0, 366*5) // 5 年兜底（2/29 由月日联合判定覆盖）
	for t.Before(limit) {
		if mons[int(t.Month())] && doms[t.Day()] && dows[int(t.Weekday())] && hrs[t.Hour()] && mins[t.Minute()] {
			return t, nil
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("cron: %q 五年内无触发点", expr)
}

// CreateCronJob 登记定时任务（立即持久化并进入调度）。
func (o *Builtin) CreateCronJob(ctx context.Context, name, expr string, target CronTarget) (CronJobView, error) {
	if _, _, _, _, _, perr := ParseCron(expr); perr != nil {
		return CronJobView{}, perr
	}
	if name == "" {
		return CronJobView{}, fmt.Errorf("orchestr: cron name required")
	}
	raw, err := json.Marshal(target)
	if err != nil {
		return CronJobView{}, err
	}
	rec := repo.CronJob{
		ID: "cron-" + id.New(), Name: name, CronExpr: expr,
		TargetJSON: string(raw), Enabled: true, CreatedAt: time.Now(),
	}
	if err := o.store.CronJobs().Create(ctx, rec); err != nil {
		return CronJobView{}, err
	}
	return cronView(rec), nil
}

// DeleteCronJob 撤销定时任务。
func (o *Builtin) DeleteCronJob(ctx context.Context, cronID string) error {
	return o.store.CronJobs().Delete(ctx, cronID)
}

// ListCronJobs 定时任务视图。
func (o *Builtin) ListCronJobs(ctx context.Context) ([]CronJobView, error) {
	jobs, err := o.store.CronJobs().List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CronJobView, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, cronView(j))
	}
	return out, nil
}

func cronView(j repo.CronJob) CronJobView {
	var t CronTarget
	_ = json.Unmarshal([]byte(j.TargetJSON), &t)
	return CronJobView{
		ID: j.ID, Name: j.Name, CronExpr: j.CronExpr, Target: t, Enabled: j.Enabled,
		LastRunAt: j.LastRunAt, LastStatus: j.LastStatus, LastTaskID: j.LastTaskID,
	}
}

// RunCron 调度循环：每 tick 检查到期任务（last_run_at < 上一分钟边界 && 本分钟匹配）。
// tick 间隔可注入（默认 30s；测试传更小值）。ctx 取消即退出。
func (o *Builtin) RunCron(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = 30 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.tickCron(ctx)
		}
	}
}

func (o *Builtin) tickCron(ctx context.Context) {
	jobs, err := o.store.CronJobs().List(ctx)
	if err != nil {
		o.log.Error("cron: list failed", "err", err)
		return
	}
	now := time.Now()
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		// 本分钟已触发过则跳过（last_run_at >= 本分钟起点）
		if j.LastRunAt != nil && !j.LastRunAt.Before(now.Truncate(time.Minute)) {
			continue
		}
		mins, hrs, doms, mons, dows, perr := ParseCron(j.CronExpr)
		if perr != nil {
			o.log.Error("cron: bad expr", "job", j.Name, "err", perr)
			continue
		}
		if !(mons[int(now.Month())] && doms[now.Day()] && dows[int(now.Weekday())] && hrs[now.Hour()] && mins[now.Minute()]) {
			continue
		}
		o.fireCron(ctx, j, now)
	}
}

// fireCron 触发一次：提交目标 → 回写 last_run/last_status/last_task_id。
// agent offline → skipped_offline（F4.4：跳过记录，不影响后续调度）。
func (o *Builtin) fireCron(ctx context.Context, j repo.CronJob, now time.Time) {
	var target CronTarget
	if err := json.Unmarshal([]byte(j.TargetJSON), &target); err != nil {
		o.log.Error("cron: target corrupted", "job", j.Name, "err", err)
		return
	}
	// 前置检查（U-F4-07）：目标 agent 非 online → 跳过本次并记录，不影响后续调度
	if offline, agent := o.targetsOffline(ctx, target); offline {
		j.LastRunAt = &now
		j.LastStatus = "skipped_offline"
		j.LastTaskID = ""
		if err := o.store.CronJobs().Update(ctx, j); err != nil {
			o.log.Error("cron: update skipped failed", "job", j.Name, "err", err)
		}
		o.log.Warn("cron skipped: agent offline", "job", j.Name, "agent", agent)
		return
	}

	var taskID, status string
	switch target.Type {
	case "chain":
		if target.Chain == nil {
			status = "failed"
			break
		}
		tid, err := o.SubmitChain(ctx, *target.Chain)
		if err != nil {
			o.log.Error("cron: submit chain failed", "job", j.Name, "err", err)
			status = "failed"
		} else {
			taskID, status = tid, "ok"
		}
	case "agent":
		tid, err := o.Submit(ctx, Job{AgentID: target.AgentID, Fn: func(taskCtx context.Context) {
			if _, serr := o.invokeAgent(taskCtx, target.AgentID, target.Input); serr != nil {
				o.log.Warn("cron agent job failed", "job", j.Name, "err", serr)
			}
		}})
		if err != nil {
			o.log.Error("cron: submit agent job failed", "job", j.Name, "err", err)
			status = "failed"
		} else {
			taskID, status = tid, "ok"
		}
	default:
		status = "failed"
	}

	j.LastRunAt = &now
	j.LastStatus = status
	j.LastTaskID = taskID
	if err := o.store.CronJobs().Update(ctx, j); err != nil {
		o.log.Error("cron: update last_run failed", "job", j.Name, "err", err)
	}
	o.log.Info("cron fired", "job", j.Name, "status", status, "task", taskID)
}

// targetsOffline 目标涉及的 agent 是否有非 online 者（返回首个 offline 的 agent id）。
// chain 目标检查全部节点（含 parallel 子节点）；parallel 容器节点仅 fan-out 无 agent
// （AgentID 为空，validateNodes 允许），不能参与检查——否则 AgentStatus("") 恒 not-found，
// 含 parallel 节点的 chain 目标会被永久 skipped_offline（2026-11-07 评审修复）。
func (o *Builtin) targetsOffline(ctx context.Context, t CronTarget) (bool, string) {
	var ids []string
	switch t.Type {
	case "agent":
		ids = append(ids, t.AgentID)
	case "chain":
		if t.Chain != nil {
			for _, n := range t.Chain.Nodes {
				if n.AgentID != "" {
					ids = append(ids, n.AgentID)
				}
				for _, p := range n.Parallel {
					ids = append(ids, p.AgentID)
				}
			}
		}
	}
	for _, id := range ids {
		st, err := o.reg.AgentStatus(ctx, id)
		if err != nil || st != "online" {
			return true, id
		}
	}
	return false, ""
}

var _ = slog.Default
