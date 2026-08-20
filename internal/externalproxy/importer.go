package externalproxy

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MissingRoundsBeforeUnlist 是连续多少轮没在上游出现就自动退出订阅。
//
// **不是一次就下架**:机场的订阅接口抽风返回部分列表是常事(限流、
// CDN 缓存、后端故障)。一次抽风就把用户订阅里的节点抹掉,客户端那边
// 节点直接消失,下一次同步又回来 —— 用户看到的是节点忽有忽无,
// 而这个现象无法复现、无法排查。
//
// **永远不自动删除**:删掉就丢了管理员配的展示名、等级、排序与备注,
// 机场恢复之后全部要重配一遍。磁盘上留一行记录的成本是零。
const MissingRoundsBeforeUnlist = 3

// PreviewItem 是导入预览里的一行。
type PreviewItem struct {
	// IdentityKey 是预览与确认导入之间的稳定标识。
	// 不用下标:两次请求之间上游的列表可能变,按下标选会选错条目。
	IdentityKey string `json:"identity_key"`
	Name        string `json:"name"`
	Protocol    string `json:"protocol"`
	Server      string `json:"server"`
	Port        int    `json:"port"`
	Method      string `json:"method"`
	// Suggested 是默认勾选状态。疑似公告的条目默认不勾。
	Suggested bool `json:"suggested"`
	// Announcement 表示疑似公告条目。仍然列出 —— 规则一定会误伤。
	Announcement bool `json:"announcement"`
	// Existing 表示库里已经有这一条,导入时会走「更新」而不是「新增」。
	Existing bool `json:"existing"`
}

// SkippedGroup 是被跳过的一类协议及其条数。
type SkippedGroup struct {
	Protocol string `json:"protocol"`
	Label    string `json:"label"`
	Count    int    `json:"count"`
}

// PreviewResult 是一次导入预览的产物。
type PreviewResult struct {
	Format      string         `json:"format"`
	FormatLabel string         `json:"format_label"`
	Items       []PreviewItem  `json:"items"`
	Skipped     []SkippedGroup `json:"skipped"`
	// ParseErrors 是识别为 Shadowsocks 却解析失败的行。
	// 逐条列出而不是只报个数:管理员拿它去问机场客服时需要具体内容。
	ParseErrors []string `json:"parse_errors"`
	Upstream    *struct {
		UsedBytes  int64   `json:"used_bytes"`
		TotalBytes int64   `json:"total_bytes"`
		ExpiresAt  *string `json:"expires_at"`
	} `json:"upstream"`
}

