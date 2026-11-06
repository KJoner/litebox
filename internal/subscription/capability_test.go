package subscription

import "testing"

// 矩阵的客户端列是现算的:REALITY 在 Surge 上一定是「不支持」并带原因,VLESS / Trojan 在 sing-box 与 mihomo 上
// 一定是「支持且验证过」—— 改了序列化器,这里跟着变,不会出现矩阵与真实下发对不上。
func TestCapabilityMatrixIsComputed(t *testing.T) {
	rows := CapabilityMatrix()
	by := map[string]CapabilityRow{}
	for _, r := range rows {
		by[r.Key] = r
		for _, c := range CapabilityColumns {
			if _, ok := r.Columns[c.Key]; !ok {
				t.Errorf("%s 缺少列 %s", r.Key, c.Key)
			}
		}
	}
	tj := by["TROJAN"].Columns
	if !tj["selfhost"].OK || !tj["sing-box"].OK || !tj["sing-box"].Verified || !tj["clash"].OK || tj["surge"].OK || tj["surge"].Note == "" {
		t.Fatalf("Trojan 行 = %+v", tj)
	}
	if tj["chain_landing"].OK {
		t.Fatal("Trojan 这一版不能当链式落地")
	}
	if by["EXT_HYSTERIA2"].Columns["outbound"].OK {
		t.Fatal("节点上的精简 sing-box 拨不动 QUIC")
	}
	if by["SS_LEGACY"].Columns["quanx"].OK != true || by["SS2022"].Columns["quanx"].OK {
		t.Fatalf("Quantumult X:传统 AEAD 应当可写、SS2022 应当跳过,得到 %+v / %+v", by["SS_LEGACY"].Columns["quanx"], by["SS2022"].Columns["quanx"])
	}
	if by["SNELL_SHARED"].Columns["metering"].OK {
		t.Fatal("共享 Snell 没有分用户流量")
	}
}
