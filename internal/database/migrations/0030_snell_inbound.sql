-- Snell 落地协议(V14)。
--
-- 自建节点的第四种落地协议,也是第三种 sing-box 入站
-- (VLESS + REALITY、Shadowsocks 2022、Snell)。它是 sing-box 1.14 才有的,
-- 所以只有 singbox_channel = 'PREVIEW' 的机器上能选 —— 见迁移 0029。
--
-- **它是 node_inbounds 的一种 protocol,不是第四张入口表。** 与 Mieru
-- 正好相反:那一个的服务端是另一个进程(mita),混进来会让配置渲染、
-- 部署、拨测、流量采集四条路径每一处都要先判断"这一行是不是真的
-- sing-box 入站";而 Snell 就是 config.json 里的一个 inbound,和另外两种
-- 共用同一个进程、同一份用户列表、同一套 stats 计数器、同一次部署。
-- 拆出去才是分叉。
--
-- ---------- 为什么必须重建整张表 ----------
--
-- protocol 那一列上有 CHECK (protocol IN ('VLESS_REALITY','SHADOWSOCKS')),
-- 而 SQLite 改不了 CHECK。与迁移 0027 给 deployments.kind 加 MIERU
-- 是同一件事,只是这张表大得多、还有三个视图压在上面。
--
-- 行要**全量搬过来**:node_inbounds 里是每一个入口的 tag、握手目标、
-- 凭据与 deployed_* 状态,丢一行就是那个入口从所有人的订阅里消失,
-- 而节点上它还在跑。
--
-- 视图必须先拆后建:SQLite 的 ALTER TABLE ... RENAME 会解析库里全部视图,
-- 其中任何一个指向一张已经不存在的表都会让整条迁移失败。
-- 依赖方向是 relays → inbounds、nodes → inbounds,所以拆的顺序与建相反。
-- user_effective_mieru_inbounds 不碰 node_inbounds(迁移 0028 把它从
-- user_effective_nodes 上摘下来了),因此不在这里动它。

DROP VIEW user_effective_relays;
DROP VIEW user_effective_nodes;
DROP VIEW user_effective_inbounds;

