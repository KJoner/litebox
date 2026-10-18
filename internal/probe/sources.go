package probe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Origin 是公共目标的来源。
type Origin string

const (
	// OriginManual 管理员自己登记的目标(自建机器,或者他另外取得了授权的目标)。
	OriginManual Origin = "MANUAL"
	// OriginZstatic Zstatic CDN 的三网省级 / 市级节点目录。
	OriginZstatic Origin = "ZSTATIC"
	// OriginTcpQuality TcpQuality 作者运营的 getNodes 节点列表(首版只收 CDN 类)。
	OriginTcpQuality Origin = "TCPQUALITY"
)

// 来源级限额的取值范围。第三方来源的最小间隔默认 300 秒,只有管理员声明拿到了
// 来源方更高频率的许可之后才允许调低,而且最低 60 秒;与迁移 0042 的 CHECK 一致。
const (
	ThirdPartyMinIntervalSec   = 300
	ThirdPartyFloorIntervalSec = 60
	MaxPerTargetPerMin         = 600
)

var (
	// ErrSourceNotFound 表示来源不存在。
	ErrSourceNotFound = errors.New("目标来源不存在")
	// ErrSourceUnauthorized 表示要对节点启用一个还没确认授权的第三方来源的目标。
	ErrSourceUnauthorized = errors.New("这个来源还没有确认授权")
)

// TargetSource 是一个来源与它的限额。
type TargetSource struct {
	Kind       Origin `json:"kind"`
	Name       string `json:"name"`
	ThirdParty bool   `json:"third_party"`
	Paused     bool   `json:"paused"`
	// AuthorizedAt / By 是管理员确认「已阅读说明并具备使用授权」的时间与人;空串 = 没确认。
	// **这只是管理员的声明,不能替代来源方的实际授权。**
	AuthorizedAt    string `json:"authorized_at"`
	AuthorizedBy    string `json:"authorized_by"`
	AuthorizedNote  string `json:"authorized_note"`
	HighFreqAllowed bool   `json:"high_freq_allowed"`
	MinIntervalSec  int    `json:"min_interval_sec"`
	PerTargetPerMin int    `json:"per_target_per_min"`
	SyncState       string `json:"sync_state"`
	LastSyncAt      string `json:"last_sync_at"`
	LastSuccessAt   string `json:"last_success_at"`
	LastError       string `json:"last_error"`
	ItemCount       int    `json:"item_count"`
	UpdatedAt       string `json:"updated_at"`

	// 下面几项不存库。IsAuthorized 是 Authorized() 的结果,给页面用(页面不自己推)。
	IsAuthorized bool     `json:"authorized"`
	Homepage     string   `json:"homepage"`
	Notice       []string `json:"notice"`
	// Syncable 表示能从上游拉列表(Zstatic 目录、TcpQuality);Presets 表示能在本地生成候选(Zstatic 省级)。
	Syncable bool `json:"syncable"`
	Presets  bool `json:"presets"`
	// TargetCount 是目标库里来自它的目标数;ActiveNodes 是此刻对它的目标生效的(机器 × 目标)组合数。
	TargetCount int `json:"target_count"`
	ActiveCount int `json:"active_count"`
}

// Authorized 表示这个来源的目标可以对节点生效:自定义来源不需要确认,第三方的要管理员确认过。
func (s TargetSource) Authorized() bool { return !s.ThirdParty || s.AuthorizedAt != "" }

// FloorSec 是这个来源的目标的探测间隔下限。
func (s TargetSource) FloorSec() int {
	if !s.ThirdParty {
		return PublicMinIntervalSec
	}
	if s.MinIntervalSec < ThirdPartyFloorIntervalSec {
		return ThirdPartyMinIntervalSec
	}
	return s.MinIntervalSec
}

