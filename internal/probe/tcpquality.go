package probe

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/litebox/litebox/internal/netprobe"
)

// TcpQuality 的节点列表(getNodes?format=tsv)。
//
// 只解析,不执行任何东西:那个项目的脚本拿列表去跑 nping,我们只取「地区、运营商、域名、IP、端口、
// 地址族与备用节点」这几列,每一列都按自己的规则重新校验 —— 外部接口给的任何一个地址都不直接信。
// 首版只收 type = cdn(即 Zstatic 的三网省级节点):别的类型(教育网、对象存储、Telegram、Apple CDN)
// 是第三方业务站点,不是专门给人测速的节点,只按类型报数、不提供批量导入。
const TcpQualityNodesURL = "https://tcpquality.ibsgss.uk/getNodes?format=tsv&scope=cdn"

// tcpQualityColumns 是这份 TSV 的表头。列名或列数变了就整份拒绝:按位置去猜,
// 会把 IP 当成端口、把备用节点当成主节点,而且不会报任何错。
var tcpQualityColumns = []string{"type", "family", "province", "isp", "host", "ip", "port", "target",
	"backup_host", "backup_ip", "backup_port", "backup_target"}

// tcpQualityTypeNames 是不支持的类型给人看的名字。
var tcpQualityTypeNames = map[string]string{
	"cernet": "教育网", "cernet2": "教育网 IPv6", "tos": "对象存储", "telegram": "Telegram",
	"applecdn": "Apple CDN",
}

// TcpQualityTypeName 返回一个上游类型给人看的名字。
func TcpQualityTypeName(t string) string {
	if n, ok := tcpQualityTypeNames[t]; ok {
		return n
	}
	return cleanName(t, 16)
}

// checkUpstreamIP 校验上游给的 IP:字面量、地址族对得上、不是受限地址。
func checkUpstreamIP(raw string, f Family) (string, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("IP %q 不合法", cleanName(raw, 40))
	}
	ip = ip.Unmap()
	if ip.Is4() != (f == FamilyV4) {
		return "", fmt.Errorf("IP %s 与地址族不符", ip)
	}
	if reason := netprobe.RestrictedAddr(ip); reason != "" {
		return "", fmt.Errorf("IP %s 是%s", ip, reason)
	}
	return ip.String(), nil
}

// ParseTcpQuality 解析 getNodes 的 TSV。返回 CDN 类的候选、被拒的条目,以及按类型统计的不支持条目数。
func ParseTcpQuality(body []byte) ([]UpstreamItem, []Rejected, map[string]int, error) {
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, nil, nil, errors.New("节点列表是空的")
	}
	if len(lines)-1 > maxUpstreamItems {
		return nil, nil, nil, fmt.Errorf("节点列表有 %d 行,超过上限 %d", len(lines)-1, maxUpstreamItems)
	}
	header := strings.Split(lines[0], "\t")
	if len(header) != len(tcpQualityColumns) {
		return nil, nil, nil, fmt.Errorf("表头有 %d 列,预期 %d 列:上游格式变了,整份拒绝", len(header), len(tcpQualityColumns))
	}
	for i, c := range tcpQualityColumns {
		if strings.TrimSpace(header[i]) != c {
			return nil, nil, nil, fmt.Errorf("第 %d 列是 %q,预期 %q:上游格式变了,整份拒绝", i+1, cleanName(header[i], 24), c)
		}
	}
	var items []UpstreamItem
	var rejected []Rejected
	skipped := map[string]int{}
	for n, line := range lines[1:] {
		lineNo := n + 2
		if strings.TrimSpace(line) == "" {
			continue
		}
		col := strings.Split(line, "\t")
		bad := func(reason string) {
			rejected = append(rejected, Rejected{Line: lineNo, Text: cleanName(strings.ReplaceAll(line, "\t", " "), 100), Reason: reason})
		}
		if len(col) != len(tcpQualityColumns) {
			bad(fmt.Sprintf("有 %d 列,预期 %d 列", len(col), len(tcpQualityColumns)))
			continue
		}
		for i := range col {
			col[i] = strings.TrimSpace(col[i])
		}
		typ := col[0]
		if typ != "cdn" {
			skipped[TcpQualityTypeName(typ)]++
			continue
		}
		var f Family
		switch col[1] {
		case "4":
			f = FamilyV4
		case "6":
			f = FamilyV6
		default:
			bad("地址族只能是 4 或 6")
			continue
		}
		prov, ok := ProvinceByName(col[2])
		if !ok {
			bad("不认识的省份 " + cleanName(col[2], 16))
			continue
		}
		isp, ok := ParseISP(col[3])
		if !ok || (isp != ISPTelecom && isp != ISPUnicom && isp != ISPMobile) {
			bad("CDN 类只收电信 / 联通 / 移动,不认识 " + cleanName(col[3], 16))
			continue
		}
		target := strings.ToLower(col[7])
		if target != ZstaticProvinceHost(prov.Code, isp, f) {
			bad("域名与省份、运营商、地址族对不上(预期 " + ZstaticProvinceHost(prov.Code, isp, f) + ")")
			continue
		}
		ip, err := checkUpstreamIP(col[5], f)
		if err != nil {
			bad(err.Error())
			continue
		}
		port, err := checkPort(col[6])
		if err != nil {
			bad(err.Error())
			continue
		}
		key := fmt.Sprintf("cdn:%s:%s:%s", familyDigit(f), strings.ToLower(prov.Code), strings.ToLower(isp))
		items = append(items, UpstreamItem{
			Key: key, Name: prov.Name + ISPName(isp), Host: target, Port: port, Family: f,
			Region: prov.Region, Province: prov.Name, ISP: isp, Role: "PRIMARY", Level: "CDN", IP: ip,
			Upstream: map[string]string{"target": target, "ip": ip, "port": col[6], "host": cleanName(col[4], 80)},
		})
		// 备用节点:域名必须仍是 Zstatic 的节点;没有备用 IP 时只能按域名导入。
		if col[11] == "" {
			continue
		}
		btarget := strings.ToLower(col[11])
		if !zstaticHostRe.MatchString(btarget) {
			bad("备用节点的域名不是 Zstatic 的节点:" + cleanName(btarget, 60))
			continue
		}
		bport, err := checkPort(col[10])
		if err != nil {
			bad("备用节点:" + err.Error())
			continue
		}
		bip := ""
		if col[9] != "" {
			if bip, err = checkUpstreamIP(col[9], f); err != nil {
				bad("备用节点:" + err.Error())
				continue
			}
		}
		items = append(items, UpstreamItem{
			Key: key + ":backup", Name: prov.Name + ISPName(isp) + "(备用)", Host: btarget, Port: bport, Family: f,
			Region: prov.Region, Province: prov.Name, ISP: isp, Role: "BACKUP", Level: "CDN", IP: bip,
			Upstream: map[string]string{"target": btarget, "ip": bip, "port": col[10], "host": cleanName(col[8], 80)},
		})
	}
	if len(items) == 0 && len(rejected) == 0 {
		return nil, nil, skipped, errors.New("节点列表里没有 CDN 类目标")
	}
	return dedupeItems(items, &rejected), rejected, skipped, nil
}
