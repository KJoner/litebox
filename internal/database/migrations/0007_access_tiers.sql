-- 节点访问等级。
--
-- 与管理角色是两件互不相干的事:admin_users 决定"能否进后台",
-- access_tiers 决定"代理用户能用哪些节点"。ROOT 只是能用到全部节点的
-- 代理用户等级,不带任何后台权限 —— 管理员要自用订阅,应当单独建一个
-- ROOT 等级的代理用户,而不是让 admin_users 兼任代理用户。

CREATE TABLE access_tiers (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    -- code 是程序内引用等级的唯一稳定标识。name 允许管理员改,不能拿它做判断。
    code        TEXT    NOT NULL UNIQUE,
    name        TEXT    NOT NULL,
    -- level 决定继承关系:节点 level <= 用户 level 即可用。
    -- 留出间隔(10/20/30)是为了以后插入 SVIP 之类的等级时不必重排。
    level       INTEGER NOT NULL UNIQUE,
    description TEXT    NOT NULL DEFAULT '',
    sort_order  INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT    NOT NULL,
    updated_at  TEXT    NOT NULL
);

INSERT INTO access_tiers (id, code, name, level, description, sort_order, created_at, updated_at)
VALUES
    (1, 'normal', '普通组',  10, '可使用普通节点',                 10,
     strftime('%Y-%m-%dT%H:%M:%SZ','now'), strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    (2, 'vip',    'VIP组',   20, '可使用普通节点与 VIP 节点',      20,
     strftime('%Y-%m-%dT%H:%M:%SZ','now'), strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    (3, 'root',   'ROOT组',  30, '可使用全部节点',                 30,
     strftime('%Y-%m-%dT%H:%M:%SZ','now'), strftime('%Y-%m-%dT%H:%M:%SZ','now'));

-- 存量用户与存量节点一律落在 NORMAL(id=1):升级后可用节点集合与升级前
-- 完全一致,不会有人在管理员不知情的情况下多拿到或少拿到节点。
--
-- 这两列刻意不写 REFERENCES:SQLite 的 ALTER TABLE ADD COLUMN 在
-- foreign_keys 打开时要求带外键的新列默认值必须是 NULL,而这里必须
-- NOT NULL DEFAULT 1 才能让存量行直接落到普通组。完整性改由
-- user_effective_nodes 视图的 JOIN 与 Go 侧校验保证。
ALTER TABLE proxy_users ADD COLUMN access_tier_id INTEGER NOT NULL DEFAULT 1;
ALTER TABLE nodes       ADD COLUMN access_tier_id INTEGER NOT NULL DEFAULT 1;

CREATE INDEX idx_proxy_users_tier ON proxy_users(access_tier_id);
CREATE INDEX idx_nodes_tier       ON nodes(access_tier_id);

-- 用户可用节点的唯一定义。
--
-- 配置生成、订阅生成、用户门户、管理后台与部署脏标记全部查这张视图,
-- 谁都不再自己拼等级条件 —— 四处各写一份判断,迟早会出现"订阅里有、
-- 节点配置里没有"这种用户能看到却连不上的组合,而且不报任何错。
--
-- 两条来源做并集:
--   1. 等级继承 —— 节点等级不高于用户等级;
--   2. user_nodes 额外授权 —— 管理员给单个用户单独追加的节点。
-- V2 不做"从组里排除某个节点",确有需要时再加排除关系,不提前复杂化。
--
-- 规则若要变更,必须新增迁移 DROP 后重建这个视图,不得改本文件。
CREATE VIEW user_effective_nodes AS
    SELECT u.id AS proxy_user_id, n.id AS node_id
      FROM proxy_users u
      JOIN access_tiers ut ON ut.id = u.access_tier_id
      JOIN nodes        n  ON n.deleted_at IS NULL
      JOIN access_tiers nt ON nt.id = n.access_tier_id
     WHERE u.deleted_at IS NULL
       AND nt.level <= ut.level
    UNION
    SELECT un.proxy_user_id, un.node_id
      FROM user_nodes un
      JOIN proxy_users u ON u.id = un.proxy_user_id AND u.deleted_at IS NULL
      JOIN nodes       n ON n.id = un.node_id       AND n.deleted_at IS NULL;
