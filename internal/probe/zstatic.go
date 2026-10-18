package probe

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/litebox/litebox/internal/netprobe"
)

// Zstatic CDN 的三网节点。
//
// 省级节点的域名是 {省份代码}-{运营商}-v{4|6}.ip.zstaticcdn.com(NetQuality 的三网 TCP 测试就是这样拼的),
// 端口 80;这一类在本地生成,不用联网。市级节点没有规则可推,只能从目录接口读,端口按记录(目前是 443),
// 不假定所有目标都是同一个端口。目录接口只有 IPv4 的记录。
const (
	ZstaticDomainSuffix  = ".ip.zstaticcdn.com"
	ZstaticDirectoryURL  = "https://www.zstaticcdn.com/api/v1/DescribeAllNodes"
	ZstaticProvincePort  = 80
	zstaticMaxCityLength = 32
)

// UpstreamItem 是来源给出的一个候选目标(预置生成的,或者从上游列表解析出来的)。
// 它只是候选:进不进目标库由管理员在预览里勾选。
type UpstreamItem struct {
	// Key 是它在来源里的身份(写进 source_key)。
	Key      string `json:"key"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Family   Family `json:"family"`
	Region   string `json:"region"`
	Province string `json:"province"`
	City     string `json:"city"`
	ISP      string `json:"isp"`
	Role     string `json:"role"`
	// Level 是 PROVINCE / CITY(Zstatic)或 CDN(TcpQuality),给预览分组用。
	Level string `json:"level"`
	// IP 是上游给的固定地址(TcpQuality 有,Zstatic 没有)。导入时可以选「按域名」或「按上游 IP」。
	IP string `json:"ip"`
	// Upstream 是上游原始字段的快照(存进 upstream 列,同步时比对用)。
	Upstream map[string]string `json:"upstream"`
}

var (
	zstaticProvinceRe = regexp.MustCompile(`^([a-z]{2})-(ct|cu|cm)-v([46])\.ip\.zstaticcdn\.com:(\d{1,5})$`)
	zstaticCityRe     = regexp.MustCompile(`^([a-z]{2})-([a-z0-9]{1,32})-(ct|cu|cm)-v4\.ip\.zstaticcdn\.com:(\d{1,5})$`)
	// zstaticHostRe 是任意一个 Zstatic 节点域名(备用节点、导入时的校验用)。
	zstaticHostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.ip\.zstaticcdn\.com$`)
)

// ZstaticProvinceHost 是一个省级节点的域名。
func ZstaticProvinceHost(code, isp string, family Family) string {
	v := "4"
	if family == FamilyV6 {
		v = "6"
	}
	return fmt.Sprintf("%s-%s-v%s%s", strings.ToLower(code), strings.ToLower(isp), v, ZstaticDomainSuffix)
}

func familyDigit(f Family) string {
	if f == FamilyV6 {
		return "6"
	}
	return "4"
}

func zstaticProvinceItem(p Province, isp string, family Family, port int) UpstreamItem {
	host := ZstaticProvinceHost(p.Code, isp, family)
	return UpstreamItem{
		Key:  fmt.Sprintf("prov:%s:%s:%s", strings.ToLower(p.Code), strings.ToLower(isp), familyDigit(family)),
		Name: p.Name + ISPName(isp), Host: host, Port: port, Family: family,
		Region: p.Region, Province: p.Name, ISP: isp, Role: "PRIMARY", Level: "PROVINCE",
		Upstream: map[string]string{"host": host, "port": strconv.Itoa(port)},
	}
}

