package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Manifest 是一次发布里全部可分发产物的清单(VNext CORE01):每个文件的组件、架构、版本、能力、
// 大小与 sha256。由 `litebox manifest` 在构建镜像 / 发布包时生成,放在 assets/release-manifest.json。
//
// 主控分发任何一个文件之前都拿它核对(DirBinaryProvider.Artifact):哈希对不上就拒绝分发 ——
// 「文件存在且非空」证明不了它是这次发布的那一份(CORE03)。没有清单(开发环境自己构建)时不核对。
type Manifest struct {
	Version string         `json:"version"`
	Commit  string         `json:"commit"`
	BuiltAt string         `json:"built_at"`
	Files   []ManifestFile `json:"files"`

	dir string
}

// ManifestFile 是清单里的一个文件。Path 相对清单所在目录,用正斜杠。
type ManifestFile struct {
	Path         string   `json:"path"`
	Component    string   `json:"component"`
	Arch         string   `json:"arch"`
	Version      string   `json:"version"`
	Size         int64    `json:"size"`
	SHA256       string   `json:"sha256"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// LoadManifest 读清单;文件不存在时返回 (nil, nil)。
func LoadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("解析发布清单 %s: %w", path, err)
	}
	m.dir = filepath.Dir(path)
	return &m, nil
}

// lookup 找 absPath 对应的清单条目。
func (m *Manifest) lookup(absPath string) (ManifestFile, bool) {
	if m == nil {
		return ManifestFile{}, false
	}
	rel, err := filepath.Rel(m.dir, absPath)
	if err != nil {
		return ManifestFile{}, false
	}
	rel = filepath.ToSlash(rel)
	for _, f := range m.Files {
		if f.Path == rel {
			return f, true
		}
	}
	return ManifestFile{}, false
}

// ManifestComponent 是生成清单时一个组件的说明:组件名、文件名前缀、版本、能力。
type ManifestComponent struct {
	Dir          string
	Prefix       string
	Component    string
	Version      string
	Capabilities []string
}

// BuildManifest 扫描 root 下各组件目录(`<Dir>/<Prefix>-linux-<arch>`),生成清单。
func BuildManifest(root, version, commit string, comps []ManifestComponent) (*Manifest, error) {
	m := &Manifest{Version: version, Commit: commit, BuiltAt: time.Now().UTC().Format(time.RFC3339), dir: root}
	for _, c := range comps {
		for _, arch := range []string{"amd64", "arm64"} {
			rel := filepath.ToSlash(filepath.Join(c.Dir, c.Prefix+"-linux-"+arch))
			full := filepath.Join(root, filepath.FromSlash(rel))
			info, err := os.Stat(full)
			if err != nil {
				return nil, fmt.Errorf("清单要的 %s 不存在:%w", rel, err)
			}
			f, err := os.Open(full)
			if err != nil {
				return nil, err
			}
			h := sha256.New()
			_, err = io.Copy(h, f)
			f.Close()
			if err != nil {
				return nil, err
			}
			caps := append([]string{}, c.Capabilities...)
			sort.Strings(caps)
			m.Files = append(m.Files, ManifestFile{
				Path: rel, Component: c.Component, Arch: arch, Version: c.Version,
				Size: info.Size(), SHA256: hex.EncodeToString(h.Sum(nil)), Capabilities: caps,
			})
		}
	}
	return m, nil
}

// WriteManifest 把清单写成缩进的 JSON。
func WriteManifest(m *Manifest, path string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// SingBoxCapabilities 从 build-metadata.json 读出 sing-box 的构建标签,作为它的能力列表。
func SingBoxCapabilities(metadataPath string) []string {
	b, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil
	}
	var meta struct {
		BuildTags string `json:"build_tags"`
	}
	if json.Unmarshal(b, &meta) != nil {
		return nil
	}
	var out []string
	for _, t := range strings.Split(meta.BuildTags, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}
