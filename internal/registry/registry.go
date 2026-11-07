// Package registry 实现 Agent Registry（F1）：注册/状态机/心跳/并发闸门 + AdapterFactory 聚合。
package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/internal/id"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

// 常见错误。
var (
	// ErrAgentOffline offline 期间收到 Acquire（F1.4）；编排层转可重试错误。
	ErrAgentOffline = errors.New("registry: agent offline")
	// ErrAgentRemoved 软删后不可用（历史保留）。
	ErrAgentRemoved = errors.New("registry: agent removed")
	// ErrAgentDisabled 手动停用（enable 恢复）。
	ErrAgentDisabled = errors.New("registry: agent disabled")
	// ErrDuplicateName 重名注册（U-F1-03 → API 409）。
	ErrDuplicateName = errors.New("registry: duplicate agent name")
	// ErrBadCapabilities 非法能力声明（MaxConcurrency=0 等，U-F1-07）。
	ErrBadCapabilities = errors.New("registry: invalid capabilities")
)

// Options 心跳与探活配置（technical-design §9 registry 段）。
type Options struct {
	HeartbeatInterval time.Duration // 默认 15s
	OfflineThreshold  int           // 连续失败阈值，默认 3
	ProbeTimeout      time.Duration // 探活超时（CLI 型探活命令 5s，F1.3），默认 5s
	Logger            *slog.Logger
	// Now 可注入时钟（测试）。
	Now func() time.Time
}

