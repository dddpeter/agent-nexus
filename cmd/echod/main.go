// echod 是内置测试用假 agent 的 CLI 形态（test-plan §1）：
// 可脚本化——固定回复 / 回显 / 延迟 N 秒 / 输出畸形 JSON / 中途退出 / 挂起 / 伪流式输出。
//
// 供 picoclaw 范式（CLI 型 adapter）与 F7 进程治理测试用作被管子进程。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/dddpeter/agent-nexus/internal/adapter/echo/echodhttp"
	"github.com/dddpeter/agent-nexus/internal/id"
)

type cliOpts struct {
	mode     string
	reply    string
	delayMs  int
	chunks   int
	prompt   string
	session  string
	output   string
	exitCode int
	probe    bool
	serve    string
	streamMs int
}

func main() {
	var o cliOpts
	flag.StringVar(&o.mode, "mode", "echo", "行为模式: echo|fixed|delay|badjson|hang|stream|down|exit")
	flag.StringVar(&o.reply, "reply", "", "fixed/stream 模式回复内容")
	flag.IntVar(&o.delayMs, "delay-ms", 0, "delay/stream 模式延迟毫秒")
	flag.IntVar(&o.chunks, "chunks", 3, "stream 模式 delta 分片数")
	flag.StringVar(&o.prompt, "p", "", "消息内容（echo 回显它）")
	flag.StringVar(&o.prompt, "prompt", "", "消息内容（同 -p）")
	flag.StringVar(&o.session, "session", "", "外部会话 ID（续话模拟）")
	flag.StringVar(&o.output, "output", "json", "输出格式: json|text")
	flag.IntVar(&o.exitCode, "exit-code", 1, "exit 模式退出码")
	flag.BoolVar(&o.probe, "probe", false, "探活模式：mode=down 时失败退出，否则立即成功")
	flag.StringVar(&o.serve, "serve", "", "HTTP 形态监听地址（如 :8642），空 = CLI 单次模式")
	flag.IntVar(&o.streamMs, "stream-interval-ms", 30, "stream 模式 delta 间隔毫秒")
	flag.Parse()

	if o.serve != "" {
		runHTTP(o)
		return
	}

	code := runCLI(o)
	os.Exit(code)
}

func runCLI(o cliOpts) int {
	// 探活命令约定：--probe 快速返回
	if o.probe {
		if o.mode == "down" {
			fmt.Fprintln(os.Stderr, "echod: probe failed (mode=down)")
			return 1
		}
		fmt.Println(`{"ok":true}`)
		return 0
	}

	session := o.session
	if session == "" {
		session = "echod-sess-" + id.New()
	}

	switch o.mode {
	case "down":
		fmt.Fprintln(os.Stderr, "echod: agent is down")
		return 1
	case "exit":
		fmt.Println(`{"reply":"partial work before crash"}`)
		return o.exitCode
	case "hang":
		for { // 永不退出（挂起模式，由平台 kill 进程组）；不用 select{} 避免触发 runtime 死锁检测
			time.Sleep(time.Hour)
		}
	case "delay":
		time.Sleep(time.Duration(o.delayMs) * time.Millisecond)
		emit(o, session, o.prompt, true)
		return 0
	case "badjson":
		// 畸形 JSON：截断输出
		fmt.Print(`{"reply":"this json is deliberately truncated`)
		return 0
	case "stream":
		content := o.reply
		if content == "" {
			content = o.prompt
		}
		runes := []rune(content)
		chunks := o.chunks
		if chunks < 2 {
			chunks = 2
		}
		per := (len(runes) + chunks - 1) / chunks
		if per == 0 {
			per = 1
		}
		for i := 0; i < len(runes); i += per {
			end := i + per
			if end > len(runes) {
				end = len(runes)
			}
			emitLine(streamLine{Type: "delta", Data: string(runes[i:end])})
			time.Sleep(time.Duration(o.streamMs) * time.Millisecond)
		}
		emitLine(streamLine{Type: "final", Data: content, Session: session})
		return 0
	default: // echo / fixed
		content := o.prompt
		if o.mode == "fixed" {
			content = o.reply
		}
		emit(o, session, content, true)
		return 0
	}
}

type replyOut struct {
	Reply   string `json:"reply"`
	Session string `json:"session"`
	Done    bool   `json:"done"`
}

type streamLine struct {
	Type    string `json:"type"`
	Data    string `json:"data"`
	Session string `json:"session,omitempty"`
}

func emit(o cliOpts, session, content string, done bool) {
	if o.output == "text" {
		fmt.Println(content)
		return
	}
	b, _ := json.Marshal(replyOut{Reply: content, Session: session, Done: done})
	fmt.Println(string(b))
}

func emitLine(line streamLine) {
	b, _ := json.Marshal(line)
	fmt.Println(string(b))
}

// runHTTP HTTP 形态：复用 echodhttp fake gateway handler。
func runHTTP(o cliOpts) {
	addr := o.serve
	fmt.Printf("echod http listening on %s\n", addr)
	mux := echodhttp.NewMux(echodhttp.Config{
		Mode: o.mode, Reply: o.reply, DelayMs: o.delayMs, Chunks: o.chunks,
	})
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "echod: http serve error:", err)
		os.Exit(1)
	}
}
