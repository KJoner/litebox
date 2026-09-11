package node

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
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
	cache map[string]*cachedBinary
}

// cachedBinary 是读进内存的一份二进制,连同读它时文件的大小与修改时间。
//
// 带上这两样是为了认出"文件换过了":升级 sing-box 的做法是重新构建、把新文件
// 放进这个目录,不一定会重启面板。只按架构缓存的话,面板会一直分发第一次读到的
// 那一份 —— 管理员在每台机器上点了「重新安装」,装上去的还是旧版,
// 而面板说"已上传"。
type cachedBinary struct {
	size    int64
	modTime time.Time
	data    []byte
	sha256  string
}

func NewDirBinaryProvider(dir string) *DirBinaryProvider {
	return &DirBinaryProvider{
		dir: dir, name: "sing-box", cache: map[string]*cachedBinary{},
		hint: "请先执行 scripts/build-singbox.sh 构建",
	}
}

// NewNamedBinaryProvider 供 mita / mieru 这类**上游直接发布**的二进制用。
//
// 它们与 sing-box 不同:我们不自己构建(没有需要调整的构建标签),
// 只是把上游 release 的那一份钉到一个版本、下发到节点。
func NewNamedBinaryProvider(dir, name, hint string) *DirBinaryProvider {
	return &DirBinaryProvider{dir: dir, name: name, cache: map[string]*cachedBinary{}, hint: hint}
}

func (p *DirBinaryProvider) path(arch string) string {
	return filepath.Join(p.dir, p.name+"-linux-"+arch)
}

// Load 读取指定架构的二进制。内容会缓存在内存中 ——
// sing-box 约 36MB、mita 约 13MB,多节点部署时反复读盘没有意义。
func (p *DirBinaryProvider) Load(arch string) ([]byte, error) {
	c, err := p.load(arch)
	if err != nil {
		return nil, err
	}
	return c.data, nil
}

// SHA256 返回指定架构那份二进制的哈希,与 Load 读到的是同一份。
//
// 服务卡片拿它与节点上那个文件比,回答"点「重新安装」会不会换掉东西"。
func (p *DirBinaryProvider) SHA256(arch string) (string, error) {
	c, err := p.load(arch)
	if err != nil {
		return "", err
	}
	return c.sha256, nil
}

func (p *DirBinaryProvider) load(arch string) (*cachedBinary, error) {
	if arch != "amd64" && arch != "arm64" {
		return nil, fmt.Errorf("不支持的节点架构 %q,目前只提供 amd64 与 arm64", arch)
	}
	path := p.path(arch)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("未找到 %s 架构的 %s 二进制(%s),%s",
				arch, p.name, path, p.hint)
		}
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.cache[arch]; ok && c.size == info.Size() && c.modTime.Equal(info.ModTime()) {
		return c, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	c := &cachedBinary{
		size: info.Size(), modTime: info.ModTime(),
		data: data, sha256: hex.EncodeToString(sum[:]),
	}
	p.cache[arch] = c
	return c, nil
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