// ZstaticProvinceItems 在本地生成省级节点候选。codes / isps / families 为空表示全部。
func ZstaticProvinceItems(codes, isps []string, families []Family) ([]UpstreamItem, error) {
	pick := map[string]bool{}
	for _, c := range codes {
		p, ok := ProvinceByCode(c)
		if !ok {
			return nil, fmt.Errorf("不认识的省份代码 %q", c)
		}
		pick[p.Code] = true
	}
	if len(isps) == 0 {
		isps = []string{ISPTelecom, ISPUnicom, ISPMobile}
	}
	for i, raw := range isps {
		isp, ok := ParseISP(raw)
		if !ok || (isp != ISPTelecom && isp != ISPUnicom && isp != ISPMobile) {
			return nil, fmt.Errorf("Zstatic 只有电信 / 联通 / 移动三网节点,%q 认不出", raw)
		}
		isps[i] = isp
	}
	if len(families) == 0 {
		families = []Family{FamilyV4}
	}
	for _, f := range families {
		if f != FamilyV4 && f != FamilyV6 {
			return nil, fmt.Errorf("不认识的地址族 %q", f)
		}
	}
	var out []UpstreamItem
	for _, p := range Provinces {
		if len(pick) > 0 && !pick[p.Code] {
			continue
		}
		for _, f := range families {
			for _, isp := range isps {
				out = append(out, zstaticProvinceItem(p, isp, f, ZstaticProvincePort))
			}
		}
	}
	return out, nil
}

// zstaticItemFromKey 由一个省级候选的 key 重新生成它(导入时不信任页面传回来的主机与端口)。
func zstaticItemFromKey(key string) (UpstreamItem, bool) {
	parts := strings.Split(key, ":")
	if len(parts) != 4 || parts[0] != "prov" {
		return UpstreamItem{}, false
	}
	p, ok := ProvinceByCode(parts[1])
	if !ok {
		return UpstreamItem{}, false
	}
	isp, ok := ParseISP(parts[2])
	if !ok || (isp != ISPTelecom && isp != ISPUnicom && isp != ISPMobile) {
		return UpstreamItem{}, false
	}
	var f Family
	switch parts[3] {
	case "4":
		f = FamilyV4
	case "6":
		f = FamilyV6
	default:
		return UpstreamItem{}, false
	}
	return zstaticProvinceItem(p, isp, f, ZstaticProvincePort), true
}

// Rejected 是上游列表里被拒掉的一条与原因。
type Rejected struct {
	Line   int    `json:"line"`
	Text   string `json:"text"`
	Reason string `json:"reason"`
}

// zstaticCarrier 把目录里的运营商英文名翻成代码。
func zstaticCarrier(raw string) (string, bool) {
	switch raw {
	case "telecom":
		return ISPTelecom, true
	case "unicom":
		return ISPUnicom, true
	case "mobile":
		return ISPMobile, true
	}
	return "", false
}

// checkPort 校验上游给的端口:范围、受限端口。
func checkPort(raw string) (int, error) {
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("端口 %q 不合法", raw)
	}
	if reason := netprobe.RestrictedPort(port); reason != "" {
		return 0, errors.New(reason)
	}
	return port, nil
}

