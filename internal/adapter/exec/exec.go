// Package exec 实现 CLI 型 agent 子进程全生命周期管控（detailed-design F7.2）。
//
// 规范：
//   - spawn：Setpgid 独立进程组；stdin/stdout 管道；env 显式白名单继承
//   - 超时三层：调用 ctx（请求级）→ 节点/任务 timeout → default_child_timeout 平台兜底
//   - 取消语义：ctx.Done → kill(-pgid, SIGTERM) → 5s 宽限 → SIGKILL；退出码与输出尾部随 Result 返回
//   - 资源上限：平台总子进程上限（默认 32），满则排队
//   - 收尸：cmd.Wait() 与 Start 成对且恰一次；启动时按记录 pgid 清理上次残留
//
// 仅支持 POSIX（Setpgid/kill 进程组）。
package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/semaphore"
)

// Options 单次子进程调用参数。
type Options struct {
	Bin  string
	Args []string
	// Env 附加注入的环境变量（KEY=VAL）；白名单父变量自动继承。
	Env []string
	// Dir 工作目录（空 = 当前目录）。
	Dir string
	// Stdin 写入子进程 stdin 的内容（空 = /dev/null）。
	Stdin string
	// MaxOutput stdout/stderr 各自捕获上限（字节），默认 1MB。
	MaxOutput int
}

// Result 子进程执行结果。被 kill 时 Err 非 nil，且 Killed=true、KillReason、Tail、ExitCode 齐备
// （U-F7-05：kill 后输出尾部与退出码记录在事件中——由 adapter 层将本结果发布为 error 事件）。
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// ExitSignal 致死信号编号（0 = 正常退出）；Go 的 ExitCode 对信号死亡恒为 -1，
	// 区分 SIGTERM(15)/SIGKILL(9) 需读 WaitStatus（U-F7-02 断言用）。
	ExitSignal int
	Err        error
	Duration   time.Duration
	Killed     bool
	KillReason string // "timeout"（ctx deadline）| "canceled"（ctx 取消）
	Tail       string // 被 kill 时的输出尾部（≤4KB）
}

// ErrTimeoutErr 判别：ctx 超时导致的失败。
var ErrTimeout = errors.New("exec: child process timeout")

// ErrCanceled ctx 主动取消导致的失败。
var ErrCanceled = errors.New("exec: child process canceled")

// 全局配置（进程级）。
type config struct {
	mu          sync.Mutex
	maxChildren int
	limiter     *semaphore.Weighted
	killGrace   time.Duration
	pgidTracker *pgidTracker
}

var global = &config{
	maxChildren: 32, // F7.2 平台总子进程上限
	killGrace:   5 * time.Second,
}

// Configure 进程级配置（serve 启动时调用）：总子进程上限、SIGTERM→SIGKILL 宽限、pgid 记录文件。
func Configure(maxChildren int, killGrace time.Duration, pgidFile string) error {
	global.mu.Lock()
	defer global.mu.Unlock()
	if maxChildren > 0 {
		global.maxChildren = maxChildren
	}
	if killGrace > 0 {
		global.killGrace = killGrace
	}
	if pgidFile != "" {
		t, err := newPgidTracker(pgidFile)
		if err != nil {
			return err
		}
		global.pgidTracker = t
	}
	global.limiter = semaphore.NewWeighted(int64(global.maxChildren))
	return nil
}

// DefaultEnvWhitelist 从父进程继承的环境变量白名单（显式继承，F7.2）。
var DefaultEnvWhitelist = []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "TERM"}

// DeriveDeadline 超时三层合成（F7.2）：节点/任务超时优先，其次沿用父 ctx deadline，
// 最后 default_child_timeout 平台兜底。全部缺省时返回 warn=true（U-F7-04 启动告警）。
func DeriveDeadline(parent context.Context, taskTimeout, platformDefault time.Duration) (context.Context, context.CancelFunc, bool) {
	warn := false
	_, parentHasDeadline := parent.Deadline()
	switch {
	case taskTimeout > 0:
		ctx, cancel := context.WithTimeout(parent, taskTimeout)
		return ctx, cancel, warn
	case parentHasDeadline:
		return parent, func() {}, warn
	case platformDefault > 0:
		ctx, cancel := context.WithTimeout(parent, platformDefault)
		return ctx, cancel, warn
	default:
		// default_child_timeout=0 且节点无超时 → 告警（调用方启动时输出）
		return parent, func() {}, true
	}
}

