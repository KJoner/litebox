-- 复核 F01:改用户的重置规则不再静默清零本周期用量,固定 N 天的起点按当地日期存。
--
-- 原来 Store.Update 直接改写当前规则,而 last_reset_at 没动:11 月 10 日把「每月 1 日」改成「每月 5 日」,
-- 下一轮额度检查把 11 月 5 日当成一个还没做过的重置点,当场把这个月的用量清零 —— 管理员做的事情只是改了个日期。
--
-- 现在默认「下一周期生效」:新规则先记在 reset_next_*,当前规则照旧跑到它自己的下一个重置点,那一刻重置
-- 并换成新规则(与计费规则的 billing_next_* 同一个做法)。「立即生效」是单独的显式选择:规则当场换,
-- 但 reset_rule_since 记下换的时刻,那之前的边界一律不算 —— 不追溯清零,下一次重置是新规则在此刻之后的第一个边界。
-- 原来不重置(NONE)的用户没有「下一周期」,只能立即生效。
--
--   reset_next_cycle       待生效的周期种类;'' = 没有待生效的规则
--   reset_next_day / reset_next_period_days / reset_next_anchor_at / reset_next_timezone  与 reset_* 一一对应
--   reset_rule_since       当前规则从何时起算(RFC3339);'' = 从 last_reset_at / 创建时间起算(存量行为不变)
ALTER TABLE proxy_users ADD COLUMN reset_next_cycle TEXT NOT NULL DEFAULT ''
    CHECK (reset_next_cycle IN ('', 'NONE', 'MONTHLY', 'DAYS'));
ALTER TABLE proxy_users ADD COLUMN reset_next_day INTEGER NOT NULL DEFAULT 0;
ALTER TABLE proxy_users ADD COLUMN reset_next_period_days INTEGER NOT NULL DEFAULT 0;
ALTER TABLE proxy_users ADD COLUMN reset_next_anchor_at TEXT NOT NULL DEFAULT '';
ALTER TABLE proxy_users ADD COLUMN reset_next_timezone TEXT NOT NULL DEFAULT '';
ALTER TABLE proxy_users ADD COLUMN reset_rule_since TEXT NOT NULL DEFAULT '';

-- 固定 N 天的起点改按「当地日期」存(YYYY-MM-DD,按 reset_timezone 的零点解释)。
-- 界面原来把选中的日期拼成 YYYY-MM-DDT00:00:00Z 发上来,后端再换到用户时区 —— 美洲时区里那是前一天的晚上,
-- 起点整整早了一天。那一种写法只可能来自日期选择器,它的本意就是那个日期,所以原样取前 10 个字符;
-- 别的 RFC3339 写法(接口直接调用)保持时刻的含义,一个字不改。
UPDATE proxy_users SET reset_anchor_at = substr(reset_anchor_at, 1, 10)
 WHERE reset_anchor_at LIKE '____-__-__T00:00:00Z';
