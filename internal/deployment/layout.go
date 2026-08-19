// Package deployment 实现节点配置的部署事务与自动回滚。
package deployment

import (
	"fmt"
	"time"
)

// Layout 描述节点上 LiteBox 相关文件的位置与服务名。
//
// 路径与服务名是全局固定的,因此**一台机器只能承载一个节点**。
// 把两个节点记录指向同一主机会让它们互相覆盖配置并重启同一个服务。
// V1 不支持这种用法:节点本来就是独立的 VPS,为多租户单机拆分路径
// 只会把部署事务与回滚逻辑复杂化。
type Layout struct {
	BaseDir     string
	ConfigPath  string
	BackupDir   string
	BinaryPath  string
	ServiceName string

	// 中转用的 nginx 走**独立实例**:自己的配置、自己的 pid、自己的服务名。
	//
	// 不往 /etc/nginx/nginx.conf 里加 include —— 那是管理员或发行版的文件,
	// 改它与改 sshd_config 同级敏感,而我们没有任何必要去改。机器上原本的
	// nginx 服务一个字不动、也不启用;我们只借用它的二进制。
	NginxConfigPath  string
	NginxPIDPath     string
	NginxErrorLog    string
	RelayServiceName string
}

// DefaultLayout 返回默认布局。
func DefaultLayout() Layout {
	return Layout{
		BaseDir:     "/opt/litebox",
		ConfigPath:  "/opt/litebox/config.json",
		BackupDir:   "/opt/litebox/backup",
		BinaryPath:  "/opt/litebox/sing-box",
		ServiceName: "litebox-singbox",

		NginxConfigPath:  "/opt/litebox/nginx.conf",
		NginxPIDPath:     "/opt/litebox/nginx.pid",
		NginxErrorLog:    "/opt/litebox/nginx-error.log",
		RelayServiceName: "litebox-nginx",
	}
}

// tempConfigPath 是原子替换前的落地路径。
// 必须与正式配置同目录,否则 mv 会跨文件系统而失去原子性。
func (l Layout) tempConfigPath() string {
	return l.ConfigPath + ".tmp"
}

// backupPath 按 revision 生成备份路径。
func (l Layout) backupPath(revision int64) string {
	return fmt.Sprintf("%s/config-%d.json", l.BackupDir, revision)
}

// probeConfigPath 是健康检查用的临时客户端配置。
func (l Layout) probeConfigPath() string {
	return l.BaseDir + "/probe.json"
}

// tempNginxConfigPath 是 nginx 配置原子替换前的落地路径。
// 与正式配置同目录,否则 mv 会跨文件系统而失去原子性。
func (l Layout) tempNginxConfigPath() string {
	return l.NginxConfigPath + ".tmp"
}

// nginxBackupPath 按 revision 生成 nginx 配置的备份路径。
// 与 sing-box 的备份分开命名,否则两者会互相覆盖 —— 而回滚时取错文件
// 意味着把一份 sing-box 的 JSON 喂给 nginx。
func (l Layout) nginxBackupPath(revision int64) string {
	return fmt.Sprintf("%s/nginx-%d.conf", l.BackupDir, revision)
}

// StepStatus 是单个部署步骤的状态。
type StepStatus string

const (
	StepSuccess StepStatus = "SUCCESS"
	StepFailed  StepStatus = "FAILED"
	StepSkipped StepStatus = "SKIPPED"
)

// Step 记录一个部署步骤的结果,用于事后追溯。
type Step struct {
	Name       string     `json:"name"`
	Status     StepStatus `json:"status"`
	DurationMS int64      `json:"duration_ms"`
	Detail     string     `json:"detail,omitempty"`
}

// Status 是一次部署的最终状态。
type Status string

const (
	StatusSuccess    Status = "SUCCESS"
	StatusFailed     Status = "FAILED"
	StatusRolledBack Status = "ROLLED_BACK"
)

// Kind 区分同一台机器上两种互不相干的下发。
type Kind string

const (
	// KindSingBox 是节点自己的 sing-box 配置下发,会重启服务。
	KindSingBox Kind = "SINGBOX"
	// KindRelay 是 nginx 转发配置下发,只 reload,不打断在途连接。
	KindRelay Kind = "RELAY"
)

// Result 是一次部署事务的完整结果。
type Result struct {
	NodeID int64 `json:"node_id"`
	// Kind 空值按 SINGBOX 处理,与迁移里那一列的默认值一致。
	Kind           Kind      `json:"kind"`
	Revision       int64     `json:"revision"`
	ConfigSHA256   string    `json:"config_sha256"`
	Status         Status    `json:"status"`
	Steps          []Step    `json:"steps"`
	ErrorMessage   string    `json:"error_message,omitempty"`
	RollbackResult string    `json:"rollback_result,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
}

// stepRecorder 累积步骤记录。
type stepRecorder struct {
	steps []Step
}

func (r *stepRecorder) run(name string, fn func() (string, error)) error {
	start := time.Now()
	detail, err := fn()
	step := Step{
		Name:       name,
		Status:     StepSuccess,
		DurationMS: time.Since(start).Milliseconds(),
		Detail:     detail,
	}
	if err != nil {
		step.Status = StepFailed
		step.Detail = err.Error()
	}
	r.steps = append(r.steps, step)
	return err
}

func (r *stepRecorder) skip(name, reason string) {
	r.steps = append(r.steps, Step{Name: name, Status: StepSkipped, Detail: reason})
}
