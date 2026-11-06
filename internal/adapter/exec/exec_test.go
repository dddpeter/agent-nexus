package exec_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	execx "github.com/dddpeter/agent-nexus/internal/adapter/exec"
)

func resetGlobal(t *testing.T) {
	t.Helper()
	require.NoError(t, execx.Configure(32, 5*time.Second, ""))
	t.Cleanup(func() { _ = execx.Configure(32, 5*time.Second, "") })
}

// U-F7-01 挂起模式（不退出）→ timeout 触发 → 进程组被 kill（ps 断言无残留）。
func TestGroupKillOnTimeout_U_F7_01(t *testing.T) {
	resetGlobal(t)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	res := execx.Run(ctx, execx.Options{
		Bin:  "sh",
		Args: []string{"-c", "echo started; sleep 300 & sleep 300"}, // 含孙进程，验证整组 kill
	})
	require.True(t, r2(t, res).Killed, "应触发 kill")
	require.ErrorIs(t, res.Err, execx.ErrTimeout)
	require.Contains(t, res.Stdout, "started")

	// ps 断言无残留
	time.Sleep(150 * time.Millisecond) // 内核回收窗口
	out, err := exec.Command("sh", "-c", `ps -eo args | grep "sleep 300" | grep -v grep || true`).Output()
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(string(out)), "进程组（含孙进程）应无残留:\n%s", out)
}

// r2 恒等辅助（保持断言链简洁）。
func r2(t *testing.T, res execx.Result) execx.Result { t.Helper(); return res }

// U-F7-02 SIGTERM 后未退 → SIGKILL 补刀（宽限期可配，默认 5s；缩短验证语义）。
func TestSIGKILLEscalation_U_F7_02(t *testing.T) {
	require.NoError(t, execx.Configure(32, 300*time.Millisecond, ""))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := execx.Run(ctx, execx.Options{
		Bin:  "sh",
		Args: []string{"-c", `trap "" TERM; echo ignoring-term; sleep 300`},
	})
	elapsed := time.Since(start)
	require.True(t, res.Killed)
	require.Equal(t, "timeout", res.KillReason)
	assert.Equal(t, int(syscall.SIGKILL), res.ExitSignal, "SIGTERM 被忽略，应以 SIGKILL 补刀")
	assert.Equal(t, -1, res.ExitCode, "信号死亡退出码为 -1")
	assert.GreaterOrEqual(t, elapsed, 500*time.Millisecond, "超时点(200ms)+宽限(300ms)后才退出")
	assert.Contains(t, res.Stdout, "ignoring-term")
}

// U-F7-03 平台总子进程达上限 → 新任务排队。
func TestGlobalChildLimitQueue_U_F7_03(t *testing.T) {
	require.NoError(t, execx.Configure(2, 5*time.Second, ""))
	defer func() { _ = execx.Configure(32, 5*time.Second, "") }()

	start := time.Now()
	results := make(chan execx.Result, 3)
	for i := 0; i < 3; i++ {
		go func() {
			results <- execx.Run(context.Background(), execx.Options{
				Bin: "sh", Args: []string{"-c", "sleep 0.8"},
			})
		}()
	}

	// 占满后并发执行的 sleep 不得超过 2
	time.Sleep(300 * time.Millisecond)
	out, err := exec.Command("sh", "-c", `ps -eo args | grep -c "^sh -c sleep 0.8" || true`).Output()
	require.NoError(t, err)
	n := strings.TrimSpace(string(out))
	assert.Equal(t, "2", n, "并发子进程不得超过上限 2，实际 %s", n)

	// 三个都完成：第三个排队 → 总耗时 ≥ 1.6s
	for i := 0; i < 3; i++ {
		res := <-results
		require.NoError(t, res.Err)
	}
	assert.GreaterOrEqual(t, time.Since(start), 1500*time.Millisecond, "第三个任务应排队等待")
}

