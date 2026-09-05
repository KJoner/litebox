package aliyun

import (
	"context"
	"errors"
	"sort"
)

// CDT(云数据传输)那一侧。
//
// **计数器是账号级 × 业务区域的,不是实例级的。** ListCdtInternetTraffic 返回的是
// 这个阿里云账号在每个 BusinessRegionId 下本月累计的公网流量,里面没有实例维度。
// 同一个账号下两台实例共用同一个池子 —— 所以阈值属于账号,动作才属于节点。
//
// 这个接口不在官方 SDK 里,也不在 OpenAPI 门户的元数据里。下面的主机名、版本与
// RegionId 是参考项目 CDT-Monitor 在真机上跑着的那一组,V17 技术验证再对一遍。

const (
	cdtHost    = "cdt.aliyuncs.com"
	cdtVersion = "2021-08-13"
	// cdtRegion 是签名参数里必须带的 RegionId。CDT 的入口是全局的,
	// 这个值只是让 RPC 签名有东西可填 —— 参考项目固定用 cn-hongkong。
	cdtRegion = "cn-hongkong"
)

// TrafficClass 是 CDT 免费额度的两个池子。
type TrafficClass string

const (
	// ClassInternational 国际区域(含中国香港),国际站账号每月 200 GB 免费。
	ClassInternational TrafficClass = "INTL"
	// ClassChina 中国内地区域,每月 20 GB 免费。
	ClassChina TrafficClass = "CN"
)

// ClassOf 按区域 ID 归类。规则与参考项目一致,也与阿里云免费额度的分法一致:
// `cn-*` 归内地,**除了 cn-hongkong** —— 香港在阿里云的定价里算国际区域。
func ClassOf(regionID string) TrafficClass {
	if len(regionID) >= 3 && regionID[:3] == "cn-" && regionID != "cn-hongkong" {
		return ClassChina
	}
	return ClassInternational
}

// Label 是给人看的类名。
func (c TrafficClass) Label() string {
	if c == ClassChina {
		return "中国内地"
	}
	return "国际 / 港澳台"
}

// RegionTraffic 是一个业务区域本月的累计公网流量。
type RegionTraffic struct {
	BusinessRegionID string `json:"business_region_id"`
	Bytes            int64  `json:"bytes"`
}

// ErrNoTrafficDetails 表示响应里没有 TrafficDetails —— 接口形状变了,
// 或者这个账号根本没开通 CDT。两种都要人看,所以不当成"用量为 0"。
var ErrNoTrafficDetails = errors.New("CDT 响应里没有 TrafficDetails:接口形状变了,或者这个账号没有开通 CDT")

// ListCdtInternetTraffic 查本月各业务区域的累计公网流量。
//
// 返回按区域 ID 排好序的列表;空列表**不是**错误 —— 一个刚开通、这个月还没
// 产生流量的账号就是这样。区分「没有条目」与「没有这个字段」:后者是 ErrNoTrafficDetails。
func (c *Client) ListCdtInternetTraffic(ctx context.Context, creds Credentials) ([]RegionTraffic, error) {
	result, err := c.call(ctx, creds, cdtHost, cdtRegion, cdtVersion, "ListCdtInternetTraffic", nil)
	if err != nil {
		return nil, err
	}
	// 参考项目见过两种形状:顶层 TrafficDetails,以及套在 Data 下面。
	raw, ok := result["TrafficDetails"]
	if !ok {
		if data, isMap := result["Data"].(map[string]any); isMap {
			raw, ok = data["TrafficDetails"]
		}
	}
	if !ok {
		return nil, ErrNoTrafficDetails
	}
	items := sliceOf(raw)
	out := make([]RegionTraffic, 0, len(items))
	for _, it := range items {
		obj, ok := it.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, RegionTraffic{
			BusinessRegionID: stringOf(obj["BusinessRegionId"]),
			Bytes:            int64Of(obj["Traffic"]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BusinessRegionID < out[j].BusinessRegionID })
	return out, nil
}

// SumByClass 把各区域的量并成两个池子。两个键**总是**存在,没有流量的那个是 0 ——
// 调用方按类取值时不必再判断"有没有这个键"。
func SumByClass(list []RegionTraffic) map[TrafficClass]int64 {
	out := map[TrafficClass]int64{ClassInternational: 0, ClassChina: 0}
	for _, r := range list {
		out[ClassOf(r.BusinessRegionID)] += r.Bytes
	}
	return out
}
