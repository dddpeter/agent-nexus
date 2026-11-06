// Package migrations 内嵌 goose SQL 迁移（启动时自动 upgrade，technical-design §1）。
package migrations

import "embed"

// FS 内嵌全部迁移脚本；goose SetBaseFS 后以 "." 为迁移目录。
//
//go:embed *.sql
var FS embed.FS
