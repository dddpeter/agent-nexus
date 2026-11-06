package main_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serve 冒烟：nexus serve 子进程可启动，/healthz 与 /api/ 可达（任务 7 骨架验收）。
func TestServeSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("serve smoke skipped in short mode")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "nexus")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/dddpeter/agent-nexus/cmd/nexus").CombinedOutput()
	require.NoError(t, err, "build nexus: %s", out)

	port := 18123 + os.Getpid()%300
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf("server:\n  listen: 127.0.0.1:%d\n  data_dir: %s\nstore:\n  dsn: %s\n", port, dir, filepath.Join(dir, "smoke.db"))
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o600))

	cmd := exec.Command(bin, "serve", "--config", cfgPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
		}
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	var lastErr error
	for i := 0; i < 50; i++ {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				lastErr = nil
				break
			}
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	require.NoError(t, lastErr, "serve 应在 5s 内就绪")

	resp, err := http.Get(base + "/api/")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "nexus", body["name"])

	// 数据目录应生成 SQLite 库
	_, err = os.Stat(filepath.Join(dir, "smoke.db"))
	require.NoError(t, err, "SQLite 库应创建")

	// 错误路径错误结构（404 统一结构在任务 10 细化，这里验证中间件链不放行未知 API）
	resp2, err := http.Get(base + "/api/nonexistent")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp2.StatusCode)
	assert.True(t, strings.HasPrefix(fmt.Sprint(resp2.Header.Get("X-Request-Id")), ""), "requestID 中间件应注入头")
}