func (o *Options) normalize() {
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 15 * time.Second
	}
	if o.OfflineThreshold <= 0 {
		o.OfflineThreshold = 3
	}
	if o.ProbeTimeout <= 0 {
		o.ProbeTimeout = 5 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// AgentSpec 注册规格（Web UI / REST / 配置文件三途径收敛到此，FR1.1）。
type AgentSpec struct {
	Name         string
	Adapter      string
	Config       adapter.Config
	Capabilities *adapter.Capabilities // 覆盖 adapter 默认能力（可部分）
}

// Agent 注册层视角的 agent（持久化 + 运行时）。
type Agent struct {
	ID              string
	Name            string
	Adapter         string
	Config          adapter.Config // 已解析 ${ENV} 的实际配置（内存态；对外展示须 Mask）
	Capabilities    adapter.Capabilities
	Status          string // online|offline|busy|removed（busy 为派生展示态）
	LastHeartbeatAt *time.Time
	InFlight        int // 并发闸门占用数（派生 busy）
	Disabled        bool
}

// StatusFilter 列表过滤。
type StatusFilter = repo.AgentFilter

// agentRT 单 agent 运行时状态。
type agentRT struct {
	mu          sync.Mutex
	agent       Agent
	adapter     adapter.AgentAdapter
	sem         *semaphore.Weighted
	inFlight    int
	consecFails int
	disabled    bool
	stopBeat    chan struct{}
}

// Registry 是注册层公开接口（detailed-design F1.3）。
type Registry interface {
	Register(ctx context.Context, spec AgentSpec) (Agent, error)
	Get(ctx context.Context, agentID string) (Agent, error)
	List(ctx context.Context, f StatusFilter) ([]Agent, error)
	// Acquire 并发闸门：容量 = MaxConcurrency，满则排队（F1.5）。
	// offline/removed/disabled 返回对应错误。
	Acquire(ctx context.Context, agentID string) (release func(), err error)
	// Heartbeat 手动执行一次探活（心跳循环内部同逻辑；测试用）。
	Heartbeat(ctx context.Context, agentID string) error
}

type Impl struct {
	store     repo.Store
	bus       bus.Bus
	factories map[string]adapter.AdapterFactory
	opts      Options
	log       *slog.Logger

	mu     sync.RWMutex
	agents map[string]*agentRT // by agent id

	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New 创建 registry；store/bus 为底层依赖，factories 为已安装的 adapter 工厂集合。
func New(store repo.Store, b bus.Bus, factories []adapter.AdapterFactory, opts Options) (*Impl, error) {
	opts.normalize()
	r := &Impl{
		store:     store,
		bus:       b,
		factories: make(map[string]adapter.AdapterFactory, len(factories)),
		opts:      opts,
		log:       opts.Logger,
		agents:    make(map[string]*agentRT),
	}
	for _, f := range factories {
		r.factories[f.Name()] = f
	}
	return r, nil
}

// Factory 返回指定类型的工厂（nil 表示未安装）。
func (r *Impl) Factory(name string) adapter.AdapterFactory { return r.factories[name] }

// Factories 全部已安装工厂（GET /api/adapters 数据源；按名字稳定排序）。
func (r *Impl) Factories() []adapter.AdapterFactory {
	out := make([]adapter.AdapterFactory, 0, len(r.factories))
	for _, f := range r.factories {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// TestResult 连通性测试结果（FR1.1a / C-F1-03）。
type TestResult struct {
	OK       bool          `json:"ok"`
	Duration time.Duration `json:"-"`
	// DurationMs 浮点毫秒（微秒级测试也非零）。
	DurationMs float64 `json:"duration_ms"`
	Err        string  `json:"error,omitempty"`
}

// TestConnection 用待保存配置临时实例化 adapter 并探活（不落库、不影响现有实例）。
func (r *Impl) TestConnection(ctx context.Context, spec AgentSpec, sendMessage bool) (res TestResult) {
	start := r.opts.Now()
	defer func() {
		res.Duration = r.opts.Now().Sub(start)
		res.DurationMs = float64(res.Duration.Microseconds()) / 1000
	}()

	f, ok := r.factories[spec.Adapter]
	if !ok {
		res.Err = fmt.Sprintf("unknown adapter %q", spec.Adapter)
		return
	}
	ad, err := f.New(adapter.ResolveEnv(spec.Config))
	if err != nil {
		res.Err = err.Error()
		return
	}
	pctx, cancel := context.WithTimeout(ctx, r.opts.ProbeTimeout)
	defer cancel()
	if err := ad.HealthCheck(pctx); err != nil {
		res.Err = fmt.Sprintf("health check failed: %v", err)
		return
	}
	if sendMessage {
		sctx, scancel := context.WithTimeout(ctx, r.opts.ProbeTimeout*4)
		defer scancel()
		sid, err := ad.CreateSession(sctx, adapter.SessionOpts{})
		if err != nil {
			res.Err = fmt.Sprintf("create session failed: %v", err)
			return
		}
		if _, err := ad.Send(sctx, sid, adapter.Message{Content: "nexus connectivity test"}); err != nil {
			res.Err = fmt.Sprintf("test message failed: %v", err)
			return
		}
		_ = ad.Close(sctx, sid)
	}
	res.OK = true
	return
}

// Register 注册 agent：三途径统一入口（FR1.1）。
// 流程：规格校验 → factory.New（配置校验失败拒绝，400 字段错误）→ 探活定初态 → 落库 → 事件。
func (r *Impl) Register(ctx context.Context, spec AgentSpec) (Agent, error) {
	if spec.Name == "" {
		return Agent{}, fmt.Errorf("registry: agent name required")
	}
	f, ok := r.factories[spec.Adapter]
	if !ok {
		return Agent{}, fmt.Errorf("registry: unknown adapter %q", spec.Adapter)
	}
	if existing, err := r.store.Agents().GetByName(ctx, spec.Name); err == nil && existing.Status != repo.StatusRemoved {
		return Agent{}, fmt.Errorf("%w: %q（先注销或改名）", ErrDuplicateName, spec.Name)
	}

	// 密钥 ${ENV} 载入解析（F1.3 / U-F1-08）
	cfg := adapter.ResolveEnv(spec.Config)
	normalized, err := adapter.CoerceConfig(f.ConfigSchema(), cfg)
	if err != nil {
		return Agent{}, err
	}
	ad, err := f.New(normalized)
	if err != nil {
		return Agent{}, err
	}

	caps := ad.Info().Capabilities
	if spec.Capabilities != nil {
		// 显式声明 MaxConcurrency=0 视为非法（U-F1-07），不能被默认值静默掩盖
		if spec.Capabilities.MaxConcurrency <= 0 {
			return Agent{}, fmt.Errorf("%w: max_concurrency must be > 0", ErrBadCapabilities)
		}
		caps = spec.Capabilities.MergeDefaults(caps)
	}

	agentID := id.New()
	// 探活决定初始状态（探活通过自动 online，失败保存为 offline）
	status := repo.StatusOnline
	pctx, cancel := context.WithTimeout(ctx, r.opts.ProbeTimeout)
	probeErr := ad.HealthCheck(pctx)
	cancel()
	if probeErr != nil {
		status = repo.StatusOffline
	}
	now := r.opts.Now()
	rec := repo.Agent{
		ID: agentID, Name: spec.Name, Adapter: spec.Adapter,
		ConfigJSON:       mustJSON(normalized.ToJSON()),
		CapabilitiesJSON: mustJSON(adapter.CapabilitiesToJSON(caps)),
		Status:           status, LastHeartbeatAt: &now,
	}
	if err := r.store.Agents().Create(ctx, rec); err != nil {
		if errors.Is(err, repo.ErrDuplicateName) {
			return Agent{}, fmt.Errorf("%w: %q", ErrDuplicateName, spec.Name)
		}
		return Agent{}, err
	}

	rt := &agentRT{
		agent: Agent{
			ID: agentID, Name: spec.Name, Adapter: spec.Adapter,
			Config: normalized, Capabilities: caps, Status: status,
			LastHeartbeatAt: &now,
		},
		adapter:  ad,
		sem:      semaphore.NewWeighted(int64(caps.MaxConcurrency)),
		stopBeat: make(chan struct{}),
	}
	r.mu.Lock()
	r.agents[agentID] = rt
	r.mu.Unlock()

	// 状态事件（初始 offline 也发布，探活结果可见）
	r.publishStatus(agentID, spec.Name, "", status, probeReason(status, probeErr))
	r.startHeartbeat(rt)
	r.log.Info("agent registered",
		"agent_id", agentID, "name", spec.Name, "adapter", spec.Adapter,
		"status", status, "config", mustJSON(adapter.MaskConfig(normalized, f.ConfigSchema()).ToJSON()))
	return rt.snapshot(), nil
}

func probeReason(status string, err error) string {
	if err != nil {
		return fmt.Sprintf("initial probe failed: %v", err)
	}
	return "initial probe ok"
}

// LoadPersisted 启动时从库恢复 agent：重新实例化 adapter 并立即探活（kill -9 重启后状态重探）。
func (r *Impl) LoadPersisted(ctx context.Context) error {
	records, err := r.store.Agents().List(ctx, repo.AgentFilter{IncludeRemoved: true})
	if err != nil {
		return err
	}
	for _, rec := range records {
		if rec.Status == repo.StatusRemoved {
			continue
		}
		f, ok := r.factories[rec.Adapter]
		if !ok {
			r.log.Warn("persisted agent has unknown adapter, left offline", "agent", rec.Name, "adapter", rec.Adapter)
			continue
		}
		cfg, err := adapter.ConfigFromJSON(rec.ConfigJSON)
		if err != nil {
			r.log.Error("persisted agent config corrupted", "agent", rec.Name, "err", err)
			continue
		}
		ad, err := f.New(cfg)
		if err != nil {
			r.log.Error("persisted agent instantiate failed", "agent", rec.Name, "err", err)
			continue
		}
		caps, err := adapter.CapabilitiesFromJSON(rec.CapabilitiesJSON)
		if err != nil {
			r.log.Error("persisted agent capabilities corrupted", "agent", rec.Name, "err", err)
			continue
		}
		// 立即探活刷新状态（重启后不信任库内旧状态）；停用 agent 不探活，保持 offline
		status := repo.StatusOnline
		if !rec.Disabled {
			pctx, cancel := context.WithTimeout(ctx, r.opts.ProbeTimeout)
			probeErr := ad.HealthCheck(pctx)
			cancel()
			if probeErr != nil {
				status = repo.StatusOffline
			}
		} else {
			status = repo.StatusOffline
		}
		now := r.opts.Now()
		rec.Status = status
		rec.LastHeartbeatAt = &now
		if err := r.store.Agents().Update(ctx, rec); err != nil {
			return err
		}
		rt := &agentRT{
			agent: Agent{
				ID: rec.ID, Name: rec.Name, Adapter: rec.Adapter,
				Config: cfg, Capabilities: caps, Status: status, LastHeartbeatAt: &now,
			},
			adapter:  ad,
			disabled: rec.Disabled,
			sem:      semaphore.NewWeighted(int64(caps.MaxConcurrency)),
			stopBeat: make(chan struct{}),
		}
		r.mu.Lock()
		r.agents[rec.ID] = rt
		r.mu.Unlock()
		r.startHeartbeat(rt)
		r.log.Info("agent restored", "agent_id", rec.ID, "name", rec.Name, "status", status, "disabled", rec.Disabled)
	}
	return nil
}

func (r *Impl) Get(ctx context.Context, agentID string) (Agent, error) {
	r.mu.RLock()
	rt, ok := r.agents[agentID]
	r.mu.RUnlock()
	if !ok {
		return Agent{}, fmt.Errorf("%w: agent %s", repo.ErrNotFound, agentID)
	}
	return rt.snapshot(), nil
}

// GetByName 按名字查询运行时 agent。
func (r *Impl) GetByName(ctx context.Context, name string) (Agent, error) {
	rec, err := r.store.Agents().GetByName(ctx, name)
	if err != nil {
		return Agent{}, err
	}
	return r.Get(ctx, rec.ID)
}

// Adapter 返回 agent 运行时 adapter 实例（session/orchestr 层取用）。
func (r *Impl) Adapter(agentID string) (adapter.AgentAdapter, error) {
	r.mu.RLock()
	rt, ok := r.agents[agentID]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: agent %s", repo.ErrNotFound, agentID)
	}
	return rt.adapter, nil
}

func (r *Impl) List(ctx context.Context, f StatusFilter) ([]Agent, error) {
	// store 驱动（软删记录仍可查），叠加运行时态（busy 派生/停用/inFlight）。
	records, err := r.store.Agents().List(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]Agent, 0, len(records))
	for _, rec := range records {
		r.mu.RLock()
		rt, live := r.agents[rec.ID]
		r.mu.RUnlock()
		if !live {
			caps, err := adapter.CapabilitiesFromJSON(rec.CapabilitiesJSON)
			if err != nil {
				return nil, err
			}
			cfg, err := adapter.ConfigFromJSON(rec.ConfigJSON)
			if err != nil {
				return nil, err
			}
			out = append(out, Agent{
				ID: rec.ID, Name: rec.Name, Adapter: rec.Adapter,
				Config: cfg, Capabilities: caps, Status: rec.Status,
				LastHeartbeatAt: rec.LastHeartbeatAt,
				Disabled:        rec.Disabled,
			})
			continue
		}
		a := rt.snapshot()
		if f.Status != "" && a.Status != f.Status {
			continue
		}
		if f.Adapter != "" && a.Adapter != f.Adapter {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// Acquire 并发闸门（F1.5）：per-agent 信号量，容量 = MaxConcurrency。
// 返回 release；调用方必须最终调用 release（通常 defer）。
func (r *Impl) Acquire(ctx context.Context, agentID string) (func(), error) {
	r.mu.RLock()
	rt, ok := r.agents[agentID]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: agent %s", repo.ErrNotFound, agentID)
	}
	rt.mu.Lock()
	if rt.disabled {
		rt.mu.Unlock()
		return nil, ErrAgentDisabled
	}
	switch rt.agent.Status {
	case repo.StatusRemoved:
		rt.mu.Unlock()
		return nil, ErrAgentRemoved
	case repo.StatusOffline:
		rt.mu.Unlock()
		return nil, ErrAgentOffline
	}
	rt.mu.Unlock()

	// 满时在此阻塞排队（受 ctx 取消约束）
	if err := rt.sem.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("registry: acquire %s: %w", agentID, err)
	}
	rt.mu.Lock()
	rt.inFlight++
	rt.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			rt.sem.Release(1)
			rt.mu.Lock()
			rt.inFlight--
			rt.mu.Unlock()
		})
	}, nil
}

