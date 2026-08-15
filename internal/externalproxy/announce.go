package externalproxy

import "strings"

// announceKeywords 用来识别「伪装成节点的公告条目」。
//
// 机场订阅里前几条经常是这种东西,server 随便填一个地址:
//
//	剩余流量:100.5 GB
//	距离下次重置剩余:15 天
//	套餐到期:2026-09-01
//	官网:https://example.com
//
// 全量导入会让每个用户的客户端里多出几条**永远连不上的「节点」**,
// 而他们只会来问「香港那几个怎么连不上」。
//
// 这份表**只影响预览里的默认勾选状态,不影响是否列出** ——
// 规则一定会误伤(真有机场把节点叫「官网直连」),
// 所以全部条目照常列出,由管理员自己决定。
var announceKeywords = []string{
	"剩余流量", "剩余", "距离", "重置", "到期", "过期", "套餐", "官网",
	"订阅", "群组", "邮箱", "客服", "教程", "网址", "续费", "购买",
	"expire", "expiry", "traffic", "reset", "official", "website",
	"telegram", "tg频道", "tg群", "www.", "http://", "https://",
}

// LooksLikeAnnouncement 判断一个上游条目名是否像公告而非节点。
func LooksLikeAnnouncement(rawName string) bool {
	name := strings.ToLower(strings.TrimSpace(rawName))
	if name == "" {
		return false
	}
	for _, kw := range announceKeywords {
		if strings.Contains(name, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// AnnounceKeywords 返回关键词表的副本,供接口展示与测试用。
func AnnounceKeywords() []string { return append([]string(nil), announceKeywords...) }
