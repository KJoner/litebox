-- 订阅 Token 的可还原副本。
--
-- sub_token_hash 仍然是公开订阅路由 /sub/{token} 的唯一查找依据 ——
-- 该路由不解密任何东西,只按哈希比对。
--
-- 但管理员需要在面板上反复查看订阅地址(换设备、重新导入客户端),
-- 若只存哈希,每次查看都得重新生成 Token 并使用户已有订阅失效。
-- 因此额外保存一份用主密钥加密的密文,仅用于面板展示。
--
-- 安全性:该密文与用户 UUID 使用同一把主密钥。仅泄露数据库文件时两者都拿不到;
-- 同时拿到主密钥的攻击者本来就能直接读出 UUID,Token 不构成额外暴露面。
ALTER TABLE proxy_users ADD COLUMN sub_token_encrypted TEXT NOT NULL DEFAULT '';

-- user_code 的单调计数器。
--
-- 用户代码是流量统计的唯一标识,删除后绝不能被复用 ——
-- 否则新用户会继承旧用户在 traffic_ledger 中的历史记录。
-- 因此不能用 max(id)+1 之类从现存行推导的方式,必须独立计数。
INSERT INTO system_settings (key, value, updated_at)
VALUES ('user_code_sequence', '0', '1970-01-01T00:00:00Z');
