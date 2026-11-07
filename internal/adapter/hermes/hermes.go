// Package hermes 实现 HTTP 型 adapter 范式（detailed-design §4.2），
// 线协议对齐真实 hermes gateway（2026-11-07 对 v0.22.7 实测校准）：
//
//	GET  /health                        探活（公开，无需密钥）
//	POST /api/sessions                  建会话 → {"session":{"id":...}}
//	POST /api/sessions/{id}/chat        {"message":...} → {"message":{"content":...}}
//	POST /v1/chat/completions           OpenAI 兼容 SSE 流式；X-Hermes-Session-Id 头绑定会话续话
//
// 会话映射无状态：adapter sid 即 gateway session id；首次发送收到平台 sid（网关 404）
// 时懒建网关会话并经 Reply.SessionRef 回填 external_ref（F2.3/F2.4 自愈语义）。
package hermes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dddpeter/agent-nexus/internal/adapter"
	"github.com/dddpeter/agent-nexus/internal/id"
)

// Factory hermes 适配器工厂。
type Factory struct{}

// NewFactory 构造工厂。
func NewFactory() *Factory { return &Factory{} }

func (f *Factory) Name() string { return "hermes" }

func (f *Factory) ConfigSchema() adapter.Schema {
	return adapter.Schema{Fields: []adapter.SchemaField{
		{Name: "base_url", Label: "Gateway 地址", Type: adapter.FieldString, Required: true,
			Help: "hermes gateway base URL，如 http://localhost:8642"},
		{Name: "profile", Label: "Profile", Type: adapter.FieldString, Default: "default",
			Options: []string{"default", "coder", "research"},
			Help:    "仅用作逻辑 agent 命名（hermes-<profile>），不改变线协议"},
		{Name: "api_key", Label: "API Key", Type: adapter.FieldString, Secret: true,
			Help: "Bearer 密钥（网关 API_SERVER_KEY，支持 ${ENV_VAR} 引用）；/health 无需密钥"},
		{Name: "model", Label: "模型名", Type: adapter.FieldString, Default: "hermes-agent",
			Help: "/v1/chat/completions 的 model 字段"},
		{Name: "timeout", Label: "请求超时", Type: adapter.FieldDuration, Default: "5m0s",
			Help: "单次请求超时；真实 LLM 回合常见 10-90s，默认 5m"},
	}}
}

func (f *Factory) New(cfg adapter.Config) (adapter.AgentAdapter, error) {
	normalized, err := adapter.CoerceConfig(f.ConfigSchema(), cfg)
	if err != nil {
		return nil, err
	}
	return &Adapter{
		agentID: id.New(),
		profile: adapter.GetStr(normalized, "profile", "default"),
		baseURL: strings.TrimRight(adapter.GetStr(normalized, "base_url", ""), "/"),
		apiKey:  adapter.GetStr(normalized, "api_key", ""),
		model:   adapter.GetStr(normalized, "model", "hermes-agent"),
		timeout: adapter.GetDuration(normalized, "timeout", 5*time.Minute),
		// 直连：不继承系统代理（纳管对象通常是本机/内网服务，走代理会导致探活结果失真）
		client: &http.Client{Transport: &http.Transport{Proxy: nil}},
	}, nil
}

// Adapter hermes gateway 实现。
type Adapter struct {
	agentID string
	profile string
	baseURL string
	apiKey  string
	model   string
	timeout time.Duration
	client  *http.Client
	mu      sync.Mutex
	gwSess  map[string]bool // 已知 gateway session id（懒建判定优化，非正确性依赖）
}

var _ adapter.AgentAdapter = (*Adapter)(nil)

func (a *Adapter) Info() adapter.AgentInfo {
	caps := adapter.DefaultCapabilities()
	caps.CanGroupChat = true
	caps.CanStream = true
	caps.TypicalLatency = adapter.Duration(30 * time.Second)
	return adapter.AgentInfo{
		ID: a.agentID, Name: "hermes-" + a.profile, Adapter: "hermes",
		Capabilities: caps,
	}
}

// HealthCheck GET /health（无需密钥；间隔 15s 由 registry 心跳器驱动）。
func (a *Adapter) HealthCheck(ctx context.Context) error {
	resp, err := a.do(ctx, http.MethodGet, "/health", nil)
	if err != nil {
		return fmt.Errorf("hermes: gateway unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		return fmt.Errorf("hermes: gateway /health returned 503 (profile=%s)", a.profile)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("hermes: gateway /health status %d", resp.StatusCode)
	}
	return nil
}

func (a *Adapter) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	reqCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, a.baseURL+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	return a.client.Do(req)
}