// Heartbeat 单次探活（心跳循环同逻辑；测试可直接调用）。
func (r *Impl) Heartbeat(ctx context.Context, agentID string) error {
	r.mu.RLock()
	rt, ok := r.agents[agentID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: agent %s", repo.ErrNotFound, agentID)
	}
	r.probe(ctx, rt)
	return nil
}

// probe 执行一次探活并推进状态机（F1.4：连续 N 败 → offline；恢复 → online）。
func (r *Impl) probe(ctx context.Context, rt *agentRT) {
	rt.mu.Lock()
	disabled := rt.disabled
	rt.mu.Unlock()
	if disabled {
		// 停用期间不探活：避免心跳把停用 agent 翻回 online（Acquire 仍被 disabled 闸拦，
		// 但状态抖动会误导 UI）；Enable 路径会主动 probe 恢复真实状态。
		return
	}

	pctx, cancel := context.WithTimeout(ctx, r.opts.ProbeTimeout)
	err := rt.adapter.HealthCheck(pctx)
	cancel()

	rt.mu.Lock()
	now := r.opts.Now()
	rt.agent.LastHeartbeatAt = &now
	from := rt.agent.Status
	_ = from
	if err == nil {
		rt.consecFails = 0
		if rt.agent.Status == repo.StatusOffline {
			rt.agent.Status = repo.StatusOnline
		}
	} else {
		rt.consecFails++
		if rt.consecFails >= r.opts.OfflineThreshold && rt.agent.Status == repo.StatusOnline {
			rt.agent.Status = repo.StatusOffline
		}
	}
	a := rt.agent
	rt.mu.Unlock()

	// 持久化心跳与状态
	rec, gerr := r.store.Agents().Get(context.Background(), a.ID)
	if gerr == nil {
		rec.Status = a.Status
		rec.LastHeartbeatAt = a.LastHeartbeatAt
		_ = r.store.Agents().Update(context.Background(), rec)
	}

	var reason string
	if err == nil {
		reason = "probe ok"
	} else {
		reason = fmt.Sprintf("probe failed (%d/%d): %v", rt.consecFailsSafe(), r.opts.OfflineThreshold, err)
	}
	if from != a.Status {
		r.publishStatus(a.ID, a.Name, from, a.Status, reason)
	}
}

