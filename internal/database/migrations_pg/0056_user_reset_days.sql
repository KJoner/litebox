-- VNext TRAFFIC08(复核批次三):用户额度周期多一种「固定 N 天」,并带时区与锚点。含义见 SQLite 那一份。
-- PG 上不用重建表:换掉 reset_cycle 的 CHECK、加三列即可。约束名按内容查(基线里没有显式命名)。
DO $$
DECLARE c text;
BEGIN
    FOR c IN SELECT conname FROM pg_constraint
              WHERE conrelid = 'proxy_users'::regclass AND contype = 'c'
                AND pg_get_constraintdef(oid) LIKE '%reset_cycle%' LOOP
        EXECUTE format('ALTER TABLE proxy_users DROP CONSTRAINT %I', c);
    END LOOP;
END $$;
ALTER TABLE proxy_users ADD CONSTRAINT proxy_users_reset_cycle_check
    CHECK (reset_cycle IN ('NONE','MONTHLY','DAYS'));
ALTER TABLE proxy_users ADD COLUMN reset_period_days BIGINT NOT NULL DEFAULT 0
    CHECK (reset_period_days BETWEEN 0 AND 3650);
ALTER TABLE proxy_users ADD COLUMN reset_anchor_at TEXT NOT NULL DEFAULT '';
ALTER TABLE proxy_users ADD COLUMN reset_timezone TEXT NOT NULL DEFAULT 'UTC';
