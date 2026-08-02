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

	mu    sync.Mutex
	cache map[string][]byte
}

func NewDirBinaryProvider(dir string) *DirBinaryProvider {
	return &DirBinaryProvider{dir: dir, cache: map[string][]byte{}}
}

func (p *DirBinaryProvider) path(arch string) string {
	return filepath.Join(p.dir, "sing-box-linux-"+arch)
}

// Load 读取指定架构的二进制。内容会缓存在内存中 ——
// 单个文件约 28MB,多节点部署时反复读盘没有意义。
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
			return nil, fmt.Errorf("未找到 %s 架构的 sing-box 二进制(%s),"+
				"请先执行 scripts/build-singbox.sh 构建", arch, path)
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
