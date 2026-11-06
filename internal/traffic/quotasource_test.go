package traffic

import (
	"testing"
	"time"
)

// 节点额度切到网卡实测:分界之前那一段仍按代理折算(账本 × 口径倍数),之后取网卡小时桶;
// 两段分开给,数据质量如实报(TRAFFIC03 / 07、A21)。
func TestNodeQuotaHostNICSegments(t *testing.T) {
	env := newCycleEnv(t)
	id := env.addNode(t, "nic", 0, "NONE", 1)
	since := "2026-03-10T10:30:00Z"
	if _, err := env.db.Exec(`UPDATE nodes SET traffic_quota_source = 'HOST_NIC', traffic_quota_source_since = ?,
		traffic_billing_mode = 'BOTH' WHERE id = ?`, since, id); err != nil {
		t.Fatal(err)
	}
	// 分界之前与之后各有一笔代理流量;之后那笔不该再算进去(那一段交给网卡)。
	env.addLedger(t, id, "user_000001", "downlink", 1000, "2026-03-01T00:00:00Z")
	env.addLedger(t, id, "user_000001", "downlink", 9999, "2026-03-11T00:00:00Z")
	// 网卡小时桶:10:00 那一桶跨着分界,不计;11:00 起计。
	for _, b := range []struct {
		at     string
		rx, tx int64
	}{{"2026-03-10T10:00:00Z", 7, 7}, {"2026-03-10T11:00:00Z", 100, 300}, {"2026-03-11T00:00:00Z", 10, 30}} {
		ts, _ := time.Parse(time.RFC3339, b.at)
		if _, err := env.db.Exec(`INSERT INTO host_traffic (node_id, granularity, bucket_ts, rx_bytes, tx_bytes, updated_at)
			VALUES (?, 'HOUR', ?, ?, ?, ?)`, id, ts.Unix(), b.rx, b.tx, b.at); err != nil {
			t.Fatal(err)
		}
	}
	// 从没成功同步过:质量是 NO_DATA,数字照给,不补、不平滑。
	if _, err := env.db.Exec(`INSERT INTO host_traffic_state (node_id, installed, iface, updated_at) VALUES (?, 1, 'eth0', ?)`,
		id, since); err != nil {
		t.Fatal(err)
	}
	u := env.usage(t, id)
	if u.QuotaSource != SourceHostNIC || u.QuotaSourceSince != since {
		t.Fatalf("来源 = %s / %s", u.QuotaSource, u.QuotaSourceSince)
	}
	if u.EstimateBytes != 2000 { // 1000 × BOTH(2)
		t.Errorf("代理折算段 = %d,期望 2000(分界之后的账本不该算进来)", u.EstimateBytes)
	}
	if u.HostBytes != 440 || u.HostRxBytes != 110 || u.HostTxBytes != 330 {
		t.Errorf("网卡段 = %d(rx %d tx %d),期望 440", u.HostBytes, u.HostRxBytes, u.HostTxBytes)
	}
	if u.UsedBytes != 2440 {
		t.Errorf("合计 = %d", u.UsedBytes)
	}
	if u.Quality != QualityNoData {
		t.Errorf("质量 = %s,期望 NO_DATA", u.Quality)
	}
	// EGRESS 只算网卡出站。
	if _, err := env.db.Exec(`UPDATE nodes SET traffic_billing_mode = 'EGRESS' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE host_traffic_state SET synced_at = ? WHERE node_id = ?`,
		time.Now().UTC().Format(time.RFC3339), id); err != nil {
		t.Fatal(err)
	}
	u = env.usage(t, id)
	if u.HostBytes != 330 || u.EstimateBytes != 1000 || u.Quality != QualityOK {
		t.Errorf("EGRESS:网卡段 %d、折算段 %d、质量 %s", u.HostBytes, u.EstimateBytes, u.Quality)
	}
}

// 代理折算的机器(存量默认)一个字节都不变:来源标明是代理折算,没有网卡段。
func TestNodeQuotaDefaultIsProxyEstimate(t *testing.T) {
	env := newCycleEnv(t)
	id := env.addNode(t, "est", 0, "NONE", 1)
	env.addLedger(t, id, "user_000001", "uplink", 500, "2026-02-01T00:00:00Z")
	u := env.usage(t, id)
	if u.QuotaSource != SourceProxyEstimate || u.UsedBytes != 500 || u.HostBytes != 0 || u.EstimateBytes != 500 {
		t.Fatalf("默认 = %+v", u)
	}
}
