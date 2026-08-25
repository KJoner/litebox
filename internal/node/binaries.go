package node

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// BinaryProvider 按架构提供要分发到节点的 sing-box 二进制。
type BinaryProvider interface {
	Load(arch string) ([]byte, error)
	Available() map[string]int64
}

// DirBinaryProvider 从本地目录读取二进制,文件名形如:
//
//	sing-box-linux-amd64
//	sing-box-linux-arm64
//
// 这些文件由 scripts/build-singbox.sh 构建产生,必须带 with_v2ray_api 标签。
type DirBinaryProvider struct {
	dir string
	// name 是文件名前缀(sing-box / mita / mieru)。
	//
	// 参数化而不是给每种二进制复制一份 provider:三者的读取、缓存与
	// "架构不支持"的报错完全一样,复制三份只是给以后改缓存策略的人
	// 留三个必须同时改的地方。
	name string
	// hint 是找不到文件时告诉管理员该跑哪个脚本。
	hint string

	mu    sync.Mutex
	cache map[string][]byte
}

func NewDirBinaryProvider(dir string) *DirBinaryProvider {
	return &DirBinaryProvider{
		dir: dir, name: "sing-box", cache: map[string][]byte{},
		hint: "请先执行 scripts/build-singbox.sh 构建",
	}
}

// NewPreviewBinaryProvider 读同一个目录下的预览版构建(V14)。
//
// 文件名是 sing-box-preview-linux-<arch>,由 scripts/build-singbox.sh
// 带 SINGBOX_CHANNEL=preview 产出。两支放同一个目录而不是两个目录:
// 它们是同一件东西的两个版本,分开只会多一个要在部署文档里解释的路径。
func NewPreviewBinaryProvider(dir string) *DirBinaryProvider {
	return &DirBinaryProvider{
		dir: dir, name: "sing-box-preview", cache: map[string][]byte{},
		hint: "请先执行 SINGBOX_CHANNEL=preview scripts/build-singbox.sh 构建",
	}
}

// NewNamedBinaryProvider 供 mita / mieru 这类**上游直接发布**的二进制用。
//
// 它们与 sing-box 不同:我们不自己构建(没有需要调整的构建标签),
// 只是把上游 release 的那一份钉到一个版本、下发到节点。
func NewNamedBinaryProvider(dir, name, hint string) *DirBinaryProvider {
	return &DirBinaryProvider{dir: dir, name: name, cache: map[string][]byte{}, hint: hint}
}

func (p *DirBinaryProvider) path(arch string) string {
	return filepath.Join(p.dir, p.name+"-linux-"+arch)
}

// Load 读取指定架构的二进制。内容会缓存在内存中 ——
// sing-box 约 28MB、mita 约 13MB,多节点部署时反复读盘没有意义。
func (p *DirBinaryProvider) Load(arch string) ([]byte, error) {
	if arch != "amd64" && arch != "arm64" {
		return nil, fmt.Errorf("不支持的节点架构 %q,目前只提供 amd64 与 arm64", arch)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if data, ok := p.cache[arch]; ok {
		return data, nil
	}

	path := p.path(arch)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("未找到 %s 架构的 %s 二进制(%s),%s",
				arch, p.name, path, p.hint)
		}
		return nil, err
	}
	p.cache[arch] = data
	return data, nil
}

// Available 返回目录中已就绪的二进制及其大小。
func (p *DirBinaryProvider) Available() map[string]int64 {
	result := map[string]int64{}
	for _, arch := range []string{"amd64", "arm64"} {
		if info, err := os.Stat(p.path(arch)); err == nil {
			result[arch] = info.Size()
		}
	}
	return result
}
