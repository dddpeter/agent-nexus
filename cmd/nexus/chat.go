package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// chatCmd `nexus chat <agent>`（FR5.4 / U-F5-05）：交互式 REPL 私聊。
// 每行一条消息走同步发送（mode=sync），/exit 退出，/new 建新会话，/session 查看当前会话 id。
func newChatCmd() *cobra.Command {
	var server, token string
	cmd := &cobra.Command{
		Use:   "chat <agent>",
		Short: "与指定 agent 交互式私聊（REPL）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runChat(newClient(server, token), args[0])
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "nexus 服务地址（默认 $NEXUS_URL 或 http://127.0.0.1:8080）")
	cmd.Flags().StringVar(&token, "token", "", "Bearer token（默认 $NEXUS_TOKEN）")
	return cmd
}

type chatAgent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func runChat(c *client, agentRef string) error {
	// 解析 agent：支持 id 或名字
	var agents []chatAgent
	if err := c.do("GET", "/api/agents", nil, &agents); err != nil {
		return err
	}
	var target *chatAgent
	for i := range agents {
		if agents[i].ID == agentRef || agents[i].Name == agentRef {
			target = &agents[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("agent %q 不存在（nexus agent list 查看）", agentRef)
	}

	// 建会话
	var sess struct {
		ID string `json:"id"`
	}
	if err := c.do("POST", "/api/sessions", map[string]any{"agent_id": target.ID}, &sess); err != nil {
		return err
	}

	fmt.Printf("nexus chat → %s（会话 %s）；/new 新会话，/exit 退出\n", target.Name, sess.ID)
	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("你> ")
		if !sc.Scan() {
			return nil
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		switch line {
		case "/exit", "/quit":
			fmt.Println("再见。")
			return nil
		case "/new":
			if err := c.do("POST", "/api/sessions", map[string]any{"agent_id": target.ID}, &sess); err != nil {
				fmt.Println("建新会话失败：", err)
				continue
			}
			fmt.Printf("新会话 %s\n", sess.ID)
			continue
		case "/session":
			fmt.Println(sess.ID)
			continue
		}

		var res struct {
			Reply *struct {
				Content string `json:"content"`
			} `json:"reply"`
			TimedOut bool `json:"timed_out"`
			Async    *struct {
				Type string `json:"type"`
			} `json:"async"`
		}
		if err := c.do("POST", "/api/sessions/"+sess.ID+"/messages", map[string]any{"content": line, "mode": "sync"}, &res); err != nil {
			fmt.Println("发送失败：", err)
			continue
		}
		if res.Reply != nil {
			fmt.Printf("%s> %s\n", target.Name, res.Reply.Content)
		} else if res.TimedOut {
			fmt.Println("（回复超时，已留占位消息，可重发）")
		} else if res.Async != nil {
			fmt.Printf("（已受理：%s；回执经 SSE 异步回填）\n", res.Async.Type)
		}
	}
}

// groupSendCmd `nexus group send <group> <content...>`（FR5.4）：群发言一次性命令。
func newGroupSendCmd() *cobra.Command {
	var server, token, sender string
	cmd := &cobra.Command{
		Use:   "group",
		Short: "群聊操作（send）",
	}
	send := &cobra.Command{
		Use:   "send <group-id-or-name> <content>",
		Short: "向群发送一条人发言（经仲裁路由）",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGroupSend(newClient(server, token), args[0], args[1], sender)
		},
	}
	send.Flags().StringVar(&sender, "from", "user", "发送者标识")
	cmd.AddCommand(send)
	cmd.PersistentFlags().StringVar(&server, "server", "", "nexus 服务地址（默认 $NEXUS_URL 或 http://127.0.0.1:8080）")
	cmd.PersistentFlags().StringVar(&token, "token", "", "Bearer token（默认 $NEXUS_TOKEN）")
	return cmd
}

func runGroupSend(c *client, groupRef, content, sender string) error {
	var groups []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := c.do("GET", "/api/groups", nil, &groups); err != nil {
		return err
	}
	var gid string
	for _, g := range groups {
		if g.ID == groupRef || g.Name == groupRef {
			gid = g.ID
			break
		}
	}
	if gid == "" {
		return fmt.Errorf("群 %q 不存在", groupRef)
	}
	var res struct {
		Reply *struct {
			SenderID string `json:"sender_id"`
			Content  string `json:"content"`
		} `json:"reply"`
		Deferred []struct {
			AgentID string `json:"agent_id"`
			Reason  string `json:"reason"`
		} `json:"deferred"`
		Frozen bool `json:"frozen"`
	}
	if err := c.do("POST", "/api/groups/"+gid+"/messages", map[string]any{"content": content, "sender_id": sender}, &res); err != nil {
		return err
	}
	if res.Reply != nil {
		fmt.Printf("%s> %s\n", res.Reply.SenderID, res.Reply.Content)
	}
	for _, d := range res.Deferred {
		fmt.Printf("[deferred:%s] %s\n", d.Reason, d.AgentID)
	}
	if res.Frozen {
		fmt.Println("（群已冻结）")
	}
	return nil
}

// taskCmd `nexus task`（FR5.4 收尾）：submit / status / cancel / cron。
func newTaskCmd() *cobra.Command {
	var server, token, input string
	cmd := &cobra.Command{Use: "task", Short: "任务编排操作（submit/status/cancel/cron）"}
	submit := &cobra.Command{
		Use:   "submit <agent> [agent...]",
		Short: "提交协作链（每个 agent 一个节点，{{prev}} 自动串接）",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTaskSubmit(newClient(server, token), args, input)
		},
	}
	submit.Flags().StringVar(&input, "input", "", "任务初始输入（首节点 {{prev}} 同源）")
	status := &cobra.Command{
		Use:   "status <task-id>",
		Short: "查询任务状态与节点进度",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTaskStatus(newClient(server, token), args[0])
		},
	}
	cancel := &cobra.Command{
		Use:   "cancel <task-id>",
		Short: "取消运行中任务",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient(server, token)
			return c.do("POST", "/api/tasks/"+args[0]+"/cancel", map[string]any{}, nil)
		},
	}
	cmd.AddCommand(submit, status, cancel)
	cmd.PersistentFlags().StringVar(&server, "server", "", "nexus 服务地址（默认 $NEXUS_URL 或 http://127.0.0.1:8080）")
	cmd.PersistentFlags().StringVar(&token, "token", "", "Bearer token（默认 $NEXUS_TOKEN）")
	return cmd
}

