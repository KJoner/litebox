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
}

// DefaultLayout 返回默认布局。
func DefaultLayout() Layout {
	return Layout{
		BaseDir:     "/opt/litebox",
		ConfigPath:  "/opt/litebox/config.json",
		BackupDir:   "/opt/litebox/backup",
		BinaryPath:  "/opt/litebox/sing-box",
		ServiceName: "litebox-singbox",
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

// Result 是一次部署事务的完整结果。
type Result struct {
	NodeID         int64     `json:"node_id"`
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