// SyncResult 是一次同步的结果。
type SyncResult struct {
	Added     int `json:"added"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Missing   int `json:"missing"`
	Skipped   int `json:"skipped"`
	// Unlisted 是本次因连续消失达到阈值而自动退出订阅的条目展示名。
	// 要逐个列出:那是用户订阅里会少掉的东西,只报个数管理员无从核对。
	Unlisted     []string       `json:"unlisted"`
	SkippedByPro []SkippedGroup `json:"skipped_by_protocol"`
	ParseErrors  []string       `json:"parse_errors"`
	// Upstream 是这一次拉取时读到的 Subscription-Userinfo。
	// 随同步结果一起带回来,而不是再拉一次 —— 拉两次既浪费一个往返,
	// 也可能拿到两份不一致的数字(机场那边随时在变)。
	Upstream UpstreamInfo `json:"-"`
	Err      error        `json:"-"`
}

func (r SyncResult) Summary() string {
	parts := []string{
		fmt.Sprintf("新增 %d", r.Added),
		fmt.Sprintf("更新 %d", r.Updated),
		fmt.Sprintf("不变 %d", r.Unchanged),
	}
	if r.Missing > 0 {
		parts = append(parts, fmt.Sprintf("上游未出现 %d", r.Missing))
	}
	if len(r.Unlisted) > 0 {
		parts = append(parts, fmt.Sprintf("连续消失自动退出订阅 %d(%s)",
			len(r.Unlisted), strings.Join(r.Unlisted, "、")))
	}
	if r.Skipped > 0 {
		labels := make([]string, 0, len(r.SkippedByPro))
		for _, g := range r.SkippedByPro {
			labels = append(labels, fmt.Sprintf("%s %d 条", g.Label, g.Count))
		}
		parts = append(parts, fmt.Sprintf("跳过 %d(%s)", r.Skipped, strings.Join(labels, "、")))
	}
	return strings.Join(parts, ",")
}

// parsedBatch 是一批解析完的上游条目。
type parsedBatch struct {
	format   Format
	items    []Parsed
	skipped  []SkippedGroup
	skipN    int
	errs     []string
	upstream UpstreamInfo
}

// fetchAndParse 拉取并解析,是预览与同步共用的前半段。
//
// 共用而不是各写一遍:预览里看到的与同步真正会做的,必须是同一份解析结果。
// 分开写的话,某天改了解析规则只改到一处,表现是「预览说会导入 12 条,
// 实际进来 9 条」,而两边都不报错。
func (s *Store) fetchAndParse(ctx context.Context, f *Fetcher, url string) (parsedBatch, error) {
	res, err := f.Fetch(ctx, url)
	if err != nil {
		return parsedBatch{}, err
	}
	format, lines, err := DecodeBody(res.Body)
	if err != nil {
		return parsedBatch{format: format}, err
	}

	batch := parsedBatch{format: format, upstream: res.Upstream}
	skipCount := map[Protocol]int{}
	for _, line := range lines {
		parsed, err := ParseURI(line)
		if err != nil {
			if !parsed.Protocol.Supported() {
				// 本版本压根不收的种类(ssr:// 之类)按类型报数就够了。
				skipCount[parsed.Protocol]++
				batch.skipN++
				continue
			}
			// **支持的协议却解析不出来,要让管理员看见原文。**
			// 归进"按类型跳过"的话,报出来的是「跳过 3 条 VMess」——
			// 他会读成"这个面板不支持 VMess",然后不再管它,
			// 而真正的原因可能只是那三条链接的 base64 被截断了。
			batch.errs = append(batch.errs, truncate(line, 120)+" —— "+err.Error())
			continue
		}
		batch.items = append(batch.items, parsed)
	}

	// 按协议名排序,让同一份订阅每次给出相同顺序的报数。
	protocols := make([]Protocol, 0, len(skipCount))
	for p := range skipCount {
		protocols = append(protocols, p)
	}
	sort.Slice(protocols, func(i, j int) bool { return protocols[i] < protocols[j] })
	batch.skipped = make([]SkippedGroup, 0, len(protocols))
	for _, p := range protocols {
		batch.skipped = append(batch.skipped, SkippedGroup{
			Protocol: string(p), Label: p.Label(), Count: skipCount[p],
		})
	}
	return batch, nil
}

// Preview 拉取并解析订阅,但**不落库**。
func (s *Store) Preview(ctx context.Context, f *Fetcher, sourceID int64, url string) (PreviewResult, error) {
	batch, err := s.fetchAndParse(ctx, f, url)
	if err != nil {
		return PreviewResult{Format: string(batch.format), FormatLabel: batch.format.Label()}, err
	}

	existing, err := s.existingKeys(ctx, sourceID)
	if err != nil {
		return PreviewResult{}, err
	}

	out := PreviewResult{
		Format:      string(batch.format),
		FormatLabel: batch.format.Label(),
		Items:       make([]PreviewItem, 0, len(batch.items)),
		Skipped:     batch.skipped,
		// 空切片而不是 nil —— nil 序列化成 JSON null,前端拿它当数组用。
		ParseErrors: append([]string{}, batch.errs...),
	}
	for _, p := range batch.items {
		key := IdentityKey(p.Protocol, p.Server, p.Port)
		announce := LooksLikeAnnouncement(p.Name)
		out.Items = append(out.Items, PreviewItem{
			IdentityKey: key,
			Name:        p.Name,
			Protocol:    string(p.Protocol),
			Server:      p.Server,
			Port:        p.Port,
			Method:      p.Params.Method,
			// 疑似公告默认不勾,但仍然列出。
			Suggested:    !announce,
			Announcement: announce,
			Existing:     existing[key],
		})
	}
	if batch.upstream.Present {
		out.Upstream = &struct {
			UsedBytes  int64   `json:"used_bytes"`
			TotalBytes int64   `json:"total_bytes"`
			ExpiresAt  *string `json:"expires_at"`
		}{batch.upstream.Used, batch.upstream.Total, batch.upstream.ExpiresAt}
	}
	return out, nil
}

func (s *Store) existingKeys(ctx context.Context, sourceID int64) (map[string]bool, error) {
	out := map[string]bool{}
	if sourceID == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT identity_key FROM external_proxies WHERE source_id = ? AND deleted_at IS NULL`,
		sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// SyncOptions 控制一次同步的行为。
type SyncOptions struct {
	// Selected 非空时只导入这些 identity_key 的**新条目**,
	// 其余新条目入库为 EXCLUDED。用于首次导入的三步向导。
	//
	// 未勾选的条目仍然入库,而不是丢掉:不入库的话下次同步它们会作为
	// 「新增」再进来一遍,管理员每次同步都要重新排除一次。
	Selected map[string]bool
	// FirstImport 为真时才应用 Selected。日常同步不带选择集,
	// 所有新条目都按源的默认值入库。
	FirstImport bool
}

// Sync 拉取订阅源并把差异写回。
//
// 失败时**一条都不改**:拿不到数据时什么都不做,比按空数据去改状态安全得多。
// 这与「流量同步读取失败必须在进入事务前返回」是同一条道理。
func (s *Store) Sync(
	ctx context.Context, f *Fetcher, src *Source, opts SyncOptions,
) SyncResult {
	batch, err := s.fetchAndParse(ctx, f, src.URL)
	if err != nil {
		return SyncResult{Err: err}
	}

	current, err := s.List(ctx, ListFilter{SourceID: &src.ID, IncludeExcluded: true})
	if err != nil {
		return SyncResult{Err: err}
	}

	byKey := make(map[string]*Proxy, len(current))
	byRawName := make(map[string]*Proxy, len(current))
	for _, p := range current {
		byKey[p.IdentityKey] = p
		if p.RawName != "" {
			byRawName[p.RawName] = p
		}
	}

	result := SyncResult{
		SkippedByPro: batch.skipped,
		Skipped:      batch.skipN,
		ParseErrors:  append([]string{}, batch.errs...),
		Unlisted:     []string{},
		Upstream:     batch.upstream,
	}
	now := time.Now().UTC().Format(time.RFC3339)
	seen := map[int64]bool{}

	for idx, item := range batch.items {
		key := IdentityKey(item.Protocol, item.Server, item.Port)

		// 一级键匹配 identity_key(协议|地址|端口)。
		// 二级键匹配上游原始名 —— 救的是「机场换了域名」:
		// 那时 identity_key 变了,但名字通常不变。
		match := byKey[key]
		if match == nil && item.Name != "" {
			match = byRawName[item.Name]
		}

		if match != nil {
			seen[match.ID] = true
			changed, err := s.applyUpstream(ctx, match, item, key, now)
			if err != nil {
				return SyncResult{Err: err}
			}
			if changed {
				result.Updated++
			} else {
				result.Unchanged++
			}
			continue
		}

		if err := s.insertImported(ctx, src, item, key, idx, opts); err != nil {
			return SyncResult{Err: err}
		}
		result.Added++
	}

	// 上游没出现的条目:计数 +1,达到阈值才退出订阅,永远不删。
	for _, p := range current {
		if seen[p.ID] || p.Status == StatusExcluded {
			continue
		}
		result.Missing++
		unlisted, err := s.markMissing(ctx, p, now)
		if err != nil {
			return SyncResult{Err: err}
		}
		if unlisted {
			result.Unlisted = append(result.Unlisted, p.EffectiveDisplayName())
		}
	}

	return result
}

// applyUpstream 把上游的事实写回一条已存在的条目。
//
// server / port / 凭据 / raw_uri **一律覆盖**,不受锁定影响:
// 锁住上游的事实等于故意保留一个连不上的地址。
//
// 展示名受 locked_fields 保护 —— 那是同步唯一会写的可锁字段。
// 管理员改过的名字不该在第二天同步时被上游的名字盖回去,
// 而且他不会知道是同步干的。
func (s *Store) applyUpstream(
	ctx context.Context, old *Proxy, item Parsed, key, now string,
) (bool, error) {
	paramsJSON, err := item.Params.Marshal()
	if err != nil {
		return false, err
	}
	paramsEnc, err := s.cipher.Encrypt(paramsJSON)
	if err != nil {
		return false, err
	}
	rawURIEnc := ""
	if item.RawURI != "" {
		if rawURIEnc, err = s.cipher.Encrypt(item.RawURI); err != nil {
			return false, err
		}
	}

	locked := LockedSet(old.LockedFields)
	displayName := old.DisplayName
	if !locked[FieldDisplayName] && item.Name != "" {
		displayName = item.Name
	}

	// 加密是带随机 nonce 的,同一份明文两次加密的密文不同 ——
	// 不能拿密文比是否有变化,只能比解密后的值。
	changed := old.Server != item.Server || old.Port != item.Port ||
		!old.Params.Equal(item.Params) || old.RawURI != item.RawURI ||
		old.RawName != item.Name || old.DisplayName != displayName ||
		old.MissingRounds != 0

	_, err = s.db.ExecContext(ctx, `
		UPDATE external_proxies
		   SET server = ?, port = ?, params_encrypted = ?, raw_uri_encrypted = ?,
		       identity_key = ?, raw_name = ?, display_name = ?,
		       missing_rounds = 0, missing_since = NULL, last_seen_at = ?, updated_at = ?
		 WHERE id = ?`,
		item.Server, item.Port, paramsEnc, rawURIEnc, key, item.Name, displayName,
		now, now, old.ID)
	return changed, err
}

// insertImported 新增一条从上游来的条目。
func (s *Store) insertImported(
	ctx context.Context, src *Source, item Parsed, key string, position int, opts SyncOptions,
) error {
	name, err := s.uniqueName(ctx, src.Name, item.Name, item.Server, item.Port)
	if err != nil {
		return err
	}

	status := StatusActive
	subEnabled := src.DefaultSubscriptionEnable
	// 首次导入时没勾选的条目入库为 EXCLUDED —— 上游有但我不要。
	if opts.FirstImport && !opts.Selected[key] {
		status = StatusExcluded
		subEnabled = false
	}

	_, err = s.Create(ctx, CreateParams{
		SourceID:    &src.ID,
		Name:        name,
		DisplayName: item.Name,
		RawName:     item.Name,
		Protocol:    item.Protocol,
		Server:      item.Server,
		Port:        item.Port,
		Params:      item.Params,
		RawURI:      item.RawURI,
		// 新条目继承源的默认值,之后管理员可以单条覆盖。
		AccessTierID:        src.DefaultAccessTierID,
		SubscriptionEnabled: subEnabled,
		// 用上游的位置当排序值:机场通常按地区分好组,
		// 保留它比全塞 0 更有用。只在新增时写,同步不再动它 ——
		// 否则管理员调好的顺序会在下次同步时被打回原样。
		SortOrder: position,
		Origin:    OriginImported,
		Status:    status,
	})
	return err
}

// uniqueName 生成不冲突的内部名称。
//
// 上游的名字在不同源之间会重复(两个机场都有「香港01」),而内部名称
// 是全局唯一的、删除确认时要输入的那个。带上源名让它自然唯一,
// 也让管理员一眼看出这条来自哪个源。
func (s *Store) uniqueName(ctx context.Context, sourceName, rawName, server string, port int) (string, error) {
	base := strings.TrimSpace(rawName)
	if base == "" {
		base = fmt.Sprintf("%s:%d", server, port)
	}
	base = sourceName + "/" + base
	if len([]rune(base)) > 56 {
		base = string([]rune(base)[:56])
	}

	for i := 0; i < 100; i++ {
		candidate := base
		if i > 0 {
			candidate = base + "-" + strconv.Itoa(i+1)
		}
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM external_proxies WHERE name = ? AND deleted_at IS NULL`,
			candidate).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("为 %q 生成内部名称失败:同名条目过多", rawName)
}

// markMissing 记一次「上游没出现」。返回是否因此退出订阅。
func (s *Store) markMissing(ctx context.Context, p *Proxy, now string) (bool, error) {
	rounds := p.MissingRounds + 1
	since := p.MissingSince
	if since == nil || *since == "" {
		since = &now
	}

	// 达到阈值才退出订阅,且只退一次 —— 已经退掉的不再重复报。
	unlist := rounds >= MissingRoundsBeforeUnlist && p.SubscriptionEnabled
	subEnabled := p.SubscriptionEnabled && !unlist

	_, err := s.db.ExecContext(ctx, `
		UPDATE external_proxies
		   SET missing_rounds = ?, missing_since = ?, subscription_enabled = ?, updated_at = ?
		 WHERE id = ?`,
		rounds, since, subEnabled, now, p.ID)
	return unlist, err
}