// CreateSession POST /api/sessions 建网关会话，gateway session id 即 adapter sid。
func (a *Adapter) CreateSession(ctx context.Context, _ adapter.SessionOpts) (adapter.SessionID, error) {
	resp, err := a.do(ctx, http.MethodPost, "/api/sessions", map[string]any{})
	if err != nil {
		return "", fmt.Errorf("hermes: create session: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("hermes: unauthorized (check api_key)")
	}
	if resp.StatusCode >= 400 {
		return "", a.apiError(resp, "create session")
	}
	var out struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("hermes: decode session resp: %w", err)
	}
	if out.Session.ID == "" {
		return "", fmt.Errorf("hermes: create session: empty session.id")
	}
	a.remember(out.Session.ID)
	return adapter.SessionID(out.Session.ID), nil
}

func (a *Adapter) remember(sid string) {
	a.mu.Lock()
	if a.gwSess == nil {
		a.gwSess = make(map[string]bool)
	}
	a.gwSess[sid] = true
	a.mu.Unlock()
}

func (a *Adapter) known(sid string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gwSess[sid]
}

// chatReq 真实网关 chat 门请求体（POST /api/sessions/{id}/chat）。
type chatReq struct {
	Message string `json:"message"`
}

// chatResp 真实网关 chat 门响应（status=completed；message.content 即回复）。
type chatResp struct {
	Status    string `json:"status"`
	SessionID string `json:"session_id"`
	Message   struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
}

// Send POST /api/sessions/{sid}/chat；sid 非网关会话（首条消息传平台 sid）或网关丢会话
// （404 session_not_found）时懒建新会话重试一次，SessionRef 回填触发 external_ref 落库。
func (a *Adapter) Send(ctx context.Context, sid adapter.SessionID, msg adapter.Message) (adapter.Reply, error) {
	reply, err := a.chatOnce(ctx, string(sid), msg.Content)
	if err == nil {
		return reply, nil
	}
	if !isSessionNotFound(err) {
		return adapter.Reply{}, err
	}
	// 懒建：平台 sid / 网关重启丢会话 → 新建网关会话重发（F2.4 自愈）
	fresh, cerr := a.CreateSession(ctx, adapter.SessionOpts{})
	if cerr != nil {
		return adapter.Reply{}, fmt.Errorf("hermes: lazy session create: %w", cerr)
	}
	reply, err = a.chatOnce(ctx, string(fresh), msg.Content)
	if err != nil {
		return adapter.Reply{}, err
	}
	return reply, nil
}

func (a *Adapter) chatOnce(ctx context.Context, gwID, content string) (adapter.Reply, error) {
	resp, err := a.do(ctx, http.MethodPost, "/api/sessions/"+gwID+"/chat", chatReq{Message: content})
	if err != nil {
		return adapter.Reply{}, fmt.Errorf("hermes: send: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return adapter.Reply{}, fmt.Errorf("hermes: unauthorized (check api_key)")
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return adapter.Reply{}, sessionLostErr(gwID, resp)
	case resp.StatusCode >= 400:
		return adapter.Reply{}, a.apiError(resp, "send")
	}
	var out chatResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return adapter.Reply{}, fmt.Errorf("hermes: decode reply: %w", err)
	}
	a.remember(gwID)
	return adapter.Reply{Content: out.Message.Content, SessionRef: gwID, Done: true}, nil
}

// isSessionNotFound 判断错误是否为「网关侧无此会话」（懒建重试依据）。
func isSessionNotFound(err error) bool {
	var sle *sessionLostError
	return errors.As(err, &sle)
}

// sessionLostError 网关侧会话不存在（404 session_not_found / 410 Gone）。
type sessionLostError struct {
	gwID string
	code int
}

func (e *sessionLostError) Error() string {
	return fmt.Sprintf("hermes: gateway session %s not found (http %d)", e.gwID, e.code)
}

func sessionLostErr(gwID string, resp *http.Response) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return &sessionLostError{gwID: gwID, code: resp.StatusCode}
}