// cleanName 清洗上游给的名字:去掉控制字符、截断长度。名字里塞一个换行会把页面与审计日志搞乱。
func cleanName(raw string, max int) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	s := b.String()
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// ParseZstaticDirectory 解析目录接口的 JSON。每一条都要过校验:域名必须是 Zstatic 节点的形状、
// 省份与运营商要与域名里的对得上、端口不能是受限端口。不认的一条进 rejected,不让整份失败。
func ParseZstaticDirectory(body []byte) ([]UpstreamItem, []Rejected, error) {
	var doc struct {
		UpdatedAt     string `json:"UpdatedAt"`
		ProvinceNodes []struct {
			Province string            `json:"province"`
			Carriers map[string]string `json:"carriers"`
		} `json:"ProvinceNodes"`
		CityNodes []struct {
			City     string `json:"city"`
			Province string `json:"province"`
			Carrier  string `json:"carrier"`
			Endpoint string `json:"endpoint"`
		} `json:"CityNodes"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, nil, errors.New("目录接口返回的不是预期的 JSON")
	}
	if len(doc.ProvinceNodes)+len(doc.CityNodes) == 0 {
		return nil, nil, errors.New("目录接口返回了空列表(没有 ProvinceNodes / CityNodes)")
	}
	if len(doc.ProvinceNodes)+len(doc.CityNodes) > maxUpstreamItems {
		return nil, nil, fmt.Errorf("目录接口返回了 %d 条,超过上限 %d", len(doc.ProvinceNodes)+len(doc.CityNodes), maxUpstreamItems)
	}
	var items []UpstreamItem
	var rejected []Rejected
	line := 0
	for _, pn := range doc.ProvinceNodes {
		prov, okProv := ProvinceByName(pn.Province)
		carriers := make([]string, 0, len(pn.Carriers))
		for c := range pn.Carriers {
			carriers = append(carriers, c)
		}
		sort.Strings(carriers)
		for _, c := range carriers {
			line++
			ep := pn.Carriers[c]
			bad := func(reason string) {
				rejected = append(rejected, Rejected{Line: line, Text: cleanName(ep, 80), Reason: reason})
			}
			isp, ok := zstaticCarrier(c)
			if !ok {
				bad("不认识的运营商 " + cleanName(c, 16))
				continue
			}
			m := zstaticProvinceRe.FindStringSubmatch(ep)
			if m == nil {
				bad("不是 Zstatic 省级节点的域名形状")
				continue
			}
			if !okProv || strings.ToLower(prov.Code) != m[1] || strings.ToLower(isp) != m[2] {
				bad("域名里的省份或运营商与记录对不上")
				continue
			}
			port, err := checkPort(m[4])
			if err != nil {
				bad(err.Error())
				continue
			}
			f := FamilyV4
			if m[3] == "6" {
				f = FamilyV6
			}
			it := zstaticProvinceItem(prov, isp, f, port)
			items = append(items, it)
		}
	}
	for _, cn := range doc.CityNodes {
		line++
		bad := func(reason string) {
			rejected = append(rejected, Rejected{Line: line, Text: cleanName(cn.Endpoint, 80), Reason: reason})
		}
		isp, ok := zstaticCarrier(cn.Carrier)
		if !ok {
			bad("不认识的运营商 " + cleanName(cn.Carrier, 16))
			continue
		}
		m := zstaticCityRe.FindStringSubmatch(cn.Endpoint)
		if m == nil {
			bad("不是 Zstatic 市级节点的域名形状")
			continue
		}
		prov, okProv := ProvinceByName(cn.Province)
		if !okProv || strings.ToLower(prov.Code) != m[1] || strings.ToLower(isp) != m[3] {
			bad("域名里的省份或运营商与记录对不上")
			continue
		}
		city := cleanName(cn.City, zstaticMaxCityLength)
		if city == "" {
			bad("没有城市名")
			continue
		}
		port, err := checkPort(m[4])
		if err != nil {
			bad(err.Error())
			continue
		}
		host := strings.TrimSuffix(cn.Endpoint, ":"+m[4])
		items = append(items, UpstreamItem{
			Key:  fmt.Sprintf("city:%s-%s-%s:4", m[1], m[2], m[3]),
			Name: prov.Name + "·" + city + ISPName(isp), Host: host, Port: port, Family: FamilyV4,
			Region: prov.Region, Province: prov.Name, City: city, ISP: isp, Role: "PRIMARY", Level: "CITY",
			Upstream: map[string]string{"host": host, "port": strconv.Itoa(port), "city": city},
		})
	}
	return dedupeItems(items, &rejected), rejected, nil
}

// maxUpstreamItems 是一次从上游读进来的条数上限。
const maxUpstreamItems = 2000

// dedupeItems 去掉 key 重复的条目(上游列表里同一个节点出现两次时只留第一条)。
func dedupeItems(items []UpstreamItem, rejected *[]Rejected) []UpstreamItem {
	seen := map[string]bool{}
	out := make([]UpstreamItem, 0, len(items))
	for _, it := range items {
		if seen[it.Key] {
			*rejected = append(*rejected, Rejected{Text: it.Host, Reason: "与前面的条目重复"})
			continue
		}
		seen[it.Key] = true
		out = append(out, it)
	}
	return out
}
