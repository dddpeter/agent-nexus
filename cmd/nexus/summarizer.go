// summarizer.go 私聊摘要 agent 化（TODO#5 闭环，F2.3）：
// 用任意可用 agent 生成上下文摘要（独立 agent 侧会话，不污染用户会话）；
// 内容寻址缓存——同轮次历史不重复触发 LLM；失败/超时降级截断（设计明示路径）。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/repo"
	"github.com/dddpeter/agent-nexus/internal/session/private"
)

const (
	summarizerTimeout    = 30 * time.Second
	summarizerInputLimit = 8 * 1024 // 喂给摘要 agent 的历史上限
	summarizerCacheMax   = 128
)

// agentSummarizer 生成带缓存的 agent 摘要器（private.Options.Summarizer 注入用）。
func agentSummarizer(store repo.Store, reg summaryRegistry, log *slog.Logger) func([]string) string {
	var mu sync.Mutex
	cache := map[string]string{}
	truncate := private.TruncateSummarizer // 降级路径（F2.3：失败则截断）

	return func(rounds []string) string {
		key := summarizerKey(rounds)
		mu.Lock()
		if c, ok := cache[key]; ok {
			mu.Unlock()
			return c
		}
		mu.Unlock()

		summary, err := generateSummary(context.Background(), store, reg, rounds)
		if err != nil {
			log.Warn("agent 摘要生成失败，降级截断", "err", err)
			return truncate(rounds)
		}
		mu.Lock()
		if len(cache) >= summarizerCacheMax {
			cache = map[string]string{} // 摘要可再生成，直接清空
		}
		cache[key] = summary
		mu.Unlock()
		return summary
	}
}

// generateSummary 选第一个可用 agent 生成摘要。
func generateSummary(ctx context.Context, store repo.Store, reg summaryRegistry, rounds []string) (string, error) {
	agents, err := store.Agents().List(ctx, repo.AgentFilter{Status: repo.StatusOnline})
	if err != nil || len(agents) == 0 {
		return "", fmt.Errorf("no online agent for summary: %w", err)
	}
	var lastErr error
	for _, a := range agents {
		if a.Disabled {
			continue
		}
		release, err := reg.Acquire(ctx, a.ID)
		if err != nil {
			lastErr = err
			continue
		}
		ad, err := reg.Adapter(a.ID)
		if err != nil {
			release()
			lastErr = err
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, summarizerTimeout)
		history := truncateRounds(rounds, summarizerInputLimit)
		reply, err := ad.Send(sctx, adapter.SessionID("summary-"+a.ID), adapter.Message{
			Content: "请用不超过 200 字摘要以下对话的要点与结论，直接输出摘要正文：\n\n" + history,
		})
		cancel()
		release()
		if err == nil && strings.TrimSpace(reply.Content) != "" {
			return private.SummaryMark + " " + strings.TrimSpace(reply.Content), nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no candidate agent produced a summary")
	}
	return "", lastErr
}

func summarizerKey(rounds []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "n=%d;", len(rounds))
	if len(rounds) > 0 {
		h.Write([]byte(rounds[0]))
		fmt.Fprintf(h, "|%d:", len(rounds[len(rounds)-1]))
		h.Write([]byte(rounds[len(rounds)-1]))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func truncateRounds(rounds []string, limit int) string {
	var sb strings.Builder
	total := 0
	for i := len(rounds) - 1; i >= 0; i-- {
		r := rounds[i]
		if total+len(r) > limit {
			break
		}
		sb.WriteString(r)
		sb.WriteString("\n")
		total += len(r) + 1
	}
	return sb.String()
}

// summaryRegistry 摘要器所需注册层能力（registry.Impl 满足）。
type summaryRegistry interface {
	Acquire(ctx context.Context, agentID string) (release func(), err error)
	Adapter(agentID string) (adapter.AgentAdapter, error)
}
