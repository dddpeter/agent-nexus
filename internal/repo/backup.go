// 备份（U-F6-06 / detailed-design F6.2）：VACUUM INTO 定时快照到 data/backups/，保留最近 K 份。
// VACUUM INTO 产出一致性好、可在线执行（SQLite 3.27+）；恢复 = 用备份文件直接 OpenSQLite。
package repo

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// VacuumBackup 执行一次 VACUUM INTO 快照并修剪旧备份；返回备份文件路径。
// db 为 nil（内存态）或 VACUUM 失败返回错误，不影响主库。
func VacuumBackup(ctx context.Context, db *sql.DB, dir string, keep int) (string, error) {
	if db == nil {
		return "", fmt.Errorf("repo: backup: nil db")
	}
	if keep <= 0 {
		keep = 7
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("repo: backup mkdir: %w", err)
	}
	name := "nexus-" + time.Now().Format("20060102-150405.000000000") + ".db" // 纳秒精度：同秒多次备份不冲突，定宽字典序=时间序
	dest := filepath.Join(dir, name)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return "", fmt.Errorf("repo: vacuum into %s: %w", dest, err)
	}
	pruneBackups(dir, keep)
	return dest, nil
}

// pruneBackups 按文件名（时间戳序）保留最近 keep 份。
func pruneBackups(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "nexus-") && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // 时间戳命名 → 字典序即时间序
	for i := 0; i < len(names)-keep; i++ {
		_ = os.Remove(filepath.Join(dir, names[i]))
	}
}

// Backup sqliteStore 快照入口（serve 备份循环调用；内存 store 无此能力）。
func (s *sqliteStore) Backup(ctx context.Context, dir string, keep int) (string, error) {
	return VacuumBackup(ctx, s.db, dir, keep)
}
