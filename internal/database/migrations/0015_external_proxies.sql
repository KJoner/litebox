-- 外部代理:不属于本面板、不由本面板部署的成品代理线路。
--
-- 与 nodes 是两类东西,只有「能被用户连」这一点相同:
--
--              自建节点(nodes)        外部代理(external_proxies)
--   机器归属    我们的                  别人的
--   有 SSH      有                      没有
--   配置来源    面板生成                上游给的
--   能否部署    能                      不能
--   能否统计流量 能                      不能
--
-- 因此绝不复用 nodes 表。nodes 的几乎每一列都假设「我们有 SSH 和 root」,
-- 混进去之后每一处查询都要判断「这行是不是真节点」,而判断写漏的表现是
-- 面板试图 SSH 到一个机场的服务器上去部署 —— 那是往别人家机器上发命令。

-- ---------- 订阅源(机场) ----------

CREATE TABLE proxy_sources (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT    NOT NULL,
    -- 订阅地址含 token,等同密码,必须加密。
    url_encrypted TEXT NOT NULL DEFAULT '',
    -- 条目展示名前缀。渲染时拼,不写进条目 —— 写进去的话改前缀要批量 UPDATE
    -- 几十行,漏掉几行的表现是同一批节点在客户端里有的带前缀有的不带。
    name_prefix TEXT NOT NULL DEFAULT '',

    -- 导入默认值:新条目继承这些,之后管理员可以单条覆盖。
    default_access_tier_id       INTEGER NOT NULL DEFAULT 1,
    default_subscription_enabled INTEGER NOT NULL DEFAULT 1,

    -- 自动同步默认关 —— 打开之前管理员应该先手工同步一次看看结果。
    auto_sync_enabled     INTEGER NOT NULL DEFAULT 0,
    sync_interval_minutes INTEGER NOT NULL DEFAULT 720
        CHECK (sync_interval_minutes >= 30),

    -- 机场账号到期。手工填,也可由 Subscription-Userinfo 的 expire 自动填,
    -- 但**手工值优先** —— 有些机场这个头填得不准,让它无声覆盖管理员填的
    -- 日期,会在到期那天出现「面板说还有 20 天」。
    expires_at TEXT,

    -- 上游给的数字。只在外部代理页展示,**绝不进 traffic_ledger,
    -- 不影响任何用户额度** —— 那是整个机场账号的总量,按我们的用户拆不开。
    upstream_used_bytes  INTEGER NOT NULL DEFAULT 0,
    upstream_total_bytes INTEGER NOT NULL DEFAULT 0,
    upstream_expires_at  TEXT,
    upstream_seen_at     TEXT,

    -- 上次同步结果。失败原因要留着 —— 只留一个 FAILED 状态的话,
    -- 管理员还得手工再点一次同步才知道是密码错了还是机场挂了。
    last_sync_at      TEXT,
    last_sync_status  TEXT NOT NULL DEFAULT 'NEVER'
        CHECK (last_sync_status IN ('NEVER', 'OK', 'FAILED')),
    last_sync_message TEXT    NOT NULL DEFAULT '',
    last_sync_added   INTEGER NOT NULL DEFAULT 0,
    last_sync_updated INTEGER NOT NULL DEFAULT 0,
    last_sync_missing INTEGER NOT NULL DEFAULT 0,
    last_sync_skipped INTEGER NOT NULL DEFAULT 0,
    -- 连续失败次数,达到阈值进仪表盘预警。成功时归零。
    consecutive_failures INTEGER NOT NULL DEFAULT 0,

    enabled    INTEGER NOT NULL DEFAULT 1,
    remark     TEXT    NOT NULL DEFAULT '',
    sort_order INTEGER NOT NULL DEFAULT 0,
    deleted_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- 部分唯一索引:软删除之后名字可以复用。
-- 与 nodes 的做法不同(那边靠给名字加删除后缀),这里是新表,直接用条件索引更干净。
CREATE UNIQUE INDEX idx_proxy_sources_name ON proxy_sources(name) WHERE deleted_at IS NULL;

-- ---------- 代理条目 ----------

CREATE TABLE external_proxies (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    -- NULL 表示手工添加。不为手工条目造一条内建 source —— 那要占一行、
    -- 要防删除、要在每个列表里过滤掉,全是为了「统一」付出的成本,
    -- 而「是不是从订阅来的」本来就是个真实区别(决定它会不会被同步覆盖)。
    --
    -- 外键**不带 ON DELETE CASCADE**:代理源用软删除,这个外键永远不会真的
    -- 触发;写上 CASCADE 只会让某天有人误用硬删除时,几十条配置连一句确认
    -- 都没有就消失了。删源时条目怎么办由管理员在界面上显式选。
    source_id INTEGER REFERENCES proxy_sources(id),

    -- name 是内部名称,唯一,删除确认时要输入的就是它;
    -- display_name 是发给用户的名字,**不含前缀**(前缀渲染时拼)。
    name         TEXT NOT NULL,
    display_name TEXT NOT NULL,
    -- 管理员改过的名字。非空时完全取代「前缀 + display_name」,不再加前缀。
    display_name_override TEXT NOT NULL DEFAULT '',
    -- 上游原始名称。同步匹配的二级键,也用于识别公告条目。
    raw_name TEXT NOT NULL DEFAULT '',

    protocol TEXT    NOT NULL,
    server   TEXT    NOT NULL,
    port     INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    -- 协议参数 JSON,含密码。别人家的账号,泄露等于送人,必须加密。
    params_encrypted TEXT NOT NULL,
    -- 原始分享链接。URI 格式的订阅优先原样透传它:机场可能用了本面板不认识的
    -- 参数(udp-over-tcp、plugin、各种私有扩展),按解析出的字段重新生成会把
    -- 它们悄悄丢掉 —— 而丢掉之后用户能连上、网页能开,只有 UDP 不通。
    raw_uri_encrypted TEXT NOT NULL DEFAULT '',

    access_tier_id       INTEGER NOT NULL DEFAULT 1,
    subscription_enabled INTEGER NOT NULL DEFAULT 1,
    sort_order           INTEGER NOT NULL DEFAULT 0,
    public_remark        TEXT    NOT NULL DEFAULT '',
    maintenance_message  TEXT    NOT NULL DEFAULT '',
    expires_at           TEXT,

    origin TEXT NOT NULL CHECK (origin IN ('MANUAL', 'IMPORTED')),
    -- 同步匹配的一级键 sha256(protocol|server|port)。
    -- **不含密码**:机场轮换密码时那仍然是同一个节点,含密码会被判成
    -- 「旧的消失 + 新的出现」,管理员配的展示名、等级、排序全丢。
    identity_key TEXT NOT NULL DEFAULT '',
    -- 管理员改过、同步时不得覆盖的字段,逗号分隔。
    -- server/port/凭据不可锁定 —— 锁住上游的事实等于故意保留一个连不上的地址。
    locked_fields TEXT NOT NULL DEFAULT '',

    -- 上游连续多少轮没出现。**不删除,也不立刻下架**:机场订阅接口抽风
    -- 返回部分列表是常事,一次抽风就抹掉用户订阅里的节点,下次同步又回来 ——
    -- 用户看到的是节点忽有忽无,而这个现象无法复现、无法排查。
    missing_rounds INTEGER NOT NULL DEFAULT 0,
    missing_since  TEXT,
    last_seen_at   TEXT,

    -- EXCLUDED 是「上游有但我不要」:导入时没勾选的条目仍然入库,
    -- 否则下次同步它们会作为新增再进来一遍,管理员每次都要重新排除。
    status TEXT NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE', 'DISABLED', 'EXCLUDED')),

    -- 手工触发的连通性检查结果。只测 TCP 可达,且是从**面板所在服务器**测的。
    last_check_at         TEXT,
    last_check_ok         INTEGER,
    last_check_message    TEXT NOT NULL DEFAULT '',
    last_check_latency_ms INTEGER,

    deleted_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE UNIQUE INDEX idx_external_proxies_name
    ON external_proxies(name) WHERE deleted_at IS NULL;
CREATE INDEX idx_external_proxies_source   ON external_proxies(source_id);
CREATE INDEX idx_external_proxies_identity ON external_proxies(source_id, identity_key);
CREATE INDEX idx_external_proxies_tier     ON external_proxies(access_tier_id);

-- ---------- 额外授权 ----------

-- 与 user_nodes(迁移 0001)逐字对应,包括 ON DELETE CASCADE 与第二列上的索引。
-- 对称是刻意的:管理员在两个页面之间切换时,「额外授权」不该需要重新学一遍。
CREATE TABLE user_external_proxies (
    proxy_user_id     INTEGER NOT NULL REFERENCES proxy_users(id)      ON DELETE CASCADE,
    external_proxy_id INTEGER NOT NULL REFERENCES external_proxies(id) ON DELETE CASCADE,
    created_at        TEXT    NOT NULL,
    PRIMARY KEY (proxy_user_id, external_proxy_id)
);
CREATE INDEX idx_user_external_proxies_proxy ON user_external_proxies(external_proxy_id);

-- ---------- 有效外部代理视图 ----------

-- 用户可用外部代理的唯一定义。订阅、门户、管理页三处查这张视图,
-- 谁都不再自己拼等级条件 —— 三处各写一份判断迟早会分叉,
-- 而分叉的表现是用户在订阅里看得到、连上去却不是给他的凭据。
--
-- **单独一张视图,不与 user_effective_nodes 合并**,三个理由:
--   1. 不得修改迁移 0007;
--   2. ID 空间会撞 —— nodes.id = 3 与 external_proxies.id = 3 是两个东西,
--      合进一张视图必须加一列类型标记,而所有既有查询都得跟着改;
--   3. 更要命的是下游:user_effective_nodes 还被**配置生成与部署脏标记**消费。
--      把外部代理混进去,部署协调器会收到一批根本不存在的「节点 ID」,
--      然后对它们发起 SSH 连接 —— 那是往别人家的机器上发命令。
--
-- 规则若要变更,必须新增迁移 DROP 后重建这个视图,不得改本文件。
CREATE VIEW user_effective_external_proxies AS
    SELECT u.id AS proxy_user_id, p.id AS external_proxy_id
      FROM proxy_users      u
      JOIN access_tiers     ut ON ut.id = u.access_tier_id
      JOIN external_proxies p  ON p.deleted_at IS NULL
      JOIN access_tiers     pt ON pt.id = p.access_tier_id
     WHERE u.deleted_at IS NULL
       AND pt.level <= ut.level
    UNION
    SELECT ep.proxy_user_id, ep.external_proxy_id
      FROM user_external_proxies ep
      JOIN proxy_users      u ON u.id = ep.proxy_user_id     AND u.deleted_at IS NULL
      JOIN external_proxies p ON p.id = ep.external_proxy_id AND p.deleted_at IS NULL;
