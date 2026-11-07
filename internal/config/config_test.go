package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dddpeter/agent-nexus/internal/config"
)

const sampleYAML = `
server:
  listen: :9090
  auth_token: ${NEXUS_TEST_TOKEN}
store:
  driver: sqlite
  dsn: file:data/test.db
registry:
  heartbeat_interval: 10s
  offline_threshold: 5
  default_child_timeout: 0
agents:
  - name: echod-main
    adapter: echod
    config:
      mode: echo
      api_key: ${NEXUS_TEST_ECHOD_KEY}
`

func TestLoadYAMLAndEnvRefs(t *testing.T) {
	require.NoError(t, os.Setenv("NEXUS_TEST_TOKEN", "sk-cli-token"))
	require.NoError(t, os.Setenv("NEXUS_TEST_ECHOD_KEY", "sk-agent-key"))
	defer os.Unsetenv("NEXUS_TEST_TOKEN")
	defer os.Unsetenv("NEXUS_TEST_ECHOD_KEY")

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(sampleYAML), 0o600))

	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, ":9090", cfg.Server.Listen)
	assert.Equal(t, "sk-cli-token", cfg.Server.AuthToken, "${ENV} 应解析")
	assert.Equal(t, 10*time.Second, cfg.Registry.HeartbeatInterval.D())
	assert.Equal(t, 5, cfg.Registry.OfflineThreshold)
	assert.Equal(t, time.Duration(0), cfg.Registry.DefaultChildTimeout.D(), "显式 0 保留（触发启动告警）")
	require.Len(t, cfg.Agents, 1)
	assert.Equal(t, "sk-agent-key", cfg.Agents[0].Config["api_key"])
	assert.Equal(t, "data/pgids.json", cfg.Registry.PgidFile, "相对 data_dir 默认")
}

func TestDefaultsWhenFileMissing(t *testing.T) {
	cfg, err := config.Load("/nonexistent/config.yaml")
	require.NoError(t, err)
	assert.Equal(t, ":8080", cfg.Server.Listen)
	assert.Equal(t, "builtin", cfg.Orchestration.Engine)
	assert.Equal(t, 15*time.Second, cfg.Registry.HeartbeatInterval.D())
	assert.Equal(t, 3, cfg.Registry.OfflineThreshold)
	assert.Equal(t, 1800*time.Second, cfg.Registry.DefaultChildTimeout.D())
	assert.Equal(t, 32, cfg.Registry.MaxChildren)
	assert.Equal(t, 5*time.Second, cfg.Registry.KillGrace.D())
	// 跨字段派生默认值同样要生效（曾因文件不存在早退跳过 applyDefaults，实机踩坑）
	assert.Equal(t, "data/backups", cfg.Server.BackupDir, "备份目录默认派生自 data_dir")
	assert.Equal(t, 7, cfg.Server.BackupKeep)
	assert.Equal(t, 24*time.Hour, cfg.Server.BackupInterval.D())
	assert.Equal(t, 24*time.Hour, cfg.Server.SessionIdleTimeout.D(), "闲置回收默认应生效")
	assert.Equal(t, "data/pgids.json", cfg.Registry.PgidFile)
}