// U-F7-04 default_child_timeout=0 且节点无超时 → 启动告警（DeriveDeadline warn=true）。
func TestDeriveDeadlineWarn_U_F7_04(t *testing.T) {
	// 三层全缺省 → warn
	_, cancel, warn := execx.DeriveDeadline(context.Background(), 0, 0)
	defer cancel()
	assert.True(t, warn, "三层超时全缺省应返回告警")

	// 节点超时存在 → 不告警
	ctx2, cancel2, warn2 := execx.DeriveDeadline(context.Background(), 5*time.Second, 0)
	defer cancel2()
	assert.False(t, warn2)
	_, hasDL := ctx2.Deadline()
	require.True(t, hasDL)

	// 父 ctx 已有 deadline → 沿用，不告警
	pctx, pcancel := context.WithTimeout(context.Background(), time.Minute)
	defer pcancel()
	_, cancel3, warn3 := execx.DeriveDeadline(pctx, 0, 0)
	defer cancel3()
	assert.False(t, warn3)

	// 平台兜底生效
	ctx4, cancel4, warn4 := execx.DeriveDeadline(context.Background(), 0, 30*time.Minute)
	defer cancel4()
	assert.False(t, warn4)
	_, hasDL4 := ctx4.Deadline()
	require.True(t, hasDL4)
}

// U-F7-05 kill 后输出尾部与退出码记录（Result 携带，adapter 层据此发 error 事件）。
func TestKilledResultTailAndExitCode_U_F7_05(t *testing.T) {
	resetGlobal(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res := execx.Run(ctx, execx.Options{
		Bin:  "sh",
		Args: []string{"-c", "echo line1; echo line2; echo line3; sleep 300"},
	})
	require.True(t, res.Killed)
	require.Error(t, res.Err)
	assert.Negative(t, res.ExitCode, "被信号杀死退出码应为负")
	assert.NotZero(t, res.ExitSignal, "致死信号应被记录")
	assert.Contains(t, res.Tail, "line3", "输出尾部应记录")
	assert.Contains(t, res.Stdout, "line1")
}

// 启动收尸：残留 pgid 清理（F7.2 收尸）。
func TestCleanupStaleOnStartup(t *testing.T) {
	pgidFile := t.TempDir() + "/pgids.json"

	// 模拟上次运行残留的进程组（不经过 Run，手动 spawn）
	cmd := exec.Command("sh", "-c", "sleep 300 & sleep 300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	pgid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()

	// 把 pgid 写进记录文件（模拟上次崩溃未清理）
	raw, err := json.Marshal([]int{pgid})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(pgidFile, raw, 0o600))

	// 重新加载 tracker + 启动清理
	require.NoError(t, execx.Configure(32, 300*time.Millisecond, pgidFile))
	cleaned := execx.CleanupStale()
	require.Contains(t, cleaned, pgid, "残留进程组应被清理")
	time.Sleep(150 * time.Millisecond)
	assert.False(t, execx.AliveGroup(pgid), "清理后进程组应不存在")
}

// ctx 取消（非超时）也应整组 kill。
func TestCancelKillsGroup(t *testing.T) {
	resetGlobal(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan execx.Result, 1)
	go func() {
		done <- execx.Run(ctx, execx.Options{
			Bin: "sh", Args: []string{"-c", "sleep 300"},
		})
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	res := <-done
	require.True(t, res.Killed)
	require.Equal(t, "canceled", res.KillReason)
	require.ErrorIs(t, res.Err, execx.ErrCanceled)
}

// stdin 注入与退出码透传；env 白名单。
func TestStdinEnvAndExitCode(t *testing.T) {
	resetGlobal(t)
	res := execx.Run(context.Background(), execx.Options{
		Bin:   "sh",
		Args:  []string{"-c", `cat; echo "env=$NEXUS_TEST_FLAG"; exit 7`},
		Stdin: "hello-stdin",
		Env:   []string{"NEXUS_TEST_FLAG=42"},
	})
	require.Error(t, res.Err, "非零退出应报错")
	assert.Equal(t, 7, res.ExitCode, "退出码应透传")
	assert.Contains(t, res.Stdout, "hello-stdin")
	assert.Contains(t, res.Stdout, "env=42")
}
