package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/litebox/litebox/internal/sshx"
)

// BinaryProvider 按架构提供要分发到节点的二进制(sing-box / mita / mieru / realm / litebox-probe)。
//
// **内容不进内存**(VNext CORE03):原来 Load 把 sing-box(~36MB × 两个架构)、mita、mieru 整份读进
// Go 堆并一直缓存着,查一次哈希也要先整份读进来。现在只缓存「路径 + 大小 + 修改时间 → 哈希」,
// 上传时按需打开文件、经 SFTP 流式发送(UploadArtifact),边传边核对哈希。
type BinaryProvider interface {
	Artifact(arch string) (Artifact, error)
	Available() map[string]int64
}

// Artifact 是一份可以分发的二进制(定义在 sshx,探测辅助程序那一侧也用它)。
type Artifact = sshx.Artifact

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
	cache map[string]*cachedHash
	// manifest 是发布清单(VNext CORE03);nil 表示开发环境自己构建、不核对。
	manifest *Manifest
}

// SetManifest 让这个目录里的文件在分发前与发布清单核对。
func (p *DirBinaryProvider) SetManifest(m *Manifest) {
	p.mu.Lock()
	p.manifest = m
	p.mu.Unlock()
}

// cachedHash 是一份二进制的哈希,连同算它时文件的大小与修改时间。
//
// 带上这两样是为了认出"文件换过了":升级 sing-box 的做法是重新构建、把新文件
// 放进这个目录,不一定会重启面板。只按架构缓存的话,面板会一直报第一次读到的
// 那个哈希 —— 管理员在每台机器上点了「重新安装」,装上去的还是旧版,
// 而面板说"已上传"。
type cachedHash struct {
	size    int64
	modTime time.Time
	sha256  string
}

func NewDirBinaryProvider(dir string) *DirBinaryProvider {
	return &DirBinaryProvider{
		dir: dir, name: "sing-box", cache: map[string]*cachedHash{},
		hint: "请先执行 scripts/build-singbox.sh 构建",
	}
}

// NewNamedBinaryProvider 供 mita / mieru 这类**上游直接发布**的二进制用。
//
// 它们与 sing-box 不同:我们不自己构建(没有需要调整的构建标签),
// 只是把上游 release 的那一份钉到一个版本、下发到节点。
func NewNamedBinaryProvider(dir, name, hint string) *DirBinaryProvider {
	return &DirBinaryProvider{dir: dir, name: name, cache: map[string]*cachedHash{}, hint: hint}
}

func (p *DirBinaryProvider) path(arch string) string {
	return filepath.Join(p.dir, p.name+"-linux-"+arch)
}

// SHA256 返回指定架构那份二进制的哈希。
//
// 服务卡片拿它与节点上那个文件比,回答"点「重新安装」会不会换掉东西"。
func (p *DirBinaryProvider) SHA256(arch string) (string, error) {
	a, err := p.Artifact(arch)
	if err != nil {
		return "", err
	}
	return a.SHA256, nil
}

// Artifact 返回指定架构的那份文件。哈希按 (大小, 修改时间) 缓存,
// 文件没变时不再读一遍;算哈希是流式读,不把整份文件放进内存。
func (p *DirBinaryProvider) Artifact(arch string) (Artifact, error) {
	if arch != "amd64" && arch != "arm64" {
		return Artifact{}, fmt.Errorf("不支持的节点架构 %q,目前只提供 amd64 与 arm64", arch)
	}
	path := p.path(arch)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Artifact{}, fmt.Errorf("未找到 %s 架构的 %s 二进制(%s),%s",
				arch, p.name, path, p.hint)
		}
		return Artifact{}, err
	}
	if info.Size() == 0 {
		return Artifact{}, fmt.Errorf("%s 架构的 %s 二进制(%s)是空文件,%s", arch, p.name, path, p.hint)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.cache[arch]; ok && c.size == info.Size() && c.modTime.Equal(info.ModTime()) {
		return Artifact{Path: path, Size: c.size, SHA256: c.sha256}, nil
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return Artifact{}, err
	}
	// 与发布清单核对:哈希对不上(被换过、下载不完整、构建串了)就拒绝分发,不往节点上送一份来路不明的东西。
	if abs, err := filepath.Abs(path); err == nil {
		if want, ok := p.manifest.lookup(abs); ok && want.SHA256 != sum {
			return Artifact{}, fmt.Errorf("%s 与发布清单不一致(清单 %.12s,文件 %.12s),拒绝分发 —— 重新拉取这个版本的镜像或发布包",
				path, want.SHA256, sum)
		}
	}
	p.cache[arch] = &cachedHash{size: info.Size(), modTime: info.ModTime(), sha256: sum}
	return Artifact{Path: path, Size: info.Size(), SHA256: sum}, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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

// UploadArtifact 把一份二进制流式传到节点的 remotePath,边传边核对哈希。
func UploadArtifact(ctx context.Context, client *sshx.Client, a Artifact, remotePath string, mode uint32) error {
	return client.UploadArtifact(ctx, a, remotePath, mode)
}