CREATE TABLE node_inbounds_new (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,

    -- 以下各列的含义与迁移 0019 完全一致,注释不再重复,只在新增的几列上写。
    tag          TEXT NOT NULL,
    display_name TEXT NOT NULL,

    protocol TEXT NOT NULL DEFAULT 'VLESS_REALITY'
        CHECK (protocol IN ('VLESS_REALITY', 'SHADOWSOCKS', 'SNELL')),

    listen_port      INTEGER NOT NULL CHECK (listen_port BETWEEN 1 AND 65535),
    public_port      INTEGER NOT NULL DEFAULT 0
        CHECK (public_port = 0 OR public_port BETWEEN 1 AND 65535),
    ipv6_public_port INTEGER NOT NULL DEFAULT 0
        CHECK (ipv6_public_port = 0 OR ipv6_public_port BETWEEN 1 AND 65535),

    tcp_fast_open INTEGER NOT NULL DEFAULT 0,

    reality_dest              TEXT    NOT NULL DEFAULT '',
    reality_dest_port         INTEGER NOT NULL DEFAULT 443,
    reality_privkey_encrypted TEXT    NOT NULL DEFAULT '',
    reality_pubkey            TEXT    NOT NULL DEFAULT '',
    reality_short_id          TEXT    NOT NULL DEFAULT '',
    handshake_max_record_size INTEGER NOT NULL DEFAULT 0,
    handshake_checked_at      TEXT,

    ss_method TEXT NOT NULL DEFAULT ''
        CHECK (ss_method IN ('', '2022-blake3-aes-128-gcm',
                             '2022-blake3-aes-256-gcm',
                             '2022-blake3-chacha20-poly1305')),
    ss_password_encrypted TEXT NOT NULL DEFAULT '',

    -- ---------- Snell 专有 ----------
    --
    -- 0 表示这不是一个 Snell 入站。入站只收 5 与 6(实测:4 与 7 都被
    -- 上游当场拒掉)。**服务端的 5 对应客户端的 4** —— 上游刻意不提供
    -- v5 客户端("v5 的线路协议实际上与 v4 没有区别"),订阅那一侧写 5
    -- 会被客户端的 enum 拒掉,而那是整条线路连不上。映射只有
    -- snell.ClientVersion 一处实现。
    snell_version INTEGER NOT NULL DEFAULT 0
        CHECK (snell_version IN (0, 5, 6)),

    -- 入站级 PSK,主密钥加密。**它是要发给每一个用户的**(客户端配置里
    -- 就有这一项),这一点与 Shadowsocks 2022 的节点级 PSK 不同 ——
    -- 那一份从不单独离开面板(客户端拿到的是 serverPSK:userPSK 拼起来的
    -- 一串)。Snell 的 psk 原样出现在每个人的配置里,userkey 才是身份。
    --
    -- 由此推出一条硬约束,写在渲染层(singbox.ErrSnellNoUsers):
    -- **users 渲染成空列表时 sing-box 会静默退回单用户模式**,那时它
    -- 根本不读 ClientID —— 于是每一个曾经拿到过 psk 的人(包括刚被移出
    -- 的那个)照常连得上,计数器一个都不产生,而面板上一个字都不说。
    -- V14 技术验证 §4 实测:两个客户端各拿到完整的 1MB,用户计数器为空。
    snell_psk_encrypted TEXT NOT NULL DEFAULT '',

    -- 仅版本 5:HTTP/TLS 混淆。空串按 none 处理并整项不渲染 ——
    -- 写一个与默认值相同的字段,行为一个字节不变,却会改掉配置哈希。
    snell_obfs_mode TEXT NOT NULL DEFAULT ''
        CHECK (snell_obfs_mode IN ('', 'none', 'http', 'tls')),

    -- 混淆时客户端伪装的 Host。**只进客户端配置,不进节点配置** ——
    -- 服务端不校验它(sing-box 的 snell 入站压根没有这个字段)。
    -- 所以它既不改配置哈希、也不需要 deployed_ 镜像、更不置 NeedsDeploy,
    -- 与 ipv6_display_name 同类:一个只影响订阅内容的字段。
    snell_obfs_host TEXT NOT NULL DEFAULT '',

    -- 仅版本 6:流量整形模式。空串按 default 处理,理由同 obfs_mode。
    snell_v6_mode TEXT NOT NULL DEFAULT ''
        CHECK (snell_v6_mode IN ('', 'default', 'unshaped', 'unsafe-raw')),

    chain_target_kind TEXT NOT NULL DEFAULT ''
        CHECK (chain_target_kind IN ('', 'INBOUND', 'EXTERNAL')),
    chain_target_inbound_id  INTEGER,
    chain_target_external_id INTEGER,

    chain_code                  TEXT NOT NULL DEFAULT '',
    chain_uuid_encrypted        TEXT NOT NULL DEFAULT '',
    chain_ss_password_encrypted TEXT NOT NULL DEFAULT '',

    access_tier_id INTEGER NOT NULL DEFAULT 1,

    sort_order           INTEGER NOT NULL DEFAULT 0,
    subscription_enabled INTEGER NOT NULL DEFAULT 1,
    public_remark        TEXT    NOT NULL DEFAULT '',
    enabled              INTEGER NOT NULL DEFAULT 1,

    deployed_protocol      TEXT    NOT NULL DEFAULT '',
    deployed_ss_method     TEXT    NOT NULL DEFAULT '',
    deployed_tcp_fast_open INTEGER NOT NULL DEFAULT 0,

    -- 节点上【当前正在生效】的两项 Snell 参数,只在部署成功时写入。
    --
    -- 与 deployed_ss_method 一字不差的理由:管理员改版本或混淆模式到
    -- 部署成功之间存在一个窗口(部署失败的话是永远),按期望值下发订阅
    -- 会让客户端拿到一份与节点上不符的参数 —— 而数据库、节点、面板三方
    -- 都是"对的",只有订阅站在中间说了假话。
    --
    -- psk 没有镜像列,与 ss_password_encrypted / reality_privkey_encrypted
    -- 一致:它在建入口时生成一次,之后没有任何路径会改它。
    deployed_snell_version   INTEGER NOT NULL DEFAULT 0,
    deployed_snell_obfs_mode TEXT    NOT NULL DEFAULT '',
    deployed_snell_v6_mode   TEXT    NOT NULL DEFAULT '',

    deleted_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    ipv6_enabled      INTEGER NOT NULL DEFAULT 1,
    ipv6_display_name TEXT    NOT NULL DEFAULT '',

    CHECK (
        (chain_target_kind = ''         AND chain_target_inbound_id  IS NULL
                                        AND chain_target_external_id IS NULL) OR
        (chain_target_kind = 'INBOUND'  AND chain_target_inbound_id  IS NOT NULL
                                        AND chain_target_external_id IS NULL) OR
        (chain_target_kind = 'EXTERNAL' AND chain_target_external_id IS NOT NULL
                                        AND chain_target_inbound_id  IS NULL)
    )
);

