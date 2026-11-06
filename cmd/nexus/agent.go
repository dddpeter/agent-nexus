package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// client 与 nexus serve 通信的最小 HTTP 客户端（FR5.4 CLI）。
type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(server, token string) *client {
	if server == "" {
		server = os.Getenv("NEXUS_URL")
	}
	if server == "" {
		server = "http://127.0.0.1:8080"
	}
	if token == "" {
		token = os.Getenv("NEXUS_TOKEN")
	}
	return &client{base: strings.TrimRight(server, "/"), token: token, http: &http.Client{}}
}

func (c *client) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("connect %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var errEnvelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &errEnvelope) == nil && errEnvelope.Error.Message != "" {
			return fmt.Errorf("HTTP %d [%s]: %s", resp.StatusCode, errEnvelope.Error.Code, errEnvelope.Error.Message)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "agent 管理（list / register）",
	}
	cmd.AddCommand(newAgentListCmd(), newAgentRegisterCmd())
	return cmd
}

func newAgentListCmd() *cobra.Command {
	var server, token string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出已注册 agent 与健康状态",
		RunE: func(_ *cobra.Command, _ []string) error {
			c := newClient(server, token)
			var agents []map[string]any
			if err := c.do(http.MethodGet, "/api/agents", nil, &agents); err != nil {
				return err
			}
			if len(agents) == 0 {
				fmt.Println("(no agents registered)")
				return nil
			}
			fmt.Printf("%-16s %-10s %-10s %-6s %s\n", "ID", "NAME", "ADAPTER", "STATUS", "HEARTBEAT")
			for _, a := range agents {
				id, _ := a["id"].(string)
				name, _ := a["name"].(string)
				ad, _ := a["adapter"].(string)
				st, _ := a["status"].(string)
				hb, _ := a["last_heartbeat_at"].(string)
				fmt.Printf("%-16s %-10s %-10s %-6s %s\n", truncate(id, 15), name, ad, st, hb)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "nexus 服务地址（默认 $NEXUS_URL 或 http://127.0.0.1:8080）")
	cmd.Flags().StringVar(&token, "token", "", "Bearer token（默认 $NEXUS_TOKEN）")
	return cmd
}

type registerReq struct {
	Name         string           `json:"name"`
	Adapter      string           `json:"adapter"`
	Config       map[string]any   `json:"config"`
	Capabilities *json.RawMessage `json:"capabilities,omitempty"`
}

func newAgentRegisterCmd() *cobra.Command {
	var server, token, name, adapterName, cfgJSON, capsJSON string
	cmd := &cobra.Command{
		Use:   "register",
		Short: "注册 agent（经 REST API，与页面/配置文件三途径收敛）",
		RunE: func(_ *cobra.Command, _ []string) error {
			if name == "" || adapterName == "" {
				return fmt.Errorf("--name 与 --adapter 必填")
			}
			cfg := map[string]any{}
			if cfgJSON != "" {
				if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
					return fmt.Errorf("--config 非法 JSON: %w", err)
				}
			}
			req := registerReq{Name: name, Adapter: adapterName, Config: cfg}
			if capsJSON != "" {
				raw := json.RawMessage(capsJSON)
				req.Capabilities = &raw
			}
			c := newClient(server, token)
			var agent map[string]any
			if err := c.do(http.MethodPost, "/api/agents", req, &agent); err != nil {
				return err
			}
			fmt.Printf("registered: id=%s name=%s status=%v\n", agent["id"], agent["name"], agent["status"])
			return nil
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "nexus 服务地址")
	cmd.Flags().StringVar(&token, "token", "", "Bearer token")
	cmd.Flags().StringVar(&name, "name", "", "agent 名字（必填）")
	cmd.Flags().StringVar(&adapterName, "adapter", "", "adapter 类型（必填，如 echod/hermes/picoclaw）")
	cmd.Flags().StringVar(&cfgJSON, "config", "{}", "adapter 配置 JSON，如 '{\"mode\":\"echo\"}'")
	cmd.Flags().StringVar(&capsJSON, "capabilities", "", "能力覆盖 JSON，如 '{\"max_concurrency\":2}'")
	return cmd
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
