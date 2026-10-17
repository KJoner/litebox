// Package web 通过 Go embed 提供前端构建产物。
//
// dist 目录由 `npm run build` 生成,不进仓库(.gitignore)。前端还没构建过的检出上
// go build / vet / test 会报 pattern all:dist: no matching files found ——
// 先 `make web`,或者像 CI 的 Go job 那样放一个占位的 dist/index.html。
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
