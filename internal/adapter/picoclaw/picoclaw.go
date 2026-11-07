// Package picoclaw 实现 CLI 型 adapter 范式（detailed-design §4.3）：
// 会话映射（--session 续话/无会话机制则平台拼上下文）、子进程全生命周期经 execx 管控
// （Setpgid 进程组 + 三层超时 + kill(-pgid) 升级）、输出解析（--output json 优先，
// 解析失败重试一次后报错）、并发闸门由 registry MaxConcurrency（CLI 默认 1）承担。
package picoclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	execx "github.com/dddpeter/agent-nexus/internal/adapter/exec"
	"github.com/dddpeter/agent-nexus/internal/id"
)

// Factory picoclaw 适配器工厂。
type Factory struct{}

// NewFactory 构造工厂。
func NewFactory() *Factory { return &Factory{} }

func (f *Factory) Name() string { return "picoclaw" }

func (f *Factory) ConfigSchema() adapter.Schema {
	return adapter.Schema{Fields: []adapter.SchemaField{
		{Name: "command", Label: "命令行模板", Type: adapter.FieldList, Required: true,
			Help: `如 ["picoclaw","agent","-p","{{input}}","--session","{{session}}","--output","json"]；占位符 {{input}} {{session}} {{context}}；消息内容不经 shell，无命令注入面`},
		{Name: "probe_command", Label: "探活命令", Type: adapter.FieldList,
			Help: "如 [\"picoclaw\",\"--version\"]；留空则仅校验可执行文件存在"},
		{Name: "output", Label: "输出解析", Type: adapter.FieldString, Default: "json",
			Options: []string{"json", "text", "lastline"},
			Help:    "json=解析最后一行 {\"reply\",\"session\"}；text=全文即回复；lastline=取最后一行非空文本（真实 CLI 的 banner/日志噪音下取回复行）"},
		{Name: "output_prefix", Label: "回复行前缀", Type: adapter.FieldString,
			Help: "lastline 模式可选：回复行前缀（如 picoclaw 的 '鲎'）；命中后取该前缀至输出末尾（多行回复/代码块完整提取），未命中回退末行"},
		{Name: "timeout", Label: "调用超时", Type: adapter.FieldDuration, Default: "5m0s"},
		{Name: "cwd", Label: "工作目录", Type: adapter.FieldString},
		{Name: "env", Label: "附加环境变量", Type: adapter.FieldList, Help: "KEY=VAL 列表"},
	}}
}

func (f *Factory) New(cfg adapter.Config) (adapter.AgentAdapter, error) {
	normalized, err := adapter.CoerceConfig(f.ConfigSchema(), cfg)
	if err != nil {
		return nil, err
	}
	cmdList := adapter.GetList(normalized, "command")
	if len(cmdList) == 0 {
		return nil, adapter.NewFieldError("command", "required")
	}
	return &Adapter{
		agentID:      id.New(),
		cmdTmpl:      cmdList,
		probe:        adapter.GetList(normalized, "probe_command"),
		output:       adapter.GetStr(normalized, "output", "json"),
		outputPrefix: strings.TrimSpace(adapter.GetStr(normalized, "output_prefix", "")),
		timeout:      adapter.GetDuration(normalized, "timeout", 5*time.Minute),
		cwd:          adapter.GetStr(normalized, "cwd", ""),
		env:          adapter.GetList(normalized, "env"),
		sessions:     make(map[adapter.SessionID]string),
	}, nil
}

// Adapter CLI 型实现。
type Adapter struct {
	agentID      string
	cmdTmpl      []string
	probe        []string
	output       string
	outputPrefix string
	timeout      time.Duration
	cwd          string
	env          []string
	mu           sync.Mutex
	sessions     map[adapter.SessionID]string // 平台 sid -> agent 侧 session（external_ref）
}

var _ adapter.AgentAdapter = (*Adapter)(nil)

