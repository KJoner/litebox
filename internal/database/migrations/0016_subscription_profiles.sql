-- 配置文件订阅:管理员上传整份客户端配置,面板按用户替换里面的占位符。
--
-- 与「节点订阅」是两件事:
--
--                节点订阅                    配置文件订阅
--   内容          一串节点                    一整份客户端配置(分流规则、DNS、入站)
--   谁写的        面板生成                    管理员自己调好的
--   面板的角色    全部                        只替换占位符
--
-- 系统里【不预置任何模板】。内置一份就等于承诺维护它的分流规则、规则集地址与
-- 语法版本 —— 而这些每隔几个月就会变,坏掉的表现是用户的客户端起不来,
-- 用户会以为是面板坏了。管理员没配的类型,门户上整块不出现。

CREATE TABLE subscription_profiles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    -- 客户端类型。一个类型可以有多份模板(完整版/精简版/给某几个人的),
    -- 每份一个独立链接 —— 所以这里不做唯一约束。
    kind TEXT NOT NULL CHECK (kind IN ('SINGBOX', 'CLASH', 'SHADOWROCKET')),

    -- name 是内部名称,唯一;display_name 是门户上给用户看的标题。
    -- 与节点、外部代理的两套名字对称:内部名称里往往写着「给谁用的」「哪一版」,
    -- 那是运维信息,不该发到用户设备上。
    name         TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',

    -- 下发时的文件名,进 Content-Disposition,也进订阅 URL 的末段。
    -- 末段的扩展名影响客户端怎么处理这份响应,摆在 URL 里能少猜一次。
    filename TEXT NOT NULL,

    -- 模板正文。存管理员给的原文,不做任何规范化 ——
    -- sing-box 支持 JSON 注释、Clash 用 YAML 锚点、小火箭是自己的 ini 方言,
    -- 任何一次「顺手格式化」都可能把它们改坏,而改坏之后面板一个错都不报。
    content TEXT NOT NULL,

    -- sing-box 专用:落地节点的出站要挂的 detour 目标 tag(链式代理的前置组)。
    -- 空表示不挂。非空时保存前校验模板正文里出现过这个字符串 ——
    -- 指向一个不存在的 tag,sing-box 直接启动失败,而改了组名忘了改这里
    -- 是必然会发生的事。
    singbox_landing_detour TEXT NOT NULL DEFAULT '',

    -- 门户上的一句话说明。给用户看的,不是内部备注。
    description TEXT NOT NULL DEFAULT '',
    -- 内部备注,只在管理页显示。
    remark TEXT NOT NULL DEFAULT '',

    enabled    INTEGER NOT NULL DEFAULT 1,
    sort_order INTEGER NOT NULL DEFAULT 0,

    -- 软删除。id 不复用(AUTOINCREMENT + 软删),用户手上的旧链接永远不会
    -- 指向一份新配置 —— 那会在他完全不知情的情况下换掉整台机器的网络栈行为。
    deleted_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- 部分唯一索引:软删除之后名字可以复用,与 external_proxies 一致。
CREATE UNIQUE INDEX idx_subscription_profiles_name
    ON subscription_profiles(name) WHERE deleted_at IS NULL;
CREATE INDEX idx_subscription_profiles_kind ON subscription_profiles(kind);
