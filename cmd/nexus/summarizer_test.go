package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

// mockSummaryAgent 可控的摘要 agent（记录收到的输入；可注入失败）。
type mockSummaryAgent struct {
	mu     sync.Mutex
	fail   bool
	lastIn string
}

func (m *mockSummaryAgent) Info() adapter.AgentInfo {
	return adapter.AgentInfo{ID: "sum", Name: "sum", Adapter: "mock", Capabilities: adapter.DefaultCapabilities()}
}
func (m *mockSummaryAgent) HealthCheck(ctx context.Context) error { return nil }
func (m *mockSummaryAgent) CreateSession(ctx context.Context, o adapter.SessionOpts) (adapter.SessionID, error) {
	return "sum-sess", nil
}
func (m *mockSummaryAgent) Close(ctx context.Context, sid adapter.SessionID) error { return nil }
func (m *mockSummaryAgent) Stream(ctx context.Context, sid adapter.SessionID, msg adapter.Message) (<-chan adapter.StreamEvent, func(), error) {
	return nil, nil, fmt.Errorf("no stream")
}
func (m *mockSummaryAgent) Send(ctx context.Context, sid adapter.SessionID, msg adapter.Message) (adapter.Reply, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastIn = msg.Content
	if m.fail {
		return adapter.Reply{}, fmt.Errorf("mock summary failure")
	}
	return adapter.Reply{Content: "摘要要点：讨论了集成测试", Done: true}, nil
}

type mockSummaryFactory struct{ ad *mockSummaryAgent }

func (f *mockSummaryFactory) Name() string                 { return "mocksum" }
func (f *mockSummaryFactory) ConfigSchema() adapter.Schema { return adapter.Schema{} }
func (f *mockSummaryFactory) New(cfg adapter.Config) (adapter.AgentAdapter, error) {
	return f.ad, nil
}

type summaryRegistryStub struct {
	agents map[string]adapter.AgentAdapter
}

func (s *summaryRegistryStub) Acquire(ctx context.Context, agentID string) (func(), error) {
	return func() {}, nil
}
func (s *summaryRegistryStub) Adapter(agentID string) (adapter.AgentAdapter, error) {
	ad, ok := s.agents[agentID]
	if !ok {
		return nil, fmt.Errorf("no agent %s", agentID)
	}
	return ad, nil
}

// TODO#5 闭环验证：agent 生成摘要 → 内容寻址缓存命中 → 失败降级截断。
func TestAgentSummarizer(t *testing.T) {
	store := repo.NewMemoryStore()
	agent := &mockSummaryAgent{}
	require_NoError(t, store.Agents().Create(context.Background(), repo.Agent{
		ID: "sum-agent", Name: "sum", Adapter: "mocksum", ConfigJSON: "{}", CapabilitiesJSON: "{}", Status: repo.StatusOnline,
	}))
	reg := &summaryRegistryStub{agents: map[string]adapter.AgentAdapter{"sum-agent": agent}}
	sum := agentSummarizer(store, reg, quietTestLogger())

	rounds := []string{"user: 问题一", "agent: 回答一", "user: 问题二"}

	// 1) 生成：走 agent，输出带 SummaryMark
	out1 := sum(rounds)
	if !strings.HasPrefix(out1, private_SummaryMark) || !strings.Contains(out1, "集成测试") {
		t.Fatalf("摘要应来自 agent 且带标记，实际 %q", out1)
	}
	if !strings.Contains(agent.lastIn, "问题一") {
		t.Fatalf("摘要输入应包含历史轮次")
	}

	// 2) 缓存命中：同轮次不重复调用（lastIn 不变 = 未重新 Send）
	before := agent.lastIn
	out2 := sum(rounds)
	if out2 != out1 {
		t.Fatal("同轮次应命中缓存")
	}
	agent.mu.Lock()
	sameInput := agent.lastIn == before
	agent.mu.Unlock()
	if !sameInput {
		t.Fatal("缓存命中不应重新调用 agent")
	}

	// 3) 新轮次 → 重新生成
	rounds2 := append(append([]string{}, rounds...), "user: 问题三")
	out3 := sum(rounds2)
	if !strings.Contains(out3, "集成测试") {
		t.Fatalf("新轮次应重新生成摘要")
	}

	// 4) 失败 → 降级截断
	agent.mu.Lock()
	agent.fail = true
	agent.mu.Unlock()
	rounds3 := append(append([]string{}, rounds...), "user: 问题四")
	out4 := sum(rounds3)
	if !strings.HasPrefix(out4, private_SummaryMark) || !strings.Contains(out4, "截断模式") {
		t.Fatalf("agent 失败应降级截断，实际 %q", out4[:min(80, len(out4))])
	}

	// 5) 全部 agent 不可用 → 降级
	emptyReg := &summaryRegistryStub{agents: map[string]adapter.AgentAdapter{}}
	sum2 := agentSummarizer(store, emptyReg, quietTestLogger())
	out5 := sum2(rounds)
	if !strings.Contains(out5, "截断模式") {
		t.Fatal("无可用 agent 应降级截断")
	}
}

func quietTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&discardWriter{}, nil))
}

type discardWriter struct{}

func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

const private_SummaryMark = "[summary]"

func require_NoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