-- 逐列写出来而不是 SELECT *:两张表的列顺序不同(新增的几列插在中间),
-- 靠位置对齐会把 snell_obfs_mode 塞进 chain_target_kind 里,
-- 而那一列上恰好有 CHECK,报出来是一句与 Snell 毫无关系的约束失败。
INSERT INTO node_inbounds_new (
    id, node_id, tag, display_name, protocol,
    listen_port, public_port, ipv6_public_port, tcp_fast_open,
    reality_dest, reality_dest_port, reality_privkey_encrypted,
    reality_pubkey, reality_short_id,
    handshake_max_record_size, handshake_checked_at,
    ss_method, ss_password_encrypted,
    chain_target_kind, chain_target_inbound_id, chain_target_external_id,
    chain_code, chain_uuid_encrypted, chain_ss_password_encrypted,
    access_tier_id, sort_order, subscription_enabled, public_remark, enabled,
    deployed_protocol, deployed_ss_method, deployed_tcp_fast_open,
    deleted_at, created_at, updated_at,
    ipv6_enabled, ipv6_display_name)
SELECT
    id, node_id, tag, display_name, protocol,
    listen_port, public_port, ipv6_public_port, tcp_fast_open,
    reality_dest, reality_dest_port, reality_privkey_encrypted,
    reality_pubkey, reality_short_id,
    handshake_max_record_size, handshake_checked_at,
    ss_method, ss_password_encrypted,
    chain_target_kind, chain_target_inbound_id, chain_target_external_id,
    chain_code, chain_uuid_encrypted, chain_ss_password_encrypted,
    access_tier_id, sort_order, subscription_enabled, public_remark, enabled,
    deployed_protocol, deployed_ss_method, deployed_tcp_fast_open,
    deleted_at, created_at, updated_at,
    ipv6_enabled, ipv6_display_name
  FROM node_inbounds;

DROP TABLE node_inbounds;
ALTER TABLE node_inbounds_new RENAME TO node_inbounds;

-- 索引与迁移 0019 逐字相同。少建一个的后果各不相同,但都不响亮:
-- 少了 listen 那个唯一索引,同机两个入站可以配到同一个端口上,
-- 第二个 bind 失败、整个 sing-box 起不来,而要到部署的健康检查才发现;
-- 少了 tag 那个,新入站会抢到软删除入站的 tag,两段互不相干的
-- 入站级统计历史被接在同一条曲线上。
CREATE UNIQUE INDEX idx_node_inbounds_listen ON node_inbounds(node_id, listen_port)
    WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX idx_node_inbounds_tag ON node_inbounds(node_id, tag)
    WHERE tag != '';
CREATE INDEX idx_node_inbounds_node ON node_inbounds(node_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_node_inbounds_chain_inbound  ON node_inbounds(chain_target_inbound_id)
    WHERE chain_target_kind = 'INBOUND';
CREATE INDEX idx_node_inbounds_chain_external ON node_inbounds(chain_target_external_id)
    WHERE chain_target_kind = 'EXTERNAL';

-- 三个视图逐字重建,**规则一个字都没改**。
--
-- 这次重建纯粹是 SQLite 改不了 CHECK 的代价,不是一次规则变更 ——
-- 顺手改一条规则会让"这次升级到底改了什么"变成一个要逐行比对才能回答的
-- 问题,而访问范围正是最不该靠比对来确认的东西。规则若要变更,
-- 必须再新增一条迁移。
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

CREATE VIEW user_effective_nodes AS
    SELECT DISTINCT proxy_user_id, node_id FROM user_effective_inbounds;

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

-- ---------- 用户的第四份凭据 ----------
--
-- 与 UUID、SS 密钥、mieru 口令平级,各管一种协议。**绝不复用其中任何一把**:
-- 复用的话,重置其中一种会连带作废另外几种,而管理员点的是一个协议的
-- 「重置凭据」。
--
-- 编码用 base64url 不补等号,与 mieru 那一份同一个理由:snell 的 userkey
-- 是一串不透明字节(上游只要求 1..255 字节、同一入站内不重复),谁都不解码它,
-- 所以可以选一个不含 + / = 的字母表,让它直接进客户端配置而不需要转义。
ALTER TABLE proxy_users ADD COLUMN snell_password_encrypted TEXT NOT NULL DEFAULT '';
