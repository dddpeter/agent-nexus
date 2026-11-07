package picoclaw_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	execx "github.com/dddpeter/agent-nexus/internal/adapter/exec"
	"github.com/dddpeter/agent-nexus/internal/adapter/picoclaw"
)

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

func echodBin(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "picoclaw-echod-")
		if err != nil {
			binErr = err
			return
		}
		bin := dir + "/echod"
		if out, err := exec.Command("go", "build", "-o", bin, "github.com/dddpeter/agent-nexus/cmd/echod").CombinedOutput(); err != nil {
			binErr = fmt.Errorf("go build echod: %v: %s", err, out)
			return
		}
		binPath = bin
	})
	require.NoError(t, binErr)
	return binPath
}

// echodCmd 构造 echod 命令模板（CLI 型范式：--session 续话 + --output json）。
// args[0] 由 newAdapter 替换为真实二进制路径。
func echodCmd(mode string) []string {
	return []string{"REPLACE_BIN", "--mode", mode, "-p", "{{input}}", "--session", "{{session}}", "--output", "json"}
}

func newAdapter(t *testing.T, cmdArgs []string, extra map[string]any) *picoclaw.Adapter {
	t.Helper()
	bin := echodBin(t)
	args := append([]string(nil), cmdArgs...)
	args[0] = bin
	cfg := map[string]any{"command": args}
	for k, v := range extra {
		cfg[k] = v
	}
	ad, err := picoclaw.NewFactory().New(cfg)
	require.NoError(t, err)
	return ad.(*picoclaw.Adapter)
}

// 回显 + 续话：第二次 Send 携带相同 external_ref。
func TestSendEchoAndSessionContinuity(t *testing.T) {
	ctx := context.Background()
	ad := newAdapter(t, echodCmd("echo"), map[string]any{"output": "json"})

	require.NoError(t, ad.HealthCheck(ctx))
	sid, err := ad.CreateSession(ctx, adapter.SessionOpts{})
	require.NoError(t, err)

	reply, err := ad.Send(ctx, sid, adapter.Message{Content: "hello picoclaw"})
	require.NoError(t, err)
	assert.Equal(t, "hello picoclaw", reply.Content)
	assert.Equal(t, string(sid), reply.SessionRef, "echod 透传 --session")

	// 续话映射：external_ref 持久于会话
	reply2, err := ad.Send(ctx, sid, adapter.Message{Content: "turn two"})
	require.NoError(t, err)
	assert.Equal(t, "turn two", reply2.Content)
	assert.Equal(t, ad.ExternalOf(sid), reply2.SessionRef)
}

// 输出解析失败重试一次后报错（--mode badjson）。
func TestParseFailRetry(t *testing.T) {
	ctx := context.Background()
	ad := newAdapter(t, echodCmd("badjson"), map[string]any{"output": "json"}) // 默认 lastline 下坏 JSON 也能取末行，需显式 json 验证解析重试
	sid, err := ad.CreateSession(ctx, adapter.SessionOpts{})
	require.NoError(t, err)
	_, err = ad.Send(ctx, sid, adapter.Message{Content: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse failed after retry", "重试一次后应报解析错误")
}

// 挂起子进程 → 调用超时 → 进程组 kill，错误含 kill 信息（U-F7-05 联动：adapter 发事件的数据源）。
func TestHangTimeoutKillsChild(t *testing.T) {
	ctx := context.Background()
	ad := newAdapter(t, echodCmd("hang"), map[string]any{"timeout": "400ms"})
	sid, err := ad.CreateSession(ctx, adapter.SessionOpts{})
	require.NoError(t, err)

	start := time.Now()
	_, err = ad.Send(ctx, sid, adapter.Message{Content: "picoclaw-hang-probe"})
	require.Error(t, err)
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 8*time.Second, "不应无限等待（含宽限期，允许负载抖动）")
	assert.Contains(t, err.Error(), "killed", "错误应含 kill 信息（供 error 事件记录）")

	time.Sleep(150 * time.Millisecond)
	out, err := exec.Command("sh", "-c", `ps -eo args | grep "picoclaw-hang-probe" | grep -v grep || true`).Output()
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(string(out)), "子进程应无残留（F7 联动）")
}