// Stream POST /v1/chat/completions（OpenAI 兼容 SSE）+ X-Hermes-Session-Id 续话。
// delta.content 渐进转译为 StreamDelta；[DONE]/finish_reason → StreamFinal（累计全量）。
func (a *Adapter) Stream(ctx context.Context, sid adapter.SessionID, msg adapter.Message) (<-chan adapter.StreamEvent, func(), error) {
	gwID := string(sid)
	if !a.known(gwID) {
		// 平台 sid / 重启后未知 sid：先懒建，避免流式路径拿不到可续话会话
		fresh, err := a.CreateSession(ctx, adapter.SessionOpts{})
		if err != nil {
			return nil, nil, fmt.Errorf("hermes: lazy session create: %w", err)
		}
		gwID = string(fresh)
	}

	payload := map[string]any{
		"model":    a.model,
		"messages": []map[string]string{{"role": "user", "content": msg.Content}},
		"stream":   true,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	sctx, scancel := context.WithTimeout(ctx, a.timeout)
	req, err := http.NewRequestWithContext(sctx, http.MethodPost, a.baseURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		scancel()
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Hermes-Session-Id", gwID)
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		scancel()
		return nil, nil, fmt.Errorf("hermes: stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			scancel()
			return nil, nil, fmt.Errorf("%w: gateway session %s expired", adapter.ErrSessionLost, gwID)
		}
		serr := a.apiError(resp, "stream")
		scancel()
		return nil, nil, serr
	}

	ch := make(chan adapter.StreamEvent, 64)
	stop := make(chan struct{})
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(stop)
			scancel()
			resp.Body.Close()
		})
	}
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		var assembled strings.Builder
		finalSent := false
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			select {
			case <-stop:
				return
			default:
			}
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, ":") {
				continue // SSE 注释/心跳
			}
			data, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			data = strings.TrimSpace(data)
			if data == "[DONE]" {
				sendStreamEvent(ch, stop, adapter.StreamEvent{Type: adapter.StreamFinal, Data: assembled.String()})
				finalSent = true
				return
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				// 非 SSE 帧（网关关流直返 JSON 等）：尽力降级为 final
				var fallback chatResp
				if json.Unmarshal([]byte(data), &fallback) == nil && fallback.Message.Content != "" {
					sendStreamEvent(ch, stop, adapter.StreamEvent{Type: adapter.StreamFinal, Data: fallback.Message.Content})
					finalSent = true
					return
				}
				continue
			}
			for _, c := range chunk.Choices {
				if c.Delta.Content == "" {
					continue
				}
				assembled.WriteString(c.Delta.Content)
				sendStreamEvent(ch, stop, adapter.StreamEvent{Type: adapter.StreamDelta, Data: c.Delta.Content})
			}
			if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil && !finalSent {
				// finish_reason=stop 后仍等 [DONE]；若网关直接关流，由 scanner 结束兜底发 final
				fr := *chunk.Choices[0].FinishReason
				if fr == "stop" || fr == "length" {
					sendStreamEvent(ch, stop, adapter.StreamEvent{Type: adapter.StreamFinal, Data: assembled.String()})
					finalSent = true
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case ch <- adapter.StreamEvent{Type: adapter.StreamError, Data: err.Error(), ErrType: "canceled"}:
			case <-stop:
			}
			return
		}
		if !finalSent {
			// 网关关流未发 [DONE]：以累计内容收尾（空内容则报错）
			if assembled.Len() > 0 {
				sendStreamEvent(ch, stop, adapter.StreamEvent{Type: adapter.StreamFinal, Data: assembled.String()})
			} else {
				sendStreamEvent(ch, stop, adapter.StreamEvent{Type: adapter.StreamError, Data: "stream ended without content", ErrType: "agent_error"})
			}
		}
	}()
	return ch, cancel, nil
}

func sendStreamEvent(ch chan adapter.StreamEvent, stop <-chan struct{}, ev adapter.StreamEvent) {
	select {
	case ch <- ev:
	case <-stop:
	}
}

func (a *Adapter) Close(_ context.Context, _ adapter.SessionID) error { return nil }

func (a *Adapter) apiError(resp *http.Response, what string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	if e.Error.Message != "" {
		return fmt.Errorf("hermes: %s: %d: %s", what, resp.StatusCode, e.Error.Message)
	}
	return fmt.Errorf("hermes: %s: %d: %s", what, resp.StatusCode, truncateStr(string(raw), 200))
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