func (a *Adapter) Info() adapter.AgentInfo {
	caps := adapter.DefaultCapabilities()
	caps.CanGroupChat = false // CLI 型默认不进群（首版宁严勿松）
	caps.TypicalLatency = adapter.Duration(30 * time.Second)
	return adapter.AgentInfo{ID: a.agentID, Name: "picoclaw", Adapter: "picoclaw", Capabilities: caps}
}

// HealthCheck 探活命令（如 picoclaw --version，超时 5s，F1.3）；未配置时校验 bin 存在。
func (a *Adapter) HealthCheck(ctx context.Context) error {
	if len(a.probe) > 0 {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		res := execx.Run(pctx, execx.Options{Bin: a.probe[0], Args: a.probe[1:], Env: a.env, Dir: a.cwd})
		if res.Err != nil {
			return fmt.Errorf("picoclaw: probe command failed (exit=%d): %s", res.ExitCode, oneLine(res.Stderr+res.Stdout))
		}
		return nil
	}
	if _, err := execx.LookPath(a.cmdTmpl[0]); err != nil {
		return fmt.Errorf("picoclaw: binary %q not found: %w", a.cmdTmpl[0], err)
	}
	return nil
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return strings.ReplaceAll(s, "\n", " ")
}

// sessionCapable 模板是否声明 {{session}}（无则视为无会话机制 agent）。
func (a *Adapter) sessionCapable() bool {
	for _, part := range a.cmdTmpl {
		if strings.Contains(part, "{{session}}") {
			return true
		}
	}
	return false
}

// CreateSession 平台侧生成 sid；agent 侧 external_ref 首次回复回填。
// 无会话机制（模板无 {{session}}）时 external_ref 恒空，平台拼上下文。
func (a *Adapter) CreateSession(_ context.Context, _ adapter.SessionOpts) (adapter.SessionID, error) {
	sid := adapter.SessionID("picoclaw-sess-" + id.New())
	if a.sessionCapable() {
		a.mu.Lock()
		a.sessions[sid] = string(sid) // CLI 型：先以平台 sid 作为 agent 侧续话标识，Reply.SessionRef 校正
		a.mu.Unlock()
	}
	return sid, nil
}

type cliOut struct {
	Reply   string `json:"reply"`
	Session string `json:"session"`
	Done    *bool  `json:"done,omitempty"`
}

// Send 渲染模板 → execx 管控子进程 → 解析输出（失败重试一次）。
func (a *Adapter) Send(ctx context.Context, sid adapter.SessionID, msg adapter.Message) (adapter.Reply, error) {
	ext := a.externalOf(sid)
	if ext == "" && a.sessionCapable() {
		// 懒绑定：Router 首次发送传平台 sid（不经 CreateSession），将其作为 agent 侧
		// 续话标识（与 CreateSession 的初始映射一致）；跨会话隔离，重启 map 丢失后可恢复。
		ext = string(sid)
		a.mu.Lock()
		a.sessions[sid] = ext
		a.mu.Unlock()
	}

	var reply adapter.Reply
	var lastErr error
	// 输出解析失败重试一次后报 error（§4.3）
	for attempt := 0; attempt < 2; attempt++ {
		args := make([]string, 0, len(a.cmdTmpl)-1)
		for i, part := range a.cmdTmpl {
			if i == 0 {
				continue // bin
			}
			args = append(args, renderArg(part, msg, ext))
		}
		runCtx, cancel, _ := execx.DeriveDeadline(ctx, a.timeout, 0)
		res := execx.Run(runCtx, execx.Options{
			Bin:  a.cmdTmpl[0],
			Args: args,
			Env:  a.env,
			Dir:  a.cwd,
		})
		cancel()
		if res.Err != nil {
			return adapter.Reply{}, fmt.Errorf("picoclaw: run %s: %w%s", a.cmdTmpl[0],
				res.Err, killedSuffix(res))
		}
		reply, lastErr = a.parseOutput(res.Stdout, ext)
		if lastErr == nil {
			if reply.SessionRef != "" && reply.SessionRef != ext {
				a.mu.Lock()
				a.sessions[sid] = reply.SessionRef
				a.mu.Unlock()
			}
			return reply, nil
		}
	}
	return adapter.Reply{}, fmt.Errorf("picoclaw: output parse failed after retry: %w", lastErr)
}