// sourceInfo 是每个来源写死在代码里的说明。说明的原文出处写在 Homepage,
// 这里只是转述要点 —— 界面上照着它提醒管理员,但它不是来源方条款的替代品。
var sourceInfo = map[Origin]struct {
	homepage string
	notice   []string
	syncable bool
	presets  bool
}{
	OriginManual: {
		notice: []string{
			"管理员自己登记的目标:自建机器,或者另外取得了授权的地址。不受第三方来源的授权确认与限额约束,间隔按目标自己的设置(最低 30 秒)。",
		},
	},
	OriginZstatic: {
		homepage: "https://www.zstaticcdn.com/",
		syncable: true,
		presets:  true,
		notice: []string{
			"Zstatic CDN 的页面注明:节点目录仅供个人测试与研究参考,不能作为网络质量评价、服务承诺、商业宣传或第三方结论依据。",
			"页面要求在合法、合规且获得授权的网络环境中使用,严禁用于扫描、压测、攻击、绕过访问控制或其他未经授权的用途;目录内容可能随时调整、限制或下线。",
			"它不是一个无限制的监测服务:持续周期探测之前,请自行确认使用授权、可接受的频率与目标可用性。面板默认每个目标至少 300 秒一次、全站合计有预算上限。",
			"省级节点按 {省份}-{运营商}-v{4|6}.ip.zstaticcdn.com 的规则在本地生成(端口 80);市级节点只能从目录读取,端口按目录记录(目前是 443)。",
		},
	},
	OriginTcpQuality: {
		homepage: "https://github.com/ibsgss/TcpQuality",
		syncable: true,
		notice: []string{
			"节点列表来自 TcpQuality 作者运营的 getNodes 服务。它的 README 没有声明允许第三方程序调用这个接口:面板只在管理员手动点击时拉取一次,不做定期同步。",
			"首版只收 CDN 类目标(即 Zstatic 的三网省级节点与备用节点);教育网、对象存储、Telegram、Apple CDN 这类第三方业务站点不提供批量导入,确有授权时请用「新增目标」单独登记。",
			"导入之后存在本地,监测不依赖这个接口实时响应;上游接口不可用时保留原配置,只标记来源状态。上游的改动只在预览里列出,由管理员逐条确认,手工改过的字段不会被覆盖。",
		},
	},
}

// originByHostSuffix 把已知第三方来源的域名后缀认回它的来源:管理员用「新增目标」或批量导入
// 登记一个 Zstatic 的域名时,它照样受那个来源的授权确认与限额约束,不能绕过去。
var originByHostSuffix = []struct {
	suffix string
	origin Origin
}{
	{ZstaticDomainSuffix, OriginZstatic},
	{".ibsgss.uk", OriginTcpQuality},
}

// OriginForHost 返回一个主机名所属的已知第三方来源;认不出时返回 OriginManual。
func OriginForHost(host string) Origin {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, s := range originByHostSuffix {
		if strings.HasSuffix(h, s.suffix) {
			return s.origin
		}
	}
	return OriginManual
}

const sourceColumns = `kind, name, third_party, paused, authorized_at, authorized_by, authorized_note,
	high_freq_allowed, min_interval_sec, per_target_per_min, sync_state, last_sync_at, last_success_at,
	last_error, item_count, updated_at`

func scanSource(r scanner) (*TargetSource, error) {
	var s TargetSource
	var third, paused, high int
	if err := r.Scan(&s.Kind, &s.Name, &third, &paused, &s.AuthorizedAt, &s.AuthorizedBy, &s.AuthorizedNote,
		&high, &s.MinIntervalSec, &s.PerTargetPerMin, &s.SyncState, &s.LastSyncAt, &s.LastSuccessAt,
		&s.LastError, &s.ItemCount, &s.UpdatedAt); err != nil {
		return nil, err
	}
	s.ThirdParty, s.Paused, s.HighFreqAllowed = third == 1, paused == 1, high == 1
	s.IsAuthorized = s.Authorized()
	info := sourceInfo[s.Kind]
	s.Homepage, s.Syncable, s.Presets = info.homepage, info.syncable, info.presets
	s.Notice = append([]string{}, info.notice...)
	if s.ThirdParty {
		s.Notice = append(s.Notice, "「确认授权」只是管理员在面板上的声明,记录谁在什么时候确认过;它不能替代来源方的实际授权。")
	}
	return &s, nil
}

// sourcesMap 读出全部来源。
func (s *Store) sourcesMap(ctx context.Context) (map[Origin]TargetSource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sourceColumns+` FROM probe_sources`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Origin]TargetSource{}
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out[src.Kind] = *src
	}
	return out, rows.Err()
}

// ListSources 列出全部来源,带上目标数与生效数。顺序固定:自定义在前。
func (s *Store) ListSources(ctx context.Context) ([]TargetSource, error) {
	g, err := s.LoadGate(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]TargetSource, 0, len(g.Sources))
	for _, kind := range []Origin{OriginManual, OriginZstatic, OriginTcpQuality} {
		src, ok := g.Sources[kind]
		if !ok {
			continue
		}
		for _, p := range g.Publics {
			if p.Source != kind {
				continue
			}
			src.TargetCount++
			for _, n := range g.NodeIDs {
				if g.On(n, p.ID) {
					src.ActiveCount++
				}
			}
		}
		out = append(out, src)
	}
	return out, nil
}

// Source 读一个来源。
func (s *Store) Source(ctx context.Context, kind Origin) (*TargetSource, error) {
	src, err := scanSource(s.db.QueryRowContext(ctx, `SELECT `+sourceColumns+` FROM probe_sources WHERE kind = ?`, kind))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSourceNotFound
	}
	return src, err
}