func (rt *agentRT) consecFailsSafe() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.consecFails
}

// startHeartbeat 每 agent 独立 ticker（间隔可配，默认 15s，F1.3）。
func (r *Impl) startHeartbeat(rt *agentRT) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ticker := time.NewTicker(r.opts.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.probe(context.Background(), rt)
			case <-rt.stopBeat:
				return
			}
		}
	}()
}

// UpdateConfig 配置变更（FR1.1b）：变更前自动连通性测试，
// 失败则变更不生效回滚旧配置（C-F1-03）；force=true 强制保存。
func (r *Impl) UpdateConfig(ctx context.Context, agentID string, newCfg adapter.Config, force bool) (Agent, error) {
	r.mu.RLock()
	rt, ok := r.agents[agentID]
	f := r.factories[rt.agent.Adapter]
	r.mu.RUnlock()
	if !ok || f == nil {
		return Agent{}, fmt.Errorf("%w: agent %s", repo.ErrNotFound, agentID)
	}

	// 密钥留空 → 沿用旧值（C-F1-04）
	merged := adapter.MergeConfigSecrets(newCfg, rt.agent.Config, f.ConfigSchema())
	res := r.TestConnection(ctx, AgentSpec{Adapter: rt.agent.Adapter, Config: merged}, false)
	if !res.OK && !force {
		return rt.snapshot(), fmt.Errorf("registry: connectivity test failed, change rejected: %s", res.Err)
	}

	normalized, err := adapter.CoerceConfig(f.ConfigSchema(), adapter.ResolveEnv(merged))
	if err != nil {
		return rt.snapshot(), err
	}
	ad, err := f.New(normalized)
	if err != nil {
		return rt.snapshot(), err
	}

	// 重探活定状态（force 保存坏配置 → offline 待恢复）
	status := repo.StatusOnline
	if !res.OK {
		status = repo.StatusOffline
	}
	rt.mu.Lock()
	oldStatus := rt.agent.Status
	rt.agent.Config = normalized
	rt.agent.Status = status
	rt.adapter = ad
	now := r.opts.Now()
	rt.agent.LastHeartbeatAt = &now
	rt.consecFails = 0
	updated := rt.agent // 持锁内直接取值，避免 snapshot 二次加锁自死锁
	updated.Config = updated.Config.Clone()
	updated.InFlight = rt.inFlight
	updated.Disabled = rt.disabled
	if updated.Status == repo.StatusOnline && rt.inFlight >= updated.Capabilities.MaxConcurrency {
		updated.Status = repo.StatusBusy
	}
	rt.mu.Unlock()

	rec, gerr := r.store.Agents().Get(ctx, agentID)
	if gerr != nil {
		return updated, gerr
	}
	cfgJSON, _ := normalized.ToJSON()
	rec.ConfigJSON = cfgJSON
	rec.Status = status
	rec.LastHeartbeatAt = &now
	if err := r.store.Agents().Update(ctx, rec); err != nil {
		return updated, err
	}
	if oldStatus != status {
		r.publishStatus(agentID, rec.Name, oldStatus, status, "config updated (probe "+mapOK[res.OK]+")")
	}
	return updated, nil
}

