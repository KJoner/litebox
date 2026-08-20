-- sing-box 多入站(V8)。
--
-- V4 的迁移 0014 写着「协议是节点级属性:一个节点一个入站」,理由是
-- 「多入站会让流量统计的归属变成一个需要重新设计的问题」。这一版推翻它,
-- 而那个问题的答案是:**根本不需要重新设计**。
--
-- V2Ray Stats 的用户计数器名是 user>>>{code}>>>traffic>>>uplink,
-- 里面【没有入站维度】—— 同一个用户在同一台机器上的流量,无论从哪个入站
-- 进来都记在同一个计数器上。而 traffic_ledger 的口径正是「某用户在某节点上
-- 用了多少」,所以多入站之后它一个字都不用改,数字也一个字节都不会错。
--
-- 真正付出的代价是另一件事:**入站级的用户流量永远拿不到**。
-- 「user_000003 在 8443 那个 SS 入口上用了多少」这个问题,多入站之后
-- 无法回答,而且不是"暂时没做",是计数器里没有这个维度。
-- 入站级的【总量】倒是有(inbound>>>tag>>>traffic>>>uplink),
-- 但那是另一条采集链路,这一版不做。
--
-- 结构上的变化:nodes 上那十几列原本在描述「这一个入站」,现在整体搬进
-- node_inbounds,存量节点迁移成一行。**nodes 上的原列就此冻结** ——
-- 不再有任何代码路径读或写它们。刻意不 DROP:SQLite 的 DROP COLUMN 会重建
-- 整张表,而 nodes 是全库被引用最多的表;留着也让这次升级可以人工比对。
--
-- 为什么不做成「nodes 上的是主入站,新表里的是附加入站」:那样每一处
-- 查询都要把两个来源并起来,而两个来源迟早分叉 —— 分叉的表现是订阅里
-- 看得到某个入口、节点上却没有它,或者反过来,全链路不报任何错。
-- 与外部代理「绝不复用 nodes 表」是同一条道理的另一面:那边是两种东西
-- 不能挤进一张表,这边是同一种东西不能分在两个地方。

-- ---------- 入站表 ----------

