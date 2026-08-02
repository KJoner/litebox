// Package web 通过 Go embed 提供前端构建产物。
//
// dist 目录由 `npm run build` 生成。为了让后端在前端尚未构建时也能编译,
// 仓库中保留了一个占位的 dist/index.html;正式构建会覆盖它。
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// Assets 返回以 dist 为根的文件系统。
func Assets() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}
