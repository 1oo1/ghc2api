// Package web 内嵌管理控制台的单文件产物 dist/index.html（由 `make web` 构建并提交到 git）。
package web

import "embed"

//go:embed dist/index.html
var assets embed.FS

// Index 返回内嵌的管理控制台 HTML。
func Index() ([]byte, error) {
	return assets.ReadFile("dist/index.html")
}