CREATE TABLE node_inbounds (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,

    -- sing-box 配置里的 inbound.tag,**一经分配不可更改**。
    --
    -- V7 之前 tag 是按协议现算的(vless-in / ss-in),那在一个节点一个入站时
    -- 成立;多入站下同一台机器上两个 VLESS 入站会撞成同一个 tag,而
    -- sing-box 对重名 tag 的表现是后面那个覆盖前面那个,check 不报错。
    --
    -- 存库还带来一个额外的好处:改协议不再改 tag,入站级统计计数器
    -- 因此是连续的 —— 按协议现算的话,把一个入站从 VLESS 改成 SS,
    -- 计数器名会跟着变,历史曲线在那一刻断掉而没有任何提示。
    --
    -- 存量行填 vless-in / ss-in:渲染结果必须与升级前【逐字节相同】,
    -- 否则十几台机器同时被判成「需要部署」,而那次重启换不来任何配置变化,
    -- 只会踢掉全部在线连接。新建的入站用 in-<id>,与协议无关。
    tag TEXT NOT NULL,

    -- 订阅与门户里显示的名字。与 node_relays 一样,**刻意没有内部名称字段**。
    display_name TEXT NOT NULL,

    protocol TEXT NOT NULL DEFAULT 'VLESS_REALITY'
        CHECK (protocol IN ('VLESS_REALITY', 'SHADOWSOCKS')),

    -- listen_port 是 sing-box 在这台机器上实际监听的端口;
    -- public_port 是客户端连接的公网端口,0 表示跟随 listen_port;
    -- ipv6_public_port 是 IPv6 条目在订阅里用的公网端口,0 表示跟随 public_port。
    --
    -- 三者的关系与 nodes 上原来的 listen_port / proxy_port / ipv6_proxy_port
    -- 一模一样,理由也一样:把公网端口写进节点配置会让 sing-box 监听在
    -- 转发链路另一端的号码上,check、服务状态、端口监听检查全部通过,
    -- 只有用户连不上。0 要原样留着,解析放在订阅生成时。
    listen_port      INTEGER NOT NULL CHECK (listen_port BETWEEN 1 AND 65535),
    public_port      INTEGER NOT NULL DEFAULT 0
        CHECK (public_port = 0 OR public_port BETWEEN 1 AND 65535),
    ipv6_public_port INTEGER NOT NULL DEFAULT 0
        CHECK (ipv6_public_port = 0 OR ipv6_public_port BETWEEN 1 AND 65535),

    -- TFO 从节点级降到入站级。它本来就只影响一个入站的监听选项,
    -- 放在节点上只是因为当时节点只有一个入站。
    tcp_fast_open INTEGER NOT NULL DEFAULT 0,

    -- VLESS + REALITY 专有。握手目标必须逐入站实测 —— 同一台机器上的两个
    -- REALITY 入站完全可以指向不同的握手目标,而 8192 字节记录上限是
    -- 目标域名的属性,不是机器的属性。
    reality_dest              TEXT    NOT NULL DEFAULT '',
    reality_dest_port         INTEGER NOT NULL DEFAULT 443,
    reality_privkey_encrypted TEXT    NOT NULL DEFAULT '',
    reality_pubkey            TEXT    NOT NULL DEFAULT '',
    reality_short_id          TEXT    NOT NULL DEFAULT '',
    handshake_max_record_size INTEGER NOT NULL DEFAULT 0,
    handshake_checked_at      TEXT,

    -- Shadowsocks 2022 专有。只允许 2022 系列三种,与迁移 0014 同。
    ss_method TEXT NOT NULL DEFAULT ''
        CHECK (ss_method IN ('', '2022-blake3-aes-128-gcm',
                             '2022-blake3-aes-256-gcm',
                             '2022-blake3-chacha20-poly1305')),
    ss_password_encrypted TEXT NOT NULL DEFAULT '',

    -- 链式出站从节点级降到入站级:同一台机器上的两个入站可以走两个不同的出口。
    --
    -- 落地指向的是【一个入站】而不是一个节点 —— 一台机器上有两个入站时,
    -- 「转发到 B」是有歧义的,而歧义的表现是流量进了管理员没打算用的那个入口
    -- (协议、端口、甚至等级都不同),没有任何一层会报错。
    chain_target_kind TEXT NOT NULL DEFAULT ''
        CHECK (chain_target_kind IN ('', 'INBOUND', 'EXTERNAL')),
    chain_target_inbound_id  INTEGER,
    chain_target_external_id INTEGER,

    -- 链路凭据。含义与迁移 0018 的 nodes.chain_* 完全一致,只是主体从
    -- 节点变成入站:它作为一个用户出现在【落地入站】的 users 与 stats.users 里。
    chain_code                  TEXT NOT NULL DEFAULT '',
    chain_uuid_encrypted        TEXT NOT NULL DEFAULT '',
    chain_ss_password_encrypted TEXT NOT NULL DEFAULT '',

    -- 入站自己的访问等级,在节点等级之上【再收一次】。
    --
    -- 存量行一律填 1(普通组)而不是继承节点的等级 —— 继承会让
    -- user_nodes 的额外授权失效:管理员显式把一台 VIP 机器授权给普通用户,
    -- 而那台机器上唯一的入站等级是 VIP,于是这个用户在节点视图里有、
    -- 在入站视图里没有,授权凭空作废而面板一个字都不说。
    -- 填最低档 = 这一层对存量数据完全透明。
    --
    -- 没有外键,与 nodes.access_tier_id 一致(access.Store.Validate 是唯一拦截点)。
    access_tier_id INTEGER NOT NULL DEFAULT 1,

    sort_order           INTEGER NOT NULL DEFAULT 0,
    subscription_enabled INTEGER NOT NULL DEFAULT 1,
    public_remark        TEXT    NOT NULL DEFAULT '',
    -- 关掉后这个入站不再渲染进 sing-box 配置(与软删除不同:行还留着,
    -- 重新打开不用重配等级、排序与握手目标)。
    enabled              INTEGER NOT NULL DEFAULT 1,

    -- 节点上【当前正在生效】的三项,只在部署成功时写入。
    --
    -- deployed_protocol 为空串表示「这个入站还没真正上过节点」,
    -- 订阅据此过滤 —— 节点级的 deployed_config_sha256 答不了这个问题:
    -- 一台部署过很多次的机器上,刚加的那个入站仍然还不存在。
    deployed_protocol      TEXT    NOT NULL DEFAULT '',
    deployed_ss_method     TEXT    NOT NULL DEFAULT '',
    deployed_tcp_fast_open INTEGER NOT NULL DEFAULT 0,

    deleted_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    -- 表级约束必须排在全部列定义之后(SQLite 一旦开始表约束就不能再回到列)。
    CHECK (
        (chain_target_kind = ''         AND chain_target_inbound_id  IS NULL
                                        AND chain_target_external_id IS NULL) OR
        (chain_target_kind = 'INBOUND'  AND chain_target_inbound_id  IS NOT NULL
                                        AND chain_target_external_id IS NULL) OR
        (chain_target_kind = 'EXTERNAL' AND chain_target_external_id IS NOT NULL
                                        AND chain_target_inbound_id  IS NULL)
    )
);

