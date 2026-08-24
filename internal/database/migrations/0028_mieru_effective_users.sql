-- 只有 Mieru 入口的机器,对谁都是空的(V13 漏掉的)。
--
-- 迁移 0024 把 user_effective_mieru_inbounds 建成了
-- 「user_effective_nodes JOIN node_mieru_inbounds」。而 user_effective_nodes
-- 在迁移 0020 之后是 **user_effective_inbounds 的投影**,那一层只读
-- node_inbounds —— 也就是只认 sing-box 入站。
--
-- 于是一台**只有 Mieru 入口、没有任何 sing-box 入站**的机器,
-- 在任何用户的 user_effective_nodes 里都不存在,它上面的 Mieru 入口
-- 因此对**所有人**都查不出用户。而这正是这个功能最自然的用法:
-- 一台只跑 mita 的机器。
--
-- 三处同时静默失效,而且都不指向真正的原因:
--
--   下发   渲染出来的 mita 配置里 users 是空的。mita 照常接受 apply,
--          但 `mita start` 时报 `server mux listening failed: no user found`
--          —— 部署失败并回滚,而错误里一个字都没提"这台机器没有 sing-box 入口";
--   订阅   这个入口不进任何人的订阅,面板上却显示它是启用的、在订阅里;
--   门户   同上。
--
-- **生产上撞到了**,那台机器的管理员本来就不打算在上面装 sing-box。
--
-- 修法是让这个视图自己表达规则,不再借道 user_effective_nodes。
-- 两条规则与 user_effective_inbounds 那一层一字不差:
--
--   1. 等级继承:用户等级 >= 入口等级;
--   2. user_nodes 的整机授权**穿透入口等级** —— 它是机器级的,意思就是
--      「这台机器给他用」,包括它上面全部入口。不穿透的话,管理员显式把
--      一台机器授权给某个用户,而那台机器上只有 VIP 入口,授权就凭空作废了。
--
-- 按 CLAUDE.md 的规矩:规则变更必须新增迁移 DROP 后重建,不得改 0024。

DROP VIEW IF EXISTS user_effective_mieru_inbounds;

CREATE VIEW user_effective_mieru_inbounds AS
    -- 1. 等级继承
    SELECT u.id AS proxy_user_id, i.id AS mieru_inbound_id, i.node_id AS node_id
      FROM node_mieru_inbounds i
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
      FROM user_nodes          un
      JOIN proxy_users         u ON u.id = un.proxy_user_id AND u.deleted_at IS NULL
      JOIN nodes               n ON n.id = un.node_id       AND n.deleted_at IS NULL
      JOIN node_mieru_inbounds i ON i.node_id = n.id
     WHERE i.deleted_at IS NULL
       AND i.enabled = 1;