func killedSuffix(res execx.Result) string {
	if res.Killed {
		return fmt.Sprintf(" [killed=%s exit=%d tail=%q]", res.KillReason, res.ExitCode, truncateStr(res.Tail, 120))
	}
	return ""
}

func (a *Adapter) parseOutput(stdout, ext string) (adapter.Reply, error) {
	out := strings.TrimSpace(stdout)
	if a.output == "text" {
		return adapter.Reply{Content: out, SessionRef: ext, Done: true}, nil
	}
	if a.output == "lastline" {
		// 真实 CLI 常在 stdout 打 banner/进度日志（如 picoclaw 鲎 banner + zerolog WRN），
		// 回复约定为最后一行非空文本；无输出视为失败。
		lines := strings.Split(out, "\n")
		// 前缀模式：多行回复（代码块等）从命中前缀的行起完整提取（C-F4-01 实测：
		// 代码类回复以 ``` 围栏结尾，朴素末行会取到空围栏标记）
		if a.outputPrefix != "" {
			for i := len(lines) - 1; i >= 0; i-- {
				if strings.HasPrefix(strings.TrimSpace(lines[i]), a.outputPrefix) {
					content := strings.TrimPrefix(strings.TrimSpace(lines[i]), a.outputPrefix)
					if i+1 < len(lines) {
						content += "\n" + strings.Join(lines[i+1:], "\n")
					}
					content = strings.TrimSpace(content)
					if content == "" {
						break // 前缀行之后无内容 → 回退末行逻辑
					}
					return adapter.Reply{Content: content, SessionRef: ext, Done: true}, nil
				}
			}
		}
		last := ""
		for _, line := range lines {
			if t := strings.TrimSpace(line); t != "" {
				last = t
			}
		}
		if last == "" {
			return adapter.Reply{}, fmt.Errorf("empty stdout")
		}
		return adapter.Reply{Content: last, SessionRef: ext, Done: true}, nil
	}
	// json：取最后一个非空行
	lines := strings.Split(out, "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			last = strings.TrimSpace(lines[i])
			break
		}
	}
	if last == "" {
		return adapter.Reply{}, fmt.Errorf("empty stdout")
	}
	var parsed cliOut
	if err := json.Unmarshal([]byte(last), &parsed); err != nil {
		return adapter.Reply{}, fmt.Errorf("malformed json %q: %w", truncateStr(last, 120), err)
	}
	done := true
	if parsed.Done != nil {
		done = *parsed.Done
	}
	sessionRef := parsed.Session
	if sessionRef == "" {
		sessionRef = ext
	}
	return adapter.Reply{Content: parsed.Reply, SessionRef: sessionRef, Done: done}, nil
}

// Stream CLI 型首版不支持流式（CanStream=false）。
func (a *Adapter) Stream(_ context.Context, _ adapter.SessionID, _ adapter.Message) (<-chan adapter.StreamEvent, func(), error) {
	return nil, nil, fmt.Errorf("picoclaw: CLI adapter does not support streaming")
}

func (a *Adapter) Close(_ context.Context, sid adapter.SessionID) error {
	a.mu.Lock()
	delete(a.sessions, sid)
	a.mu.Unlock()
	return nil
}

func (a *Adapter) externalOf(sid adapter.SessionID) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[sid]
}

// ExternalOf 测试辅助：读取 sid 的 agent 侧会话标识。
func (a *Adapter) ExternalOf(sid adapter.SessionID) string { return a.externalOf(sid) }

func renderArg(part string, msg adapter.Message, ext string) string {
	r := strings.ReplaceAll(part, "{{input}}", msg.Content)
	r = strings.ReplaceAll(r, "{{session}}", ext)
	r = strings.ReplaceAll(r, "{{context}}", msg.ContextDigest)
	return r
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
