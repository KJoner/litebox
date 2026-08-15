-- 节点落地协议。V4 起节点可以跑 VLESS + REALITY 或 Shadowsocks 2022。
--
-- 协议是节点级属性:一个节点一个入站,不在同一节点上同时跑两种。
-- 多入站会让流量统计的归属变成一个需要重新设计的问题,而配置生成里
-- "只有一个入站"那条断言正是靠这一点成立的。
--
-- 默认 VLESS_REALITY:存量节点升级后渲染出的配置与升级前【逐字节相同】,
-- 不会凭空产生一次 diff 与一次全站重新部署。
ALTER TABLE nodes ADD COLUMN protocol TEXT NOT NULL DEFAULT 'VLESS_REALITY'
    CHECK (protocol IN ('VLESS_REALITY', 'SHADOWSOCKS'));

-- Shadowsocks 2022 的加密方法与节点级 PSK。VLESS 节点上这两列为空,
-- 不参与任何校验 —— 拿 VLESS 的规矩去量 SS 节点会让正常节点保存不了,反之亦然。
--
-- 只允许 2022 系列三种,不收传统 AEAD(aes-128-gcm 等):后者的多用户没有 EIH,
-- 服务端要对每个用户试解密,而且没有 replay 防护。
ALTER TABLE nodes ADD COLUMN ss_method TEXT NOT NULL DEFAULT ''
    CHECK (ss_method IN ('', '2022-blake3-aes-128-gcm',
                         '2022-blake3-aes-256-gcm',
                         '2022-blake3-chacha20-poly1305'));

-- 节点 PSK 存主密钥加密后的密文。存的是 32 字节的 base64,
-- 按 method 需要的长度截取发生在渲染时 —— 换 method 不必重新签发凭据。
ALTER TABLE nodes ADD COLUMN ss_password_encrypted TEXT NOT NULL DEFAULT '';

-- 节点上【当前正在生效】的协议与方法,只在部署成功时写入。
--
-- 订阅与门户只看这两列,不看上面那两列的期望值。
-- 管理员改协议到部署成功之间存在一个窗口 —— 可能二十秒,也可能是
-- 部署失败自动回滚之后的永远。订阅若按期望值渲染,这个窗口里用户拉到的是
-- ss:// 而节点上跑的还是 VLESS,客户端握手失败,而数据库、节点、面板
-- 三方都是"对的",只有订阅站在中间说了假话。
ALTER TABLE nodes ADD COLUMN deployed_protocol TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN deployed_ss_method TEXT NOT NULL DEFAULT '';

-- 存量节点凡是部署过的,节点上跑的就是 VLESS。没部署过的保持空串,
-- 它们本来也进不了订阅(订阅要求 deployed_config_sha256 非空)。
UPDATE nodes SET deployed_protocol = 'VLESS_REALITY'
 WHERE deployed_config_sha256 != '';

-- 用户的 Shadowsocks 2022 PSK,同样是 32 字节 base64 的密文。
--
-- 迁移里生成不了 —— 主密钥在 Go 侧,SQLite 这边没有加密能力。
-- 由服务启动时的一次性 backfill 补齐:扫这一列为空的未删除用户,
-- 生成并写回,跑过一次之后永远是 no-op。
--
-- 空串表示"还没生成",与 nodes.ssh_key_encrypted 的既有约定一致:
-- 空值必须存空串而不是加密后的空串,后者不为空,会被当成一把解不开的密钥。
ALTER TABLE proxy_users ADD COLUMN ss_password_encrypted TEXT NOT NULL DEFAULT '';
