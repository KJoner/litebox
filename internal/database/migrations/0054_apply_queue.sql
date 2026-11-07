-- R05:持久化的下发队列,以及 Mieru 入口「节点上生效的是哪一份配置」。
--
-- 部署协调器原来只在内存里排队:节点离线时那一次下发失败就没有下文了,被停用 / 过期 / 超额 /
-- 撤权的人的凭据会一直留在节点上,直到碰巧有别的改动再触发一次下发;面板重启时排着的也丢了。
-- apply_queue 每台机器每种下发一行:标脏时写进来,下发成功删掉;失败时按「节点有没有被动过」分两种 ——
--   RETRY:节点一个字节都没动过(连不上、前置检查拦下),按退避自动再试,机器恢复后自己收敛;
--   BLOCKED:已经改过配置又回滚了,再试一次就是再重启一次服务、再踢一次全部在线连接,停下来等人处理。
-- seq 每次标脏加一:下发进行中又有新的改动进来时,成功那一刻 seq 已经变了,这一行留着等下一轮。
CREATE TABLE apply_queue (
    node_id         INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    kind            TEXT    NOT NULL CHECK (kind IN ('SINGBOX', 'RELAYS', 'REALM', 'MIERU')),
    seq             INTEGER NOT NULL DEFAULT 1,
    state           TEXT    NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING', 'RETRY', 'BLOCKED')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT    NOT NULL DEFAULT '',
    last_error      TEXT    NOT NULL DEFAULT '',
    queued_at       TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL,
    PRIMARY KEY (node_id, kind)
);

-- 节点上生效的 mita 配置的哈希,以及去掉用户列表之后的「形状」哈希。
-- 自动下发只在形状没变(只动了用户)时做,而且走 reload —— 端口、传输层、出口这些改动
-- 仍然等管理员自己挑时机下发。存量行为空串:那时按 deployed_* 那几列判断形状。
ALTER TABLE node_mieru_inbounds ADD COLUMN deployed_config_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE node_mieru_inbounds ADD COLUMN deployed_shape_sha256 TEXT NOT NULL DEFAULT '';
