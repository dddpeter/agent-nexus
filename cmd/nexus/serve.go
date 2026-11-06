package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/adapter/echo"
	execx "github.com/dddpeter/agent-nexus/internal/adapter/exec"
	"github.com/dddpeter/agent-nexus/internal/adapter/hermes"
	"github.com/dddpeter/agent-nexus/internal/adapter/picoclaw"
	"github.com/dddpeter/agent-nexus/internal/api"
	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/internal/config"
	"github.com/dddpeter/agent-nexus/internal/orchestr"
	"github.com/dddpeter/agent-nexus/internal/registry"
	"github.com/dddpeter/agent-nexus/internal/repo"
	"github.com/dddpeter/agent-nexus/internal/session/private"
)

// schedulerRunner 将 orchestr.Scheduler 适配为 session.AsyncRunner（serve 装配层）。
type schedulerRunner struct{ s orchestr.Scheduler }

func (a schedulerRunner) Submit(ctx context.Context, job func(context.Context)) (string, error) {
	return a.s.Submit(ctx, orchestr.Job{Fn: job})
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "nexus",
		Short: "Nexus — 异构 Agent 纳管与编排平台",
	}
	root.AddCommand(newServeCmd(), newAgentCmd())
	return root
}

func newServeCmd() *cobra.Command {
	var configPath string
	var listen string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "启动 nexus 服务（API + Web UI + 心跳 + 事件总线）",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			if listen != "" {
				cfg.Server.Listen = listen
			}
			return runServe(cmd.Context(), cfg)
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "config.yaml", "配置文件路径（不存在则用默认值）")
	cmd.Flags().StringVar(&listen, "listen", "", "覆盖监听地址（默认 :8080）")
	return cmd
}

func runServe(ctx context.Context, cfg config.Config) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if cfg.Server.AuthToken == "" {
		logger.Warn("auth_token 未配置：API 将无鉴权开放（仅限本地开发）")
	}

	// F7：进程治理全局配置 + 启动收尸 + 三层超时告警检查（U-F7-04）
	if err := execx.Configure(cfg.Registry.MaxChildren, cfg.Registry.KillGrace.D(), cfg.Registry.PgidFile); err != nil {
		logger.Warn("pgid tracker 初始化失败（收尸扫描禁用）", "err", err)
	}
	if cleaned := execx.CleanupStale(); len(cleaned) > 0 {
		logger.Warn("清理上次运行残留子进程组", "pgids", cleaned)
	}
	if _, _, warn := execx.DeriveDeadline(ctx, 0, cfg.Registry.DefaultChildTimeout.D()); warn {
		logger.Warn("default_child_timeout=0 且无节点级超时：子进程可能无限运行（建议配置兜底超时）")
	}

	if cfg.Orchestration.Engine == "temporal" {
		// 铁律：本夜禁止接入 Temporal，降级 builtin 并告警（TODO 记录）
		logger.Warn("orchestration.engine=temporal 未实现，已降级 builtin（见 TODO.md）")
		cfg.Orchestration.Engine = "builtin"
	}

	// 存储层（含 goose 迁移）
	store, err := repo.OpenSQLite(ctx, cfg.Store.DSN)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// 事件总线
	b, err := bus.New(store.Events(), bus.Options{})
	if err != nil {
		return fmt.Errorf("init bus: %w", err)
	}
	defer b.Close()

	// 注册层：已安装 adapter 工厂集合（echod + 两范式实现）
	factories := []adapter.AdapterFactory{
		echo.NewFactory(),
		hermes.NewFactory(),
		picoclaw.NewFactory(),
	}
	reg, err := registry.New(store, b, factories, registry.Options{
		HeartbeatInterval: cfg.Registry.HeartbeatInterval.D(),
		OfflineThreshold:  cfg.Registry.OfflineThreshold,
		Logger:            logger,
	})
	if err != nil {
		return fmt.Errorf("init registry: %w", err)
	}
	defer reg.Close()

	// 重启恢复：持久化 agent 重新实例化并立即探活（DoD：kill -9 后状态重探）
	if err := reg.LoadPersisted(ctx); err != nil {
		logger.Error("恢复持久化 agent 失败", "err", err)
	}

	// 配置文件静态注册（FR1.1：三途径收敛，同名视为更新）
	for _, spec := range cfg.Agents {
		if err := upsertAgent(ctx, reg, spec); err != nil {
			logger.Error("静态 agent 注册失败", "name", spec.Name, "err", err)
		}
	}

	// 私聊路由 + 内置降级调度器（async 模式回执执行体；Temporal 二期接入）
	scheduler := orchestr.NewBuiltin(store, b, logger)
	router := private.New(store, reg, b, private.Options{
		SyncTimeout:       300 * time.Second,
		StreamIdleTimeout: 60 * time.Second,
		Logger:            logger,
	})
	router.SetAsyncRunner(schedulerRunner{scheduler})

	srv := api.New(api.Deps{
		Cfg: cfg, Store: store, Bus: b, Registry: reg, Router: router, Log: logger,
	})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		cancel()
	}()
	return srv.Run(ctx)
}

func upsertAgent(ctx context.Context, reg *registry.Impl, spec config.AgentStatic) error {
	sp := registry.AgentSpec{
		Name: spec.Name, Adapter: spec.Adapter,
		Config: adapter.Config(spec.Config), Capabilities: spec.Capabilities,
	}
	if existing, err := reg.GetByName(ctx, spec.Name); err == nil {
		_, err = reg.UpdateConfig(ctx, existing.ID, sp.Config, false)
		return err
	}
	_, err := reg.Register(ctx, sp)
	return err
}