var mapOK = map[bool]string{true: "ok", false: "failed"}

// UpdateCapabilities 能力覆盖更新（MaxConcurrency=0 拒绝；并发闸门容量随新值重建）。
func (r *Impl) UpdateCapabilities(ctx context.Context, agentID string, caps adapter.Capabilities) (Agent, error) {
	r.mu.RLock()
	rt, ok := r.agents[agentID]
	r.mu.RUnlock()
	if !ok {
		return Agent{}, fmt.Errorf("%w: agent %s", repo.ErrNotFound, agentID)
	}
	if caps.MaxConcurrency <= 0 {
		return Agent{}, fmt.Errorf("%w: max_concurrency must be > 0", ErrBadCapabilities)
	}
	rt.mu.Lock()
	inFlight := rt.inFlight
	if inFlight > caps.MaxConcurrency {
		rt.mu.Unlock()
		return Agent{}, fmt.Errorf("%w: in-flight (%d) exceeds new max_concurrency (%d)", ErrBadCapabilities, inFlight, caps.MaxConcurrency)
	}
	base := rt.agent.Capabilities
	merged := caps.MergeDefaults(base)
	rt.agent.Capabilities = merged
	rt.sem = semaphore.NewWeighted(int64(merged.MaxConcurrency)) // 重建闸门（当前占用 < 新容量）
	updated := rt.agent
	updated.Config = updated.Config.Clone()
	updated.InFlight = inFlight
	updated.Disabled = rt.disabled
	rt.mu.Unlock()

	rec, err := r.store.Agents().Get(ctx, agentID)
	if err != nil {
		return updated, err
	}
	rec.CapabilitiesJSON = mustJSON(adapter.CapabilitiesToJSON(merged))
	if err := r.store.Agents().Update(ctx, rec); err != nil {
		return updated, err
	}
	return updated, nil
}