// SourceInput 是改一个来源时收的东西。nil = 不改。
type SourceInput struct {
	Paused          *bool `json:"paused"`
	MinIntervalSec  *int  `json:"min_interval_sec"`
	PerTargetPerMin *int  `json:"per_target_per_min"`
	HighFreqAllowed *bool `json:"high_freq_allowed"`
	// Authorize 为真 = 确认授权(要带 AuthorizedNote 以外的什么都不用);为假 = 撤销确认,
	// 这个来源的目标随即对所有节点停测。
	Authorize      *bool   `json:"authorize"`
	AuthorizedNote *string `json:"authorized_note"`
}

// UpdateSource 改一个来源。by 是操作的管理员(写进授权确认记录)。
func (s *Store) UpdateSource(ctx context.Context, kind Origin, in SourceInput, by string) (*TargetSource, error) {
	cur, err := s.Source(ctx, kind)
	if err != nil {
		return nil, err
	}
	next := *cur
	if in.Paused != nil {
		next.Paused = *in.Paused
	}
	if !cur.ThirdParty && (in.MinIntervalSec != nil || in.PerTargetPerMin != nil || in.HighFreqAllowed != nil ||
		in.Authorize != nil) {
		return nil, errors.New("自定义目标不受来源限额与授权确认约束,间隔在每个目标上单独设")
	}
	if in.HighFreqAllowed != nil {
		next.HighFreqAllowed = *in.HighFreqAllowed
	}
	if in.MinIntervalSec != nil {
		next.MinIntervalSec = *in.MinIntervalSec
	}
	// 撤销「更高频率许可」时,下限自动回到 300 秒 —— 留着一个 60 秒的下限而许可已经撤了,
	// 是一个没有任何人授权过的频率。
	if !next.HighFreqAllowed && next.MinIntervalSec < ThirdPartyMinIntervalSec {
		if in.MinIntervalSec != nil {
			return nil, fmt.Errorf("没有来源方的更高频率许可时,最小间隔不能低于 %d 秒", ThirdPartyMinIntervalSec)
		}
		next.MinIntervalSec = ThirdPartyMinIntervalSec
	}
	if next.ThirdParty && (next.MinIntervalSec < ThirdPartyFloorIntervalSec || next.MinIntervalSec > PublicMaxIntervalSec) {
		return nil, fmt.Errorf("最小间隔必须是 %d~%d 秒", ThirdPartyFloorIntervalSec, PublicMaxIntervalSec)
	}
	if in.PerTargetPerMin != nil {
		next.PerTargetPerMin = *in.PerTargetPerMin
		if next.PerTargetPerMin < 1 || next.PerTargetPerMin > MaxPerTargetPerMin {
			return nil, fmt.Errorf("单目标全站限流必须是 1~%d 次 / 分钟", MaxPerTargetPerMin)
		}
	}
	now := s.now().UTC().Format(time.RFC3339)
	if in.AuthorizedNote != nil {
		note := strings.TrimSpace(*in.AuthorizedNote)
		if len([]rune(note)) > 200 {
			return nil, errors.New("授权说明最长 200 个字符")
		}
		for _, r := range note {
			if unicode.IsControl(r) {
				return nil, errors.New("授权说明里不能有换行或控制字符")
			}
		}
		next.AuthorizedNote = note
	}
	if in.Authorize != nil {
		if *in.Authorize {
			next.AuthorizedAt, next.AuthorizedBy = now, by
		} else {
			next.AuthorizedAt, next.AuthorizedBy, next.AuthorizedNote = "", "", ""
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE probe_sources SET paused = ?, high_freq_allowed = ?, min_interval_sec = ?,
		per_target_per_min = ?, authorized_at = ?, authorized_by = ?, authorized_note = ?, updated_at = ? WHERE kind = ?`,
		b2i(next.Paused), b2i(next.HighFreqAllowed), next.MinIntervalSec, next.PerTargetPerMin,
		next.AuthorizedAt, next.AuthorizedBy, next.AuthorizedNote, now, kind); err != nil {
		return nil, err
	}
	// 暂停、撤销授权都要立刻让各机器上的序列停下(并清掉告警状态);恢复也在这里重新打开。
	if err := s.SyncOutbound(ctx); err != nil {
		return nil, err
	}
	return s.Source(ctx, kind)
}

// markSync 记下一次从上游拉取的结果。失败时目标库一行都不动,只有来源状态变。
func (s *Store) markSync(ctx context.Context, kind Origin, items int, syncErr error) error {
	now := s.now().UTC().Format(time.RFC3339)
	if syncErr != nil {
		msg := syncErr.Error()
		if r := []rune(msg); len(r) > 200 {
			msg = string(r[:200]) + "…"
		}
		_, err := s.db.ExecContext(ctx, `UPDATE probe_sources SET sync_state = 'ERROR', last_sync_at = ?, last_error = ?
			WHERE kind = ?`, now, msg, kind)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE probe_sources SET sync_state = 'OK', last_sync_at = ?, last_success_at = ?,
		last_error = '', item_count = ? WHERE kind = ?`, now, now, items, kind)
	return err
}