-- 同一台机器上监听端口不能重复,否则第二个入站 bind 失败,
-- sing-box 整个起不来 —— 而部署要到健康检查才发现,那时配置已经换过去了。
CREATE UNIQUE INDEX idx_node_inbounds_listen ON node_inbounds(node_id, listen_port)
    WHERE deleted_at IS NULL;
-- tag 的唯一性【不带 deleted_at 过滤】:软删除的入站在下一次部署之前
-- 仍然留在节点上,而它的入站级统计计数器也还在。让新入站抢到同一个 tag,
-- 两段互不相干的历史会接在一条曲线上。空串是插入过程中的中间态(见 tag 列的说明)。
CREATE UNIQUE INDEX idx_node_inbounds_tag ON node_inbounds(node_id, tag)
    WHERE tag != '';
CREATE INDEX idx_node_inbounds_node ON node_inbounds(node_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_node_inbounds_chain_inbound  ON node_inbounds(chain_target_inbound_id)
    WHERE chain_target_kind = 'INBOUND';
CREATE INDEX idx_node_inbounds_chain_external ON node_inbounds(chain_target_external_id)
    WHERE chain_target_kind = 'EXTERNAL';

-- ---------- 存量节点迁移成一行 ----------

-- 包含软删除的节点:它们的入站行同样打上 deleted_at。漏掉它们的话,
-- 下面 node_relays 那个 CHECK 会在「转发规则指向一台已删除的落地」时
-- 拿不到 target_inbound_id 而整条迁移失败 —— 而那种数据完全合法
-- (删节点时会先释放中转关系,但历史上的软删除记录仍然指着它)。
--
-- RELAY 角色不产生入站行:那台机器上根本没有 sing-box 配置。
INSERT INTO node_inbounds (
    node_id, tag, display_name, protocol,
    listen_port, public_port, ipv6_public_port, tcp_fast_open,
    reality_dest, reality_dest_port, reality_privkey_encrypted,
    reality_pubkey, reality_short_id,
    handshake_max_record_size, handshake_checked_at,
    ss_method, ss_password_encrypted,
    chain_target_kind, chain_target_inbound_id, chain_target_external_id,
    chain_code, chain_uuid_encrypted, chain_ss_password_encrypted,
    access_tier_id, sort_order, subscription_enabled, public_remark, enabled,
    deployed_protocol, deployed_ss_method, deployed_tcp_fast_open,
    deleted_at, created_at, updated_at)
SELECT
    n.id,
    CASE WHEN n.protocol = 'SHADOWSOCKS' THEN 'ss-in' ELSE 'vless-in' END,
    n.display_name,
    n.protocol,
    n.listen_port, n.proxy_port, n.ipv6_proxy_port, n.tcp_fast_open,
    n.reality_dest, n.reality_dest_port, n.reality_privkey_encrypted,
    n.reality_pubkey, n.reality_short_id,
    n.handshake_max_record_size, n.handshake_checked_at,
    n.ss_method, n.ss_password_encrypted,
    -- 链式去向先一律留空,下面两条 UPDATE 再补。
    --
    -- 不能在这里就写 'INBOUND':指向哪一行要等全部入站行都插进来之后才知道
    -- (落地可能是本次 SELECT 里排在后面的那台机器),而表级 CHECK 要求
    -- kind 与目标列同时成立 —— 先写 kind、后补目标会当场违约,整条迁移失败。
    '', NULL, NULL,
    n.chain_code, n.chain_uuid_encrypted, n.chain_ss_password_encrypted,
    -- 等级填最低档而不是继承 n.access_tier_id,见上面 access_tier_id 的说明。
    1,
    0, n.subscription_enabled, '', 1,
    n.deployed_protocol, n.deployed_ss_method, n.deployed_tcp_fast_open,
    n.deleted_at, n.created_at, n.updated_at
  FROM nodes n
 WHERE n.role = 'LANDING';

-- 链式落地由「那个节点」改成「那个节点唯一的入站」。
-- 此刻每个节点恰好一行,所以这个子查询没有歧义。
--
-- kind 与目标列必须在同一条 UPDATE 里一起写,否则表级 CHECK 当场违约。
UPDATE node_inbounds
   SET chain_target_kind = 'INBOUND',
       chain_target_inbound_id = (
        SELECT t.id FROM node_inbounds t
         WHERE t.node_id = (SELECT n.chain_target_node_id
                              FROM nodes n WHERE n.id = node_inbounds.node_id))
 WHERE node_id IN (SELECT id FROM nodes WHERE chain_target_kind = 'NODE');

-- 落地是外部代理的那些,目标 id 原样搬过来。
UPDATE node_inbounds
   SET chain_target_kind = 'EXTERNAL',
       chain_target_external_id = (
        SELECT n.chain_target_external_id FROM nodes n WHERE n.id = node_inbounds.node_id)
 WHERE node_id IN (SELECT id FROM nodes WHERE chain_target_kind = 'EXTERNAL');

-- ---------- 转发规则的落地改成指向入站 ----------

-- 视图引用 node_relays,重建表之前必须先拆掉。
DROP VIEW user_effective_relays;

CREATE TABLE node_relays_new (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    display_name TEXT NOT NULL,
    listen_port INTEGER NOT NULL CHECK (listen_port BETWEEN 1 AND 65535),
    public_port INTEGER NOT NULL DEFAULT 0
        CHECK (public_port = 0 OR public_port BETWEEN 1 AND 65535),

    -- NODE 改称 INBOUND:落地是一个入站,不是一台机器。改名而不是沿用旧值,
    -- 是为了让每一处调用点都必须被重新看过一遍 —— 沿用的话,那些
    -- 「按节点解析落地参数」的代码会继续编译通过,而它取到的是这台机器上
    -- 恰好排在前面的那个入站。
    target_kind TEXT NOT NULL CHECK (target_kind IN ('INBOUND', 'EXTERNAL')),
    target_inbound_id  INTEGER,
    target_external_id INTEGER,

    access_tier_id INTEGER NOT NULL DEFAULT 1,
    sort_order           INTEGER NOT NULL DEFAULT 0,
    subscription_enabled INTEGER NOT NULL DEFAULT 1,
    public_remark        TEXT    NOT NULL DEFAULT '',
    enabled              INTEGER NOT NULL DEFAULT 1,

    deleted_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    CHECK (
        (target_kind = 'INBOUND'  AND target_inbound_id  IS NOT NULL
                                  AND target_external_id IS NULL) OR
        (target_kind = 'EXTERNAL' AND target_external_id IS NOT NULL
                                  AND target_inbound_id  IS NULL)
    )
);

INSERT INTO node_relays_new (
    id, node_id, display_name, listen_port, public_port,
    target_kind, target_inbound_id, target_external_id,
    access_tier_id, sort_order, subscription_enabled, public_remark, enabled,
    deleted_at, created_at, updated_at)
SELECT
    r.id, r.node_id, r.display_name, r.listen_port, r.public_port,
    CASE WHEN r.target_kind = 'NODE' THEN 'INBOUND' ELSE 'EXTERNAL' END,
    CASE WHEN r.target_kind = 'NODE'
         THEN (SELECT i.id FROM node_inbounds i WHERE i.node_id = r.target_node_id)
         END,
    r.target_external_id,
    r.access_tier_id, r.sort_order, r.subscription_enabled, r.public_remark, r.enabled,
    r.deleted_at, r.created_at, r.updated_at
  FROM node_relays r;

DROP TABLE node_relays;
ALTER TABLE node_relays_new RENAME TO node_relays;

CREATE UNIQUE INDEX idx_node_relays_listen
    ON node_relays(node_id, listen_port) WHERE deleted_at IS NULL;
CREATE INDEX idx_node_relays_node     ON node_relays(node_id)     WHERE deleted_at IS NULL;
CREATE INDEX idx_node_relays_target   ON node_relays(target_inbound_id)
    WHERE deleted_at IS NULL AND target_kind = 'INBOUND';
CREATE INDEX idx_node_relays_external ON node_relays(target_external_id)
    WHERE deleted_at IS NULL AND target_kind = 'EXTERNAL';

-- ---------- 入站的可见性 ----------

-- 用户能用哪些入站 = 他能用这台机器(user_effective_nodes)∩ 入站等级不高于他。
--
-- 刻意建立在 user_effective_nodes 之上而不是重写一遍等级条件:节点这一层
-- 还包含 user_nodes 的额外授权,重写必然漏掉,而漏掉的表现是管理员显式
-- 授权过的用户在订阅里什么都看不到。
--
-- 规则若要变更,必须新增迁移 DROP 后重建这两个视图,不得改本文件。
CREATE VIEW user_effective_inbounds AS
    SELECT en.proxy_user_id AS proxy_user_id,
           i.id             AS inbound_id,
           i.node_id        AS node_id
      FROM node_inbounds i
      JOIN user_effective_nodes en ON en.node_id = i.node_id
      JOIN proxy_users  u  ON u.id = en.proxy_user_id
      JOIN access_tiers ut ON ut.id = u.access_tier_id
      JOIN access_tiers it ON it.id = i.access_tier_id
     WHERE i.deleted_at IS NULL
       AND i.enabled = 1
       AND it.level <= ut.level;

-- 中转条目的可见性 = 规则自己的等级 ∩ 用户在【落地入站】上确实有凭据。
--
-- 与迁移 0018 的版本只有一处不同:后半条从「用户在落地节点上有凭据」
-- 收紧成「用户在落地那个入站上有凭据」。多入站之后前者已经不够了 ——
-- 一台机器上的 VIP 入站与普通入站,普通用户在节点这一层是通过的,
-- 而透传时他出示的是那个 VIP 入站的凭据,连上去握手直接被拒。
--
-- 落地入站的 deployed_protocol != '' 取代了原来的 b.deployed_config_sha256 != '':
-- 那台机器可能部署过很多次,而这个入站是刚加的,节点上还没有它。
--
-- 落地的 subscription_enabled 仍然刻意不参与:把落地的订阅开关关掉、
-- 只让用户经中转访问它,是这个功能最典型的用法。
CREATE VIEW user_effective_relays AS
    -- 落地是自建节点的某个入站
    SELECT u.id AS proxy_user_id, r.id AS relay_id
      FROM node_relays   r
      JOIN nodes         a  ON a.id = r.node_id AND a.deleted_at IS NULL
      JOIN proxy_users   u  ON u.deleted_at IS NULL
      JOIN access_tiers  ut ON ut.id = u.access_tier_id
      JOIN access_tiers  rt ON rt.id = r.access_tier_id
      JOIN node_inbounds b  ON b.id = r.target_inbound_id AND b.deleted_at IS NULL
      JOIN nodes         bn ON bn.id = b.node_id AND bn.deleted_at IS NULL
      JOIN user_effective_inbounds eb
             ON eb.inbound_id = b.id AND eb.proxy_user_id = u.id
     WHERE r.deleted_at IS NULL
       AND r.target_kind = 'INBOUND'
       AND rt.level <= ut.level
       AND bn.status != 'DISABLED'
       AND b.deployed_protocol != ''
    UNION
    -- 落地是外部代理
    SELECT u.id AS proxy_user_id, r.id AS relay_id
      FROM node_relays      r
      JOIN nodes            a  ON a.id = r.node_id AND a.deleted_at IS NULL
      JOIN proxy_users      u  ON u.deleted_at IS NULL
      JOIN access_tiers     ut ON ut.id = u.access_tier_id
      JOIN access_tiers     rt ON rt.id = r.access_tier_id
      JOIN external_proxies p  ON p.id = r.target_external_id AND p.deleted_at IS NULL
      JOIN user_effective_external_proxies en_p
             ON en_p.external_proxy_id = p.id AND en_p.proxy_user_id = u.id
     WHERE r.deleted_at IS NULL
       AND r.target_kind = 'EXTERNAL'
       AND rt.level <= ut.level;