// Disable 停用：心跳暂停、Acquire 拒绝（C-F1-05）。
func (r *Impl) Disable(agentID string) error {
	return r.setDisabled(agentID, true)
}

// Enable 启用：恢复并立即探活。
func (r *Impl) Enable(ctx context.Context, agentID string) error {
	if err := r.setDisabled(agentID, false); err != nil {
		return err
	}
	r.mu.RLock()
	rt := r.agents[agentID]
	r.mu.RUnlock()
	r.probe(ctx, rt)
	return nil
}

func (r *Impl) setDisabled(agentID string, disabled bool) error {
	r.mu.RLock()
	rt, ok := r.agents[agentID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: agent %s", repo.ErrNotFound, agentID)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.disabled == disabled {
		return nil
	}
	rt.disabled = disabled
	if disabled {
		rt.agent.Status = repo.StatusOffline
	} else {
		rt.agent.Status = repo.StatusOffline // enable 后由 probe 决定真实状态
	}
	verb := "enabled"
	reason := "manual enable"
	if disabled {
		verb = "disabled"
		reason = "manual disable"
	}
	r.log.Info("agent "+verb, "agent_id", agentID, "name", rt.agent.Name)
	// 停用状态持久化（TODO#1）：重启后 LoadPersisted 恢复，不再丢失
	if err := r.persistDisabled(context.Background(), agentID, disabled); err != nil {
		r.log.Error("persist disabled flag failed", "agent_id", agentID, "err", err)
	}
	r.publishStatus(agentID, rt.agent.Name, "", rt.agent.Status, reason)
	return nil
}

