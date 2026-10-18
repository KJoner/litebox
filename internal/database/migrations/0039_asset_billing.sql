-- 节点资产卡片(P0-A / P0-B):套餐计费字段与资产标签。
--
-- 计费字段加在 V20 的 expiry_profiles 上,而不是另建一张「套餐」表 —— 那张表的主键
-- 已经是「对象种类 + id」,商家、到期、自动续费、提醒都在它上面;再建一张会出现两个
-- 「编辑套餐 / 续费」入口,而两份数据迟早分叉。适用对象仍然只有三类:自建节点、
-- 外部代理、代理源。**LiteBox 对外生成的订阅不在其中**:那是我们发出去的东西,
-- 不是一笔采购成本,kind 的 CHECK 里本来就没有它。
--
-- 全部是 ADD COLUMN + DEFAULT:存量行的新列都是「未设置」,旧节点照常显示,
-- 到期提醒的行为一个字不变。
--
-- 剩余未摊销价值、日均 / 月均成本都是派生量,不入库 —— 读取时按当前时间现算。

-- 计费周期。空串 = 未设置 / 未知(界面上显示「未设置」,不猜)。
--   MONTHLY / QUARTERLY / SEMIANNUAL / YEARLY  按日历月推进(1 / 3 / 6 / 12 个月)
--   CUSTOM        cycle_count + cycle_unit(MONTH 或 DAY)
--   ONE_TIME      一次性买断,有到期日就按到期日摊销
--   TRAFFIC_PACK  流量包,按流量估算;有到期日时另外按时间估算,两种口径分开列
--   PERMANENT     永久 / 无到期,不做时间摊销
ALTER TABLE expiry_profiles ADD COLUMN plan_name TEXT NOT NULL DEFAULT '';
ALTER TABLE expiry_profiles ADD COLUMN billing_cycle TEXT NOT NULL DEFAULT ''
    CHECK (billing_cycle IN ('', 'MONTHLY', 'QUARTERLY', 'SEMIANNUAL', 'YEARLY',
                             'CUSTOM', 'ONE_TIME', 'TRAFFIC_PACK', 'PERMANENT'));
ALTER TABLE expiry_profiles ADD COLUMN cycle_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE expiry_profiles ADD COLUMN cycle_unit TEXT NOT NULL DEFAULT ''
    CHECK (cycle_unit IN ('', 'MONTH', 'DAY'));
-- ISO 4217 三位大写,空串 = 未填。多币种不换汇、不求和,所以币种跟着每一笔金额走。
ALTER TABLE expiry_profiles ADD COLUMN currency TEXT NOT NULL DEFAULT '';
-- 金额一律存最小货币单位的整数(分),不存浮点。-1 = 未填;0 = 免费。
-- price_minor 是套餐标价(每个周期);period_paid_minor 是【本次】实际支付,-1 表示与标价相同。
ALTER TABLE expiry_profiles ADD COLUMN price_minor INTEGER NOT NULL DEFAULT -1;
ALTER TABLE expiry_profiles ADD COLUMN purchased_at TEXT NOT NULL DEFAULT '';
-- 本次已付费周期的起点;空串表示按「到期时间往前推一个计费周期」现算。
ALTER TABLE expiry_profiles ADD COLUMN period_start TEXT NOT NULL DEFAULT '';
ALTER TABLE expiry_profiles ADD COLUMN period_paid_minor INTEGER NOT NULL DEFAULT -1;
-- 提前续费时上一周期还没用完的部分(按续费那一刻的估算值)结转进本期,与本次支付一起摊销。
ALTER TABLE expiry_profiles ADD COLUMN period_carry_minor INTEGER NOT NULL DEFAULT 0;
-- 管理员登记的「不限流量」。为真时不做按流量估算(没有分母)。
ALTER TABLE expiry_profiles ADD COLUMN traffic_unlimited INTEGER NOT NULL DEFAULT 0;

-- 续费历史记下实际支付:金额(-1 = 没记)、币种,以及这一笔建立的已付费周期从哪天起。
-- 历史只追加不覆盖;幂等仍靠 request_id 的部分唯一索引。
ALTER TABLE expiry_renewals ADD COLUMN amount_minor INTEGER NOT NULL DEFAULT -1;
ALTER TABLE expiry_renewals ADD COLUMN currency TEXT NOT NULL DEFAULT '';
ALTER TABLE expiry_renewals ADD COLUMN period_start TEXT NOT NULL DEFAULT '';

-- 资产标签:地区与标签,只用于仪表盘卡片的筛选与展示。
--
-- **刻意不放进 expiry_profiles**:外部代理一旦有了自己的档案行,就不再跟随来源的
-- 到期信息(expiry.Resolve)。只想给它打个「香港」标签,却让它悄悄脱离了来源的到期
-- 与提醒 —— 那是一个没有任何提示的副作用。
CREATE TABLE asset_labels (
    kind       TEXT    NOT NULL CHECK (kind IN ('NODE', 'EXTERNAL_PROXY', 'PROXY_SOURCE')),
    object_id  INTEGER NOT NULL,
    -- ISO 3166-1 alpha-2 大写(HK / JP / US…),空串 = 未设置。不按名字猜地区。
    region     TEXT    NOT NULL DEFAULT '',
    -- 逗号分隔,已去重、去空白,最多 8 个,每个最长 16 字符。
    tags       TEXT    NOT NULL DEFAULT '',
    updated_at TEXT    NOT NULL,
    PRIMARY KEY (kind, object_id)
);
