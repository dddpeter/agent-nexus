// Package hermes 实现 HTTP 型 adapter 范式（detailed-design §4.2）：
// CreateSession=POST 建 session；Send=POST 消息等结果；Stream=订阅流式接口转译统一事件；
// 健康=GET /health。default/coder/research 三 profile 各注册为一个逻辑 agent。
//
// 测试面向 echodhttp fake gateway（不打真实服务）。
package hermes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
			Help: "hermes gateway base URL，如 http://localhost:8600"},
		{Name: "profile", Label: "Profile", Type: adapter.FieldString, Default: "default",
			Options: []string{"default", "coder", "research"}},
		{Name: "api_key", Label: "API Key", Type: adapter.FieldString, Secret: true,
			Help: "Bearer 密钥（支持 ${ENV_VAR} 引用）"},
		{Name: "timeout", Label: "请求超时", Type: adapter.FieldDuration, Default: "2m0s"},
	}}
}

func (f *Factory) New(cfg adapter.Config) (adapter.AgentAdapter, error) {
	normalized, err := adapter.CoerceConfig(f.ConfigSchema(), cfg)
	if err != nil {
		return nil, err
	}
	return &Adapter{
		agentID:  id.New(),
		profile:  adapter.GetStr(normalized, "profile", "default"),
		baseURL:  strings.TrimRight(adapter.GetStr(normalized, "base_url", ""), "/"),
		apiKey:   adapter.GetStr(normalized, "api_key", ""),
		timeout:  adapter.GetDuration(normalized, "timeout", 2*time.Minute),
		client:   &http.Client{},
		sessions: make(map[adapter.SessionID]string),
	}, nil
}

// Adapter hermes gateway 实现。
type Adapter struct {
	agentID  string
	profile  string
	baseURL  string
	apiKey   string
	timeout  time.Duration
	client   *http.Client
	mu       sync.Mutex
	sessions map[adapter.SessionID]string // 平台 sid -> gateway session_id
}

var _ adapter.AgentAdapter = (*Adapter)(nil)

func (a *Adapter) Info() adapter.AgentInfo {
	caps := adapter.DefaultCapabilities()
	caps.CanGroupChat = true
	caps.CanStream = true
	caps.TypicalLatency = adapter.Duration(10 * time.Second)
	return adapter.AgentInfo{
		ID: a.agentID, Name: "hermes-" + a.profile, Adapter: "hermes",
		Capabilities: caps,
	}
}

// HealthCheck GET /health（间隔 15s 由 registry 心跳器驱动）。
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

// CreateSession POST gateway 建 session，保存 external_ref（§4.2）。
func (a *Adapter) CreateSession(ctx context.Context, _ adapter.SessionOpts) (adapter.SessionID, error) {
	resp, err := a.do(ctx, http.MethodPost, "/v1/sessions", map[string]any{})
	if err != nil {
		return "", fmt.Errorf("hermes: create session: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("hermes: unauthorized (check api_key)")
	}
	if resp.StatusCode != http.StatusCreated {
		return "", a.apiError(resp, "create session")
	}
	var out struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("hermes: decode session resp: %w", err)
	}
	sid := adapter.SessionID("hermes-sess-" + id.New())
	a.mu.Lock()
	a.sessions[sid] = out.SessionID
	a.mu.Unlock()
	return sid, nil
}

func (a *Adapter) externalOf(sid adapter.SessionID) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ext, ok := a.sessions[sid]
	return ext, ok
}

type sendReq struct {
	Content       string `json:"content"`
	ContextDigest string `json:"context_digest,omitempty"`
}

type sendResp struct {
	Reply   string `json:"reply"`
	Session string `json:"session"`
	Done    bool   `json:"done"`
}