// persistDisabled 与 probe 的 Get+Update 同构：只改 disabled 列语义，避免整行覆盖写。
// disable 同时把库内 status 置 offline（停用期间不再探活，无人顺带持久化状态）；
// enable 不动 status，紧随其后的 Enable probe 会写入真实状态。
func (r *Impl) persistDisabled(ctx context.Context, agentID string, disabled bool) error {
	rec, err := r.store.Agents().Get(ctx, agentID)
	if err != nil {
		return err
	}
	rec.Disabled = disabled
	if disabled {
		rec.Status = repo.StatusOffline
	}
	return r.store.Agents().Update(ctx, rec)
}

// Unregister 软删（status=removed，FR1.6）：历史会话/任务保留可查。
func (r *Impl) Unregister(ctx context.Context, agentID string) error {
	r.mu.Lock()
	rt, ok := r.agents[agentID]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("%w: agent %s", repo.ErrNotFound, agentID)
	}
	delete(r.agents, agentID)
	r.mu.Unlock()
	close(rt.stopBeat)

	rt.mu.Lock()
	from := rt.agent.Status
	name := rt.agent.Name
	rt.mu.Unlock()

	rec, err := r.store.Agents().Get(ctx, agentID)
	if err != nil {
		return err
	}
	rec.Status = repo.StatusRemoved
	if err := r.store.Agents().Update(ctx, rec); err != nil {
		return err
	}
	r.publishStatus(agentID, name, from, repo.StatusRemoved, "unregistered (soft delete)")
	return nil
}

// Close 停止全部心跳循环。
func (r *Impl) Close() {
	r.stopOnce.Do(func() {
		r.mu.Lock()
		for _, rt := range r.agents {
			select {
			case <-rt.stopBeat:
			default:
				close(rt.stopBeat)
			}
		}
		r.mu.Unlock()
		r.wg.Wait()
	})
}

// AdapterInstance 测试辅助：取 agent 当前 adapter 实例（类型断言到具体 fake 以读记录）。
func (r *Impl) AdapterInstance(agentID string) (adapter.AgentAdapter, error) {
	return r.Adapter(agentID)
}

// SetOfflineThresholdForTest 运行时调整 offline 阈值（测试注入）。
func (r *Impl) SetOfflineThresholdForTest(n int) { r.opts.OfflineThreshold = n }

// publishStatus 发布 agent.status 事件（from/to/reason）。
func (r *Impl) publishStatus(agentID, name, from, to, reason string) {
	payload := fmt.Sprintf(`{"agent_id":%q,"name":%q,"from":%q,"to":%q,"reason":%q}`, agentID, name, from, to, reason)
	_ = r.bus.Publish(context.Background(), bus.Event{Type: bus.TypeAgentStatus, Payload: []byte(payload)})
}

// snapshot 线程安全快照；busy 派生：inFlight >= MaxConcurrency（F1.3，不可手工设置）。
func (rt *agentRT) snapshot() Agent {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	a := rt.agent
	a.Config = a.Config.Clone()
	a.InFlight = rt.inFlight
	a.Disabled = rt.disabled
	if a.Status == repo.StatusOnline && rt.inFlight >= a.Capabilities.MaxConcurrency {
		a.Status = repo.StatusBusy
	}
	return a
}

func mustJSON(s string, err error) string {
	if err != nil {
		return "{}"
	}
	return s
}
