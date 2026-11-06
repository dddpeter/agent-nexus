// Package web 内嵌 htmx 前端静态资源与模板（technical-design §1：embed 打包，单二进制）。
package web

import "embed"

// Assets 静态资源（htmx.min.js 等）。
//
//go:embed assets/*
var Assets embed.FS

// Templates HTML 模板（agent 列表 + 注册向导，任务 11 范围）。
//
//go:embed templates/*.gohtml
var Templates embed.FS
