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
	BackupDir   string
	BinaryPath  string
	ServiceName string

	// ConfigInRAM 为真时,**sing-box 的配置与它的备份放在 RuntimeDir 下**,
	// 而 RuntimeDir 是内存文件系统(/run 在 systemd 与 OpenRC 上都是 tmpfs)。
	//
	// 为什么是"不落盘"而不是"启动后删掉":能读 config.json 的人本来就是 root
	// (它是 0600),而 root 可以读 /proc/<pid>/mem、可以重启服务让面板把配置
	// 重新推上来。删文件挡不住他。tmpfs 挡的是另一类人 —— 拿到磁盘镜像、
	// 快照、或者商家手里那块退役硬盘的人,而那恰恰是删文件同样挡不住的。
	//
	// 代价必须写清楚:机器重启后配置就没了,sing-box 起不来,
	// 要靠巡检重新下发把它救回来。所以这个开关只有在巡检开着时才安全。
	ConfigInRAM bool
	// RuntimeDir 是内存文件系统上的目录。ConfigInRAM 为假时不使用。
	RuntimeDir string

	// **nginx 的配置刻意不跟着进内存。** 两个理由:
	// 它里面只有"哪个端口通向哪个地址",是拓扑不是凭据;而它在不在磁盘上
	// 是"这台机器下发过转发没有"唯一可靠的判据,而那个判断正是巡检
	// 决定要不要自动恢复 nginx 的前提。

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
		BackupDir:   "/opt/litebox/backup",
		BinaryPath:  "/opt/litebox/sing-box",
		ServiceName: "litebox-singbox",
		RuntimeDir:  "/run/litebox",

		NginxConfigPath:  "/opt/litebox/nginx.conf",
		NginxPIDPath:     "/opt/litebox/nginx.pid",
		NginxErrorLog:    "/opt/litebox/nginx-error.log",
		RelayServiceName: "litebox-nginx",
	}
}

// WithConfigInRAM 返回一份切换了配置存放位置的副本。
//
// 返回副本而不是就地改:Deployer 上那份 layout 是全局的,
// 按节点改它会让并发部署互相看见对方的设置 —— 而那种错误的表现是
// 配置被写到另一台机器该用的路径上,两台机器的服务都指不到它。
func (l Layout) WithConfigInRAM(inRAM bool) Layout {
	l.ConfigInRAM = inRAM
	return l
}

// ConfigDir 是 sing-box 配置所在的目录。
func (l Layout) ConfigDir() string {
	if l.ConfigInRAM {
		return l.RuntimeDir
	}
	return l.BaseDir
}

// ConfigPath 是 sing-box 的配置文件。
//
// 从字段改成方法是刻意的:它的取值依赖 ConfigInRAM,而字段没有办法
// 表达这种依赖 —— 留成字段的话,某处忘了跟着 ConfigInRAM 一起改,
// 表现是配置写进了 A 路径而服务定义指着 B 路径,sing-box 起不来。
func (l Layout) ConfigPath() string {
	return l.ConfigDir() + "/config.json"
}

// ConfigBackupDir 是 sing-box 配置备份所在的目录。
//
// 跟着配置一起走 —— 备份里是**同一份凭据**。只把主配置搬进内存
// 而把备份留在磁盘上,等于什么都没做,而且更糟:管理员会以为已经做了。
func (l Layout) ConfigBackupDir() string {
	if l.ConfigInRAM {
		return l.RuntimeDir + "/backup"
	}
	return l.BackupDir
}

// tempConfigPath 是原子替换前的落地路径。
// 必须与正式配置同目录,否则 mv 会跨文件系统而失去原子性
// —— 配置进了 tmpfs 之后这一条更要紧:/run 与 /opt 一定是两个文件系统。
func (l Layout) tempConfigPath() string {
	return l.ConfigPath() + ".tmp"
}

// TempConfigPathForCleanup 是 tempConfigPath 的导出版本。
//
// 只有切换存放位置时的清理用得到:临时文件通常在 mv 之后就没了,
// 但部署中途失败会把它留在原地 —— 而它是**完整的配置**,
// 里面有全部用户的凭据。只删正式配置而漏掉它,等于白做。
func (l Layout) TempConfigPathForCleanup() string { return l.tempConfigPath() }

// backupPath 按 revision 生成备份路径。
func (l Layout) backupPath(revision int64) string {
	return fmt.Sprintf("%s/config-%d.json", l.ConfigBackupDir(), revision)
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
