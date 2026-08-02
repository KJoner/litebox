-- 订阅访问记录。
--
-- 只保留"最近一次"而不是完整历史:客户端会周期性拉取订阅,
-- 逐次入库会让表无节制增长,而运维真正要回答的问题只有
-- "这个用户到底导入订阅了没有""上次是什么时候拉的"。
ALTER TABLE proxy_users ADD COLUMN sub_last_access_at TEXT;
ALTER TABLE proxy_users ADD COLUMN sub_last_access_ip TEXT NOT NULL DEFAULT '';
ALTER TABLE proxy_users ADD COLUMN sub_last_user_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE proxy_users ADD COLUMN sub_access_count INTEGER NOT NULL DEFAULT 0;
