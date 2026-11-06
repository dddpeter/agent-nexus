// Package web 内嵌前端构建产物（frontend/ → npm run build → dist/，单二进制打包）。
package web

import "embed"

// Dist SPA 构建产物（index.html + assets/ hash 静态资源）。
//
//go:embed all:dist
var Dist embed.FS