const (
	maxOutputDefault = 1 << 20 // 1MB
	tailLimit        = 4 << 10 // 4KB
)

// Run 执行子进程并全生命周期管控（资源上限排队 → spawn → 超时/取消升级 kill → 收尸）。
func Run(ctx context.Context, opts Options) Result {
	global.mu.Lock()
	limiter, grace, tracker := global.limiter, global.killGrace, global.pgidTracker
	global.mu.Unlock()
	if limiter == nil {
		limiter = semaphore.NewWeighted(int64(global.maxChildren))
		global.mu.Lock()
		global.limiter = limiter
		global.mu.Unlock()
	}
	maxOut := opts.MaxOutput
	if maxOut <= 0 {
		maxOut = maxOutputDefault
	}

	start := time.Now()
	res := Result{}

	// 平台总子进程上限：满则排队（U-F7-03）
	if err := limiter.Acquire(ctx, 1); err != nil {
		res.Err = fmt.Errorf("exec: wait for child slot: %w", err)
		res.Duration = time.Since(start)
		return res
	}
	defer limiter.Release(1)

	cmd := exec.Command(opts.Bin, opts.Args...)           //nolint:gosec // 命令仅来自配置文件（test-plan 3.3 安全验证）
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 独立进程组
	cmd.Env = buildEnv(opts.Env)
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		res.Err = err
		res.Duration = time.Since(start)
		return res
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		res.Err = err
		res.Duration = time.Since(start)
		return res
	}
	var stdin io.WriteCloser
	if opts.Stdin != "" {
		stdin, err = cmd.StdinPipe()
		if err != nil {
			res.Err = err
			res.Duration = time.Since(start)
			return res
		}
	}

	var outBuf, errBuf boundedBuf
	outBuf.reset(maxOut)
	errBuf.reset(maxOut)

	if err := cmd.Start(); err != nil {
		res.Err = fmt.Errorf("exec: start %s: %w", opts.Bin, err)
		res.Duration = time.Since(start)
		return res
	}
	pgid := cmd.Process.Pid // Setpgid: pgid == 子进程 pid
	if tracker != nil {
		tracker.add(pgid)
	}

	if stdin != nil {
		go func() {
			_, _ = io.WriteString(stdin, opts.Stdin)
			_ = stdin.Close()
		}()
	}
	// 输出管道并发捕获；收尸：Wait 在 goroutine 中成对调用（F7.2）
	go func() { _, _ = io.Copy(&outBuf, stdout); stdout.Close() }()
	go func() { _, _ = io.Copy(&errBuf, stderr); stderr.Close() }()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case werr := <-waitDone:
		res.Stdout = outBuf.String()
		res.Stderr = errBuf.String()
		res.Duration = time.Since(start)
		if werr != nil {
			res.Err = werr
			if ee, ok := werr.(*exec.ExitError); ok {
				res.ExitCode = ee.ExitCode()
				res.ExitSignal = exitSignal(ee)
			}
		} else {
			res.ExitCode = 0
		}
	case <-ctx.Done():
		// 取消语义：kill(-pgid, SIGTERM) → 宽限 → SIGKILL
		reason := ErrTimeout
		if errors.Is(ctx.Err(), context.Canceled) {
			reason = ErrCanceled
		}
		killReason := "timeout"
		if reason == ErrCanceled {
			killReason = "canceled"
		}
		killGroup(pgid, grace)

		werr := <-waitDone // 等 Wait 收尸完成
		res.Killed = true
		res.KillReason = killReason
		res.Stdout = outBuf.String()
		res.Stderr = errBuf.String()
		res.Tail = tailOf(res.Stdout + "\n" + res.Stderr)
		res.Duration = time.Since(start)
		res.Err = fmt.Errorf("%w: %s (pgid=%d)", reason, killReason, pgid)
		if ee, ok := werr.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode() // 信号死亡时为 -1
			res.ExitSignal = exitSignal(ee)
		}
	}

	if tracker != nil {
		tracker.remove(pgid)
	}
	return res
}