// 非零退出：echod --mode down → Send 报错；探活失败（probe_command）。
func TestNonZeroExitAndProbe(t *testing.T) {
	ctx := context.Background()
	ad := newAdapter(t, echodCmd("down"), nil)
	sid, _ := ad.CreateSession(ctx, adapter.SessionOpts{})
	_, err := ad.Send(ctx, sid, adapter.Message{Content: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit")

	// probe_command 探活：down 失败
	bin := echodBin(t)
	ad2 := newAdapter(t, echodCmd("echo"), map[string]any{
		"probe_command": []string{bin, "--mode", "down", "--probe"},
	})
	err = ad2.HealthCheck(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "probe")

	ad3 := newAdapter(t, echodCmd("echo"), map[string]any{
		"probe_command": []string{bin, "--mode", "echo", "--probe"},
	})
	require.NoError(t, ad3.HealthCheck(ctx))

	// 未配置 probe：LookPath 兜底（bin 存在即健康）
	require.NoError(t, ad.HealthCheck(ctx))
}

// text 输出模式。
func TestTextOutputMode(t *testing.T) {
	ctx := context.Background()
	bin := echodBin(t)
	cfg := map[string]any{
		"command": []string{bin, "--mode", "fixed", "--reply", "纯文本回复", "-p", "{{input}}", "--output", "text"},
		"output":  "text",
	}
	ad2, err := picoclaw.NewFactory().New(cfg)
	require.NoError(t, err)
	sid, _ := ad2.CreateSession(ctx, adapter.SessionOpts{})
	reply, err := ad2.Send(ctx, sid, adapter.Message{Content: "x"})
	require.NoError(t, err)
	assert.Equal(t, "纯文本回复", reply.Content)
}

// 无会话机制（模板无 {{session}}）：external_ref 恒空，{{context}} 注入。
func TestStatelessTemplate(t *testing.T) {
	ctx := context.Background()
	bin := echodBin(t)
	cfg := map[string]any{
		"command": []string{bin, "--mode", "fixed", "--reply", "ctx={{context}}", "-p", "{{input}}", "--output", "text"},
		"output":  "text",
	}
	ad, err := picoclaw.NewFactory().New(cfg)
	require.NoError(t, err)
	sid, _ := ad.CreateSession(ctx, adapter.SessionOpts{})
	assert.Empty(t, ad.(*picoclaw.Adapter).ExternalOf(sid), "无会话机制 external_ref 恒空")

	reply, err := ad.Send(ctx, sid, adapter.Message{Content: "x", ContextDigest: "摘要内容"})
	require.NoError(t, err)
	assert.Equal(t, "ctx=摘要内容", reply.Content, "{{context}} 占位符应注入")
}

// 配置校验：缺 command → 字段错误；空列表拒绝。
func TestConfigValidation(t *testing.T) {
	// 空配置 → 开箱即用（schema 默认模板，降低注册难度）
	ad, err := picoclaw.NewFactory().New(map[string]any{})
	require.NoError(t, err)
	cmd := ad.Info()
	assert.NotEmpty(t, cmd.Name)

	// 显式传空 list → 明确拒绝
	_, err = picoclaw.NewFactory().New(map[string]any{"command": []any{}})
	require.Error(t, err)
	var fe *adapter.FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, "command", fe.Field)
}

// Stream 不支持。
func TestStreamUnsupported(t *testing.T) {
	ad := newAdapter(t, echodCmd("echo"), nil)
	_, _, err := ad.Stream(context.Background(), "s", adapter.Message{Content: "x"})
	require.Error(t, err)
	info := ad.Info()
	assert.False(t, info.Capabilities.CanStream)
}

// execx 引用占位（防止未使用 import 时编译失败由 build 保证；此用例实际验证超时派生）。
func TestDeriveTimeoutUsed(t *testing.T) {
	_, cancel, warn := execx.DeriveDeadline(context.Background(), time.Second, 0)
	defer cancel()
	assert.False(t, warn)
}