// Send POST 消息 → 等待结果 → Reply（§4.2：轮询/长连接等结果；fake gateway 为同步响应）。
func (a *Adapter) Send(ctx context.Context, sid adapter.SessionID, msg adapter.Message) (adapter.Reply, error) {
	ext, ok := a.externalOf(sid)
	if !ok {
		return adapter.Reply{}, fmt.Errorf("hermes: unknown session %s", sid)
	}
	body := sendReq{Content: msg.Content, ContextDigest: msg.ContextDigest}
	resp, err := a.do(ctx, http.MethodPost, "/v1/sessions/"+ext+"/messages", body)
	if err != nil {
		return adapter.Reply{}, fmt.Errorf("hermes: send: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusGone:
		// agent 侧会话丢失（F2.4 翻译源）
		a.mu.Lock()
		delete(a.sessions, sid)
		a.mu.Unlock()
		return adapter.Reply{}, fmt.Errorf("%w: gateway session %s expired", adapter.ErrSessionLost, ext)
	case resp.StatusCode == http.StatusUnauthorized:
		return adapter.Reply{}, fmt.Errorf("hermes: unauthorized (check api_key)")
	case resp.StatusCode >= 500:
		raw, _ := io.ReadAll(resp.Body)
		return adapter.Reply{}, fmt.Errorf("hermes: gateway %d: %s", resp.StatusCode, truncateStr(string(raw), 200))
	case resp.StatusCode != http.StatusOK:
		return adapter.Reply{}, a.apiError(resp, "send")
	}
	var out sendResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return adapter.Reply{}, fmt.Errorf("hermes: decode reply: %w", err)
	}
	return adapter.Reply{Content: out.Reply, SessionRef: out.Session, Done: out.Done}, nil
}

// Stream 订阅 gateway 流式接口，转译为统一 Event（delta / final / error）。
func (a *Adapter) Stream(ctx context.Context, sid adapter.SessionID, msg adapter.Message) (<-chan adapter.StreamEvent, func(), error) {
	ext, ok := a.externalOf(sid)
	if !ok {
		return nil, nil, fmt.Errorf("hermes: unknown session %s", sid)
	}
	body := sendReq{Content: msg.Content, ContextDigest: msg.ContextDigest}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/sessions/"+ext+"/stream", bodyReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("hermes: stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusGone {
			return nil, nil, fmt.Errorf("%w: gateway session %s expired", adapter.ErrSessionLost, ext)
		}
		return nil, nil, a.apiError(resp, "stream")
	}

	ch := make(chan adapter.StreamEvent, 64)
	stop := make(chan struct{})
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(stop)
			resp.Body.Close()
		})
	}
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			select {
			case <-stop:
				return
			default:
			}
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var ev struct {
				Type string `json:"type"`
				Data string `json:"data"`
			}
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				select {
				case ch <- adapter.StreamEvent{Type: adapter.StreamError, Data: "malformed stream line: " + truncateStr(line, 120), ErrType: "agent_error"}:
				case <-stop:
				}
				continue
			}
			se := adapter.StreamEvent{Data: ev.Data}
			switch ev.Type {
			case "delta":
				se.Type = adapter.StreamDelta
			case "final":
				se.Type = adapter.StreamFinal
			default:
				se.Type = adapter.StreamError
				se.ErrType = "agent_error"
			}
			select {
			case ch <- se:
			case <-stop:
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case ch <- adapter.StreamEvent{Type: adapter.StreamError, Data: err.Error(), ErrType: "canceled"}:
			case <-stop:
			}
		}
	}()
	return ch, cancel, nil
}

func (a *Adapter) Close(_ context.Context, sid adapter.SessionID) error {
	a.mu.Lock()
	delete(a.sessions, sid)
	a.mu.Unlock()
	return nil
}

func (a *Adapter) apiError(resp *http.Response, what string) error {
	raw, _ := io.ReadAll(resp.Body)
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	if e.Error != "" {
		return fmt.Errorf("hermes: %s: %d: %s", what, resp.StatusCode, e.Error)
	}
	return fmt.Errorf("hermes: %s: %d: %s", what, resp.StatusCode, truncateStr(string(raw), 200))
}

func bodyReader(body any) io.Reader {
	raw, _ := json.Marshal(body)
	return bytes.NewReader(raw)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
