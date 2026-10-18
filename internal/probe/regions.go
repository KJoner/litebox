package probe

import "strings"

// Province 是一个省级行政区。代码取 ISO 3166-2:CN(小写之后就是 Zstatic 节点域名的第一段),
// 大区按通行的七大地理分区。这张表是照公开标准写的,不是从哪个脚本里抄来的。
type Province struct {
	Code   string `json:"code"`
	Name   string `json:"name"`
	Region string `json:"region"`
	// Common 是预置弹窗里默认勾上的「常用地区」。
	Common bool `json:"common"`
}

// Provinces 是中国大陆 31 个省级行政区(不含港澳台:第三方目录里没有它们的三网节点)。
var Provinces = []Province{
	{"BJ", "北京", "华北", true}, {"TJ", "天津", "华北", false}, {"HE", "河北", "华北", false},
	{"SX", "山西", "华北", false}, {"NM", "内蒙古", "华北", false},
	{"LN", "辽宁", "东北", false}, {"JL", "吉林", "东北", false}, {"HL", "黑龙江", "东北", false},
	{"SH", "上海", "华东", true}, {"JS", "江苏", "华东", false}, {"ZJ", "浙江", "华东", false},
	{"AH", "安徽", "华东", false}, {"FJ", "福建", "华东", false}, {"JX", "江西", "华东", false},
	{"SD", "山东", "华东", false},
	{"HA", "河南", "华中", false}, {"HB", "湖北", "华中", false}, {"HN", "湖南", "华中", false},
	{"GD", "广东", "华南", true}, {"GX", "广西", "华南", false}, {"HI", "海南", "华南", false},
	{"CQ", "重庆", "西南", false}, {"SC", "四川", "西南", false}, {"GZ", "贵州", "西南", false},
	{"YN", "云南", "西南", false}, {"XZ", "西藏", "西南", false},
	{"SN", "陕西", "西北", false}, {"GS", "甘肃", "西北", false}, {"QH", "青海", "西北", false},
	{"NX", "宁夏", "西北", false}, {"XJ", "新疆", "西北", false},
}

// ProvinceByCode 按代码(大小写都行)找省份。
func ProvinceByCode(code string) (Province, bool) {
	code = strings.ToUpper(strings.TrimSpace(code))
	for _, p := range Provinces {
		if p.Code == code {
			return p, true
		}
	}
	return Province{}, false
}

// ProvinceByName 按名字找省份。「北京市」「广西壮族自治区」这类全称也认。
func ProvinceByName(name string) (Province, bool) {
	name = strings.TrimSpace(name)
	for _, p := range Provinces {
		if name == p.Name || strings.HasPrefix(name, p.Name) {
			return p, true
		}
	}
	return Province{}, false
}

// 运营商代码。只收这几个:筛选维度要可枚举,随手写的「电信CN2」「CT」「ChinaTelecom」会让
// 同一个运营商在筛选里变成三项。
const (
	ISPTelecom = "CT"
	ISPUnicom  = "CU"
	ISPMobile  = "CM"
	ISPCernet  = "CERNET"
	ISPOther   = "OTHER"
)

// ISPName 是运营商给人看的名字。
func ISPName(code string) string {
	switch code {
	case ISPTelecom:
		return "电信"
	case ISPUnicom:
		return "联通"
	case ISPMobile:
		return "移动"
	case ISPCernet:
		return "教育网"
	case ISPOther:
		return "其他"
	}
	return ""
}

// ParseISP 认运营商的几种写法(代码、中文名、Zstatic 目录里的英文),认不出返回 false。
func ParseISP(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "", true
	case "ct", "电信", "telecom", "chinatelecom":
		return ISPTelecom, true
	case "cu", "联通", "unicom", "chinaunicom":
		return ISPUnicom, true
	case "cm", "移动", "mobile", "chinamobile":
		return ISPMobile, true
	case "cernet", "教育网", "cernet2":
		return ISPCernet, true
	case "other", "其他":
		return ISPOther, true
	}
	return "", false
}
