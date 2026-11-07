package repo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// U-F6-06：VACUUM INTO 快照生成且可恢复（用备份文件重新 OpenSQLite，数据一致）。
func TestVacuumBackupRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := OpenSQLite(ctx, filepath.Join(dir, "main.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	require.NoError(t, store.Agents().Create(ctx, Agent{
		ID: "a1", Name: "backup-e", Adapter: "echod", ConfigJSON: "{}", CapabilitiesJSON: "{}", Status: StatusOnline,
	}))
	require.NoError(t, store.Groups().Put(ctx, Group{ID: "g1", Name: "备份群", MembersJSON: "[]", ArbiterConfigJSON: "{}"}))

	path, err := VacuumBackup(ctx, store.(*sqliteStore).db, filepath.Join(dir, "backups"), 3)
	require.NoError(t, err)
	require.FileExists(t, path, "备份文件应生成")

	// 快照还原：备份文件可独立打开且数据一致
	restored, err := OpenSQLite(ctx, path)
	require.NoError(t, err, "备份文件应可打开（可恢复性）")
	t.Cleanup(func() { _ = restored.Close() })

	got, err := restored.Agents().Get(ctx, "a1")
	require.NoError(t, err)
	assert.Equal(t, "backup-e", got.Name)

	g, err := restored.Groups().Get(ctx, "g1")
	require.NoError(t, err)
	assert.Equal(t, "备份群", g.Name)

	// 保留策略：超过 keep 份按时间序修剪
	for i := 0; i < 5; i++ {
		_, err := VacuumBackup(ctx, store.(*sqliteStore).db, filepath.Join(dir, "backups"), 3)
		require.NoError(t, err)
	}
	backups := listBackupFiles(t, filepath.Join(dir, "backups"))
	assert.LessOrEqual(t, len(backups), 3, "应只保留最近 3 份")
}

func listBackupFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		// 只计快照本体；还原库打开时产生的 -wal/-shm 不算
		if !e.IsDir() && strings.HasPrefix(e.Name(), "nexus-") && strings.HasSuffix(e.Name(), ".db") {
			out = append(out, e.Name())
		}
	}
	return out
}