// exitSignal 从 WaitStatus 提取致死信号（非信号死亡返回 0）。
func exitSignal(ee *exec.ExitError) int {
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if ok && ws.Signaled() {
		return int(ws.Signal())
	}
	return 0
}

// killGroup SIGTERM 整组 → 宽限 → SIGKILL 整组（U-F7-02）。
func killGroup(pgid int, grace time.Duration) {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !groupAlive(pgid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// groupAlive 进程组是否仍有存活进程（kill -pgid 0 探测）。
func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// AliveGroup 进程组存活探测（测试/U-F7-01 断言用）。
func AliveGroup(pgid int) bool { return groupAlive(pgid) }

// CleanupStale 启动时清理上次残留（按记录的 pgid 扫描，F7.2 收尸）。
// 返回清理掉的 pgid 列表；对已消失的记录仅做清除。
func CleanupStale() []int {
	global.mu.Lock()
	tracker := global.pgidTracker
	grace := global.killGrace
	global.mu.Unlock()
	if tracker == nil {
		return nil
	}
	pgids := tracker.list()
	var cleaned []int
	for _, pgid := range pgids {
		if groupAlive(pgid) {
			// 残留进程组：TERM → 短宽限 → KILL
			killGroup(pgid, minDuration(grace, 2*time.Second))
			cleaned = append(cleaned, pgid)
		}
		tracker.remove(pgid)
	}
	return cleaned
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func buildEnv(extra []string) []string {
	env := make([]string, 0, len(DefaultEnvWhitelist)+len(extra)+4)
	parent := os.Environ()
	for _, key := range DefaultEnvWhitelist {
		for _, kv := range parent {
			if strings.HasPrefix(kv, key+"=") {
				env = append(env, kv)
				break
			}
		}
	}
	env = append(env, extra...)
	return env
}

func tailOf(s string) string {
	if len(s) <= tailLimit {
		return s
	}
	b := []byte(s)
	// 对齐 rune 边界
	for i := len(b) - tailLimit; i < len(b); i++ {
		if b[i] < 0x80 || b[i] >= 0xC0 { // ASCII 或 rune 首字节
			return string(b[i-1:])
		}
	}
	return string(b[len(b)-tailLimit:])
}

// boundedBuf 有上限的输出捕获。
type boundedBuf struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	cap_ int
}

func (b *boundedBuf) reset(capBytes int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cap_ = capBytes
	b.buf.Reset()
}

func (b *boundedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	room := b.cap_ - b.buf.Len()
	if room <= 0 {
		return len(p), nil // 超限丢弃（进程仍继续跑）
	}
	if len(p) > room {
		b.buf.Write(p[:room])
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *boundedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ---------- pgid 记录（收尸扫描）----------

type pgidTracker struct {
	mu    sync.Mutex
	path  string
	pgids map[int]bool
}

func newPgidTracker(path string) (*pgidTracker, error) {
	t := &pgidTracker{path: path, pgids: make(map[int]bool)}
	raw, err := os.ReadFile(path)
	if err == nil && len(raw) > 0 {
		var list []int
		if json.Unmarshal(raw, &list) == nil {
			for _, p := range list {
				t.pgids[p] = true
			}
		}
	}
	if err := t.flush(); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *pgidTracker) add(pgid int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pgids[pgid] = true
	_ = t.flushLocked()
}

func (t *pgidTracker) remove(pgid int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.pgids, pgid)
	_ = t.flushLocked()
}

func (t *pgidTracker) list() []int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]int, 0, len(t.pgids))
	for p := range t.pgids {
		out = append(out, p)
	}
	return out
}

func (t *pgidTracker) flush() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.flushLocked()
}

func (t *pgidTracker) flushLocked() error {
	list := make([]int, 0, len(t.pgids))
	for p := range t.pgids {
		list = append(list, p)
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, t.path)
}

// LookPath 在 PATH 白名单环境内查找可执行文件（探活兜底用）。
func LookPath(bin string) (string, error) {
	return exec.LookPath(bin)
}
