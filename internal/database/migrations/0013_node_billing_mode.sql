-- 节点流量额度的计费口径。
--
-- sing-box 的 uplink/downlink 计的是**客户端↔节点这一段**的双向字节,
-- 而一次用户下载在节点网卡上要走两趟:节点从源站收 1 份(入),再发给客户端
-- 1 份(出)。于是:
--
--   VPS 只计出站(egress)  ≈ sing-box 计数 × 1
--   VPS 进出合计(双向)     ≈ sing-box 计数 × 2
--
-- 两种口径在各家 VPS 里都常见,甚至同一家不同套餐都不一样,所以不能写死成
-- 某一个倍数:一律 ×2 会让按出站计费的机器高报一倍,额度还剩一半就报红。
--
-- 默认 EGRESS(倍数 1),存量节点的显示与告警与升级前一个字节都不差。
ALTER TABLE nodes ADD COLUMN traffic_billing_mode TEXT NOT NULL DEFAULT 'EGRESS'
    CHECK (traffic_billing_mode IN ('EGRESS', 'BOTH'));
