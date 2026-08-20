-- 访问等级从机器降到入口(V8.1)。
--
-- V8 刚把等级加到 node_inbounds 上时,它是在机器等级【之上再收一次】——
-- 两层交集。实际用下来那一层是多余的:一台机器上如果有 VIP 入口,
-- 它就该只对 VIP 开放;而「这台机器整体是 VIP」这句话,在多入站之后
-- 已经没有一个能落到实处的含义 —— 机器本身不接受任何连接,入口才接受。
--
-- 两层同时存在还有一个具体的坏处:管理员在机器上设了 VIP、又想让其中
-- 一个入口对所有人开放,做不到 —— 而他会以为把入口调成普通组就行了,
-- 结果那个入口谁都看不见,面板一个字都不说。
--
-- 于是 nodes.access_tier_id 就此冻结,不再有任何代码路径读或写它。
-- 与迁移 0019 冻结那十几列同一个处理:刻意不 DROP,SQLite 的 DROP COLUMN
-- 会重建整张表,而 nodes 是全库被引用最多的表。

-- ---------- 先把机器的等级搬到它的入口上 ----------
--
-- **这一步不能省。** 不搬的话,升级后一台原本 VIP 的机器,它的入口
-- 全是普通组(迁移 0019 刻意填的),于是那台机器对全体用户敞开 ——
-- 而管理员那边什么都没做过,面板也不会报任何错。
-- 权限的静默放大是这类改动里最坏的一种失败。
--
-- 只搬还停在普通组的入口:管理员如果已经显式给某个入口设过等级,
-- 那是更晚、更具体的意思表示,不能被机器上那个旧值盖掉。
--
-- 「机器是 VIP、入口是普通组」这个组合在 0019~0020 之间不可能出现:
-- 那时机器等级是第一道闸门,普通用户根本到不了 VIP 机器上的任何入口,
-- 把入口调成普通组也没有意义。所以 access_tier_id = 1 只可能是
-- 迁移 0019 填的那个默认值。
UPDATE node_inbounds
   SET access_tier_id = (SELECT n.access_tier_id FROM nodes n WHERE n.id = node_inbounds.node_id)
 WHERE access_tier_id = 1
   AND (SELECT n.access_tier_id FROM nodes n WHERE n.id = node_inbounds.node_id) != 1;

-- ---------- 重建两个视图 ----------
--
-- 顺序要紧:user_effective_nodes 现在建立在 user_effective_inbounds 之上
-- (与 0019 正好反过来),所以先拆后者依赖的那个,再按新的依赖方向建回去。
DROP VIEW user_effective_nodes;
DROP VIEW user_effective_inbounds;

-- 用户能用哪些入口 = 入口等级不高于他 ∪ 管理员对这台机器的额外授权。
--
-- **额外授权是整台机器,包括它上面全部入口。** 不这么定的话,
-- 管理员显式把一台机器授权给某个用户,而那台机器上只有 VIP 入口,
-- 授权就凭空作废了 —— 而面板一个字都不说。授权本来就是一个粗粒度的
-- 越权开关,它的意思就是「这台机器给他用」。
--
-- 规则若要变更,必须新增迁移 DROP 后重建,不得改本文件。
CREATE VIEW user_effective_inbounds AS
    -- 1. 等级继承
    SELECT u.id AS proxy_user_id, i.id AS inbound_id, i.node_id AS node_id
      FROM node_inbounds i
      JOIN nodes        n  ON n.id = i.node_id AND n.deleted_at IS NULL
      JOIN proxy_users  u  ON u.deleted_at IS NULL
      JOIN access_tiers ut ON ut.id = u.access_tier_id
      JOIN access_tiers it ON it.id = i.access_tier_id
     WHERE i.deleted_at IS NULL
       AND i.enabled = 1
       AND it.level <= ut.level
    UNION
    -- 2. user_nodes 里管理员对整台机器的额外授权
    SELECT un.proxy_user_id, i.id, i.node_id
      FROM user_nodes    un
      JOIN proxy_users   u ON u.id = un.proxy_user_id AND u.deleted_at IS NULL
      JOIN nodes         n ON n.id = un.node_id       AND n.deleted_at IS NULL
      JOIN node_inbounds i ON i.node_id = n.id
     WHERE i.deleted_at IS NULL
       AND i.enabled = 1;

-- 有效节点 = 「这个用户在这台机器上至少有一个能用的入口」。
--
-- 它不再是一条独立的规则,而是入口那一层的投影 —— 两处各写一遍的话,
-- 迟早会出现「机器在列表里、而他在上面一个凭据都没有」这种状态,
-- 那正是脏标记会去重启一台与他无关的机器的来源。
--
-- 中转角色的机器因此不在里面:它上面没有任何入站,用户拿不到它的任何凭据。
-- 中转线路的可见性由 user_effective_relays 单独管(那一层看的是落地入口)。
CREATE VIEW user_effective_nodes AS
    SELECT DISTINCT proxy_user_id, node_id FROM user_effective_inbounds;
