-- 流量基线加一个「来源」维度(V13)。
--
-- 在此之前 node_counters 的主键是 (node_id, user_code, direction),
-- 那在「一台机器上只有一个 sing-box 进程」时成立。Mieru 之后不成立了:
-- 同一个用户在同一台机器上可能同时有
--
--   * 一个(或多个)sing-box 入站  —— 计数在 sing-box 进程里,
--   * 一个或多个 mita 实例        —— **每个实例一个独立的进程与计数器**。
--
-- 三份计数器都是"这个用户在这台机器上的字节数",但它们**各自独立地
-- 重启归零**。共用一行基线的后果是两种,都不报错:
--
--   把三份加起来存一行:sing-box 重启时总和下跌,重启判定(靠
--   GetSysStats.Uptime)会把基线清零 —— 而 mita 那两份并没有重启,
--   于是它们已经入过账的累计值会被**再计一遍**。用户凭空多出一大截用量。
--
--   只存其中一份:另外两份的增量永远算不出来,那些流量静默丢失。
--
-- 所以基线必须按来源分开。source 的取值:
--
--   ''            sing-box 的 V2Ray Stats(存量数据全是它,所以默认空串)
--   'mieru:<id>'  某个 Mieru 入口对应的 mita 实例
--
-- 用入口 id 而不是笼统的 'MIERU':一台机器上可以有好几个实例,
-- 它们同样各自重启 —— 合成一个来源就回到了上面那个"加起来存一行"的问题。
--
-- ---------- 为什么必须重建表 ----------
--
-- SQLite 改不了主键,只能建新表再搬。**搬的时候一行都不能丢** ——
-- node_counters 是纯缓存没错,但丢了它不是"少算一轮":下一次同步会把
-- 计数器的**全部累计值**当成新增流量记进去,用户凭空多出他这台机器上
-- 有史以来的总用量。所以这里是 INSERT ... SELECT 全量搬,不是 DROP 重建。
--
-- traffic_ledger 不动。它的唯一索引是 (batch_id, node_id, user_code, direction),
-- 而 sing-box 与每个 mita 实例各自用一个 batch_id 同步 —— 不同批次之间
-- 本来就不会撞。给它也加一列的话,那张表是全站写入量最大的一张,
-- 加一列要重建它,而收益只是"看流水时能一眼看出来源",
-- 那个信息从 counter_value 与同批次的其他行里也读得出来。

CREATE TABLE node_counters_new (
    node_id    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    user_code  TEXT    NOT NULL,
    direction  TEXT    NOT NULL CHECK (direction IN ('uplink','downlink')),
    -- source 空串表示 sing-box 的 V2Ray Stats。
    --
    -- 默认空串而不是 'SINGBOX':存量行搬过来时那一列就是空的,
    -- 而空串与"还没被填过"在这里是同一件事 —— 给它一个非空的默认值
    -- 只会让搬迁语句多一次 UPDATE,换不来任何东西。
    source     TEXT    NOT NULL DEFAULT '',
    last_value INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT    NOT NULL,
    PRIMARY KEY (node_id, user_code, direction, source)
);

INSERT INTO node_counters_new (node_id, user_code, direction, source, last_value, updated_at)
SELECT node_id, user_code, direction, '', last_value, updated_at FROM node_counters;

DROP TABLE node_counters;
ALTER TABLE node_counters_new RENAME TO node_counters;