func runTaskSubmit(c *client, agentRefs []string, input string) error {
	var agents []chatAgent
	if err := c.do("GET", "/api/agents", nil, &agents); err != nil {
		return err
	}
	nodes := make([]map[string]any, 0, len(agentRefs))
	for i, ref := range agentRefs {
		id := ref
		if !strings.Contains(ref, "-") || len(ref) < 16 { // 名字解析（nanoid 含 - 且较长）
			for _, a := range agents {
				if a.Name == ref {
					id = a.ID
					break
				}
			}
		}
		tpl := "{{prev}}"
		if i == 0 {
			tpl = input
			if tpl == "" {
				tpl = "{{prev}}"
			}
		}
		nodes = append(nodes, map[string]any{"agent_id": id, "input": tpl})
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := c.do("POST", "/api/tasks", map[string]any{"chain": map[string]any{"nodes": nodes, "input": input}}, &res); err != nil {
		return err
	}
	fmt.Printf("submitted: %s\n", res.ID)
	return nil
}

func runTaskStatus(c *client, taskID string) error {
	var t struct {
		ID     string                   `json:"id"`
		Status string                   `json:"status"`
		Result string                   `json:"result"`
		Error  string                   `json:"error"`
		Nodes  []map[string]interface{} `json:"nodes"`
	}
	if err := c.do("GET", "/api/tasks/"+taskID, nil, &t); err != nil {
		return err
	}
	fmt.Printf("%s  %s\n", t.ID, t.Status)
	for _, n := range t.Nodes {
		fmt.Printf("  #%v  %-8v %v  retried=%v\n", n["id"], n["status"], n["agent_id"], n["retried"])
	}
	if t.Error != "" {
		fmt.Println("  error:", t.Error)
	}
	if t.Result != "" {
		fmt.Printf("  result: %.200s\n", t.Result)
	}
	return nil
}
