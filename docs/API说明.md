# LiteBox API 说明

三套互不相干的接口:

| 前缀 | 认证方式 | 使用者 |
| --- | --- | --- |
| `/api/*` | Cookie `litebox_session` | 管理员后台 |
| `/api/portal/*` | Cookie `litebox_portal_session` | 普通用户门户 |
| `/sub/{token}` | URL 里的随机 Token | 代理客户端 |

**两套会话不共享任何东西** —— 表、Cookie 名、中间件都不同。拿门户 Cookie
打管理接口一律 401,反之亦然(`internal/httpapi/portal_test.go` 守着这一条)。
不做成"同一套认证加个角色字段":那样每加一个接口都要重新判断一次角色,
判断写漏的后果是普通用户拿到管理权限。

## 通用约定

* 请求与响应都是 JSON,`Content-Type: application/json; charset=utf-8`;
* 请求体**拒绝未知字段**(`DisallowUnknownFields`)。字段拼错会得到
  `请求格式错误:json: unknown field "xxx"` —— 直接点名是哪个字段对不上;
* 错误响应统一为 `{"error": "中文原因"}`;
* 时间一律是 UTC 的 RFC3339 字符串,流量一律是字节;
* 节点安装、部署、握手扫描等长操作有单独的 10 分钟写超时。

## 公开接口

```
GET  /api/health                  健康检查
POST /api/auth/login              管理员登录
POST /api/portal/auth/login       用户登录
GET  /sub/{token}                 订阅(base64,通用格式)
GET  /sub/{token}?format=uri      订阅(明文 VLESS URI)
GET  /sub/{token}?format=sing-box 订阅(sing-box 客户端配置)
```

订阅端点按来源限流,不可缓存。响应带 `Subscription-Userinfo` 头,
客户端据此显示流量与到期时间。

订阅里的节点必须同时满足:用户有访问权(等级继承或额外授权)、节点未删除、
未禁用、`subscription_enabled = true`、至少成功部署过一次。最后一条是关键 ——
未部署过的节点上根本没有该用户的凭据。

**订阅只显示节点的 `display_name`**,不含内部名称。

## 管理员接口 `/api/*`

### 认证与设置

```
POST   /api/auth/logout
GET    /api/auth/me
POST   /api/auth/password
GET    /api/settings                 订阅站点根、面板公钥
PUT    /api/settings
GET    /api/access-tiers             访问等级列表
PUT    /api/access-tiers/{id}        只能改 name/description/sort_order
```

`code` 与 `level` 不开放修改:前者是程序内的引用标识,后者决定继承关系 ——
改 `level` 会让所有用户可用的节点集合同时变化,而页面上看不出发生了什么。

### 仪表盘

```
GET /api/dashboard/summary        用户/节点/流量统计
GET /api/dashboard/alerts         预警列表
GET /api/audit-logs               审计日志
```

预警条件:流量 80% / 95% / 100%,剩余 7 天 / 3 天 / 已到期,节点部署失败,
节点监控数据超过 10 分钟未更新。

**监控数据过期只算 warning,不把节点判成离线** —— 采集走的是独立的 SSH 通道,
它失败不代表 sing-box 停了。从未采样过的节点不报警(刚加的节点本来就没数据)。

### 用户

```
GET    /api/users
POST   /api/users                            可同时创建门户登录账号
GET    /api/users/{id}
PATCH  /api/users/{id}
DELETE /api/users/{id}
POST   /api/users/{id}/enabled
POST   /api/users/{id}/reset-traffic
POST   /api/users/{id}/regenerate-uuid
POST   /api/users/{id}/regenerate-sub-token
GET    /api/users/{id}/traffic
```

用户对象里有两个节点集合:

* `node_ids` —— 管理员单独追加的额外授权(`user_nodes`),编辑页面改的就是它;
* `effective_node_ids` —— 等级继承与额外授权合并后的**实际可用节点**。

配置生成、订阅与部署脏标记一律看后者。

`next_reset_at` 是流量的下次重置时刻(不重置的用户为空串),由后端算好下发。
它与门户 `/api/portal/dashboard` 用的是**同一个** `portal.NextResetAt` ——
门户上写「09-01 00:00 UTC 重置」、管理后台自己再算一遍,两边差一天时
谁都说不清哪个才算数。

创建用户时可带 `login_username` / `login_password` / `must_change_password`
一并开通门户登录。登录账号创建失败**不回滚用户** —— 失败原因几乎都是
"账号名被占用"这类需要人改一下的问题,而 `user_code` 不可复用,
回滚等于白白烧掉一个号。此时响应里会有 `portal_account_error`。

### 门户账号(管理端)

```
PUT    /api/users/{id}/portal-account          创建或修改登录账号
DELETE /api/users/{id}/portal-account
POST   /api/users/{id}/portal-login-enabled    启用/停用门户登录
POST   /api/users/{id}/revoke-portal-sessions  踢出全部登录设备
```

`password` **留空表示不修改密码**,不是"设为空密码"。填写后该账号的
全部会话立即失效。停用登录同样立刻踢掉在线会话,不等过期。

### 续期与额度调整

```
POST /api/users/{id}/adjust        单个用户
POST /api/users/batch-adjust       批量(带 user_ids)
GET  /api/users/{id}/adjustments   调整记录
```

`action` 取值:`ADD_QUOTA` `SET_QUOTA` `RESET_TRAFFIC` `EXTEND_EXPIRY`
`SET_EXPIRY` `CHANGE_TIER` `ENABLE_USER` `DISABLE_USER`。

两个必须知道的行为:

* `EXTEND_EXPIRY` 的基准取"原到期时间与现在之中较晚的那个"。从原到期日
  起算的话,给一个过期三个月的人续 30 天,他仍然是过期状态;
* `ADD_QUOTA` 对不限量用户(`quota_bytes = 0`)直接返回 400。`0 + N` 会把
  "不限"变成"只有 N",与点这个按钮的意图正好相反 —— 请改用 `SET_QUOTA`。

批量操作部分失败不回滚已成功的那些,逐条返回 `{user_id, ok, error}`。

`remark` 会展示给用户,不要写内部说明。

### 节点

```
GET    /api/nodes
POST   /api/nodes
GET    /api/nodes/{id}
PUT    /api/nodes/{id}
DELETE /api/nodes/{id}
POST   /api/nodes/{id}/enabled
POST   /api/nodes/{id}/test-ssh
POST   /api/nodes/{id}/probe
POST   /api/nodes/{id}/dest-check     握手目标实测
POST   /api/nodes/{id}/dest-scan      候选目标批量扫描
POST   /api/nodes/{id}/bootstrap      装面板公钥
POST   /api/nodes/{id}/install        装 sing-box 与服务
POST   /api/nodes/{id}/uninstall
POST   /api/nodes/{id}/deploy
POST   /api/nodes/{id}/restart
POST   /api/nodes/{id}/reset-host-key
GET    /api/nodes/{id}/deployments
GET    /api/nodes/{id}/config-diff
GET    /api/panel-key
GET    /api/dest-candidates
GET    /api/deployments
```

节点有两个名称:`name` 是内部名称,只在管理后台出现;`display_name` 是
发给用户与订阅的名字。留空创建时 `display_name` 复制 `name`。

`GET /api/nodes` 与 `GET /api/nodes/{id}` 的每一项额外带两个**算出来的**字段
(创建与更新的响应里没有,调用方随后都会重新拉列表):

| 字段 | 取值 | 含义 |
| --- | --- | --- |
| `config_state` | `NEVER_DEPLOYED` | 从未成功部署过 |
| | `IN_SYNC` | 节点上生效的配置与库里当前应渲染的一致 |
| | `PENDING` | 库里已变更,节点上还是旧配置 |
| | `DEPLOY_FAILED` | 最近一次部署失败,且改动仍未下发 |
| | `UNKNOWN` | 配置渲染不出来,面板也判定不了 |
| `needs_deploy` | bool | 是否该提示部署 |

它回答的是「库里当前应有的配置,是否已经在节点上生效」,与 `status`(sing-box
在不在跑)正交:一台 ONLINE 的机器完全可能跑着三次变更之前的配置。

判定是**纯查库**的 —— 比较 `deployed_config_sha256` 与此刻重新渲染出的哈希,
不连 SSH。`config-diff` 能给更准的答案,但它要连上去读节点上的实际配置,
10 台机器就是 10 条 SSH 会话,不能在列表里逐行调用。

两个值分开给:`needs_deploy` 驱动界面上的「该部署了」提示,`config_state` 只
描述事实。`UNKNOWN` 与已禁用的节点一律 `needs_deploy=false` —— 不确定就不催,
催错了管理员会去重启一台正常的机器。

`PUT /api/nodes/{id}` 的几个字段有特殊语义:

* `access_tier_id` 为 **0** 表示保持原等级;
* `subscription_enabled` 为 **null** 表示保持原值;
* `traffic_quota_bytes` 为 **null** 表示保持原额度(0 是"改成不限量",
  所以不能用零值表达"没传");
* `traffic_reset_cycle` 留空、`traffic_reset_day` 为 0 表示保持原值;
* `ipv6_address` 留空表示**清空 IPv6**,与上面几个正好相反 ——
  清空是管理员的显式动作(把 IPv6 条目从订阅撤下来),必须有办法表达。
  `ipv6_proxy_port` 同理:0 表示「跟随 IPv4」,不是「保持原值」。
  它与 `ipv6_address` 总是一起提交,不存在只漏传一个的情况。

前四个不回落到零值,是因为漏传的后果是静默的:VIP 节点被降成普通组等于
给全体用户开门,订阅开关被关掉等于把节点从所有人的订阅里摘掉,
额度被清成不限量则是预警从此不再出现,三者都不报错。

### 地址字段

* `host` 是 **IPv4**,同时是 SSH 管理地址与 IPv4 订阅地址,必填。
  新建节点要求 IPv4 字面量;编辑时只有确实改动了这一栏才按严格规则校验
  (V1 起就允许填域名,存量节点可能就是域名接入的);
* `ipv6_address` 选填,**只影响订阅**。填了之后订阅里会额外出现一条
  `展示名称-IPV6`,服务器地址换成 IPv6,其余(UUID、REALITY 公钥、short ID、
  握手目标、指纹、flow)与 IPv4 条目完全相同;
* `ipv6_proxy_port` 选填,是 IPv6 条目在订阅里用的公网端口。
  **为 0 表示跟随 `proxy_port`**,只有两个协议栈映射到不同外部端口时才需要填
  (NAT 小鸡上很常见:IPv4 是服务商映射的高位端口,IPv6 是直连的 443)。
  它只改 IPv6 那一条,IPv4 条目一个字段都不动;`listen_port` 仍然只有一个 ——
  两条链路指向同一个 sing-box 入站;
* **0 原样存库,不在写入时解析成当时的 `proxy_port`**,解析发生在订阅生成时。
  写死的话,之后把公网端口从 443 改成 8443,IPv6 条目会继续停在 443 上,
  而管理员当初看到的是一个空输入框,完全不会想到那里固化了一个值;
* 清空 `ipv6_address` 时 `ipv6_proxy_port` 一并归零。留着它的话,下次重新填上
  IPv6 会静默套用一个几个月前的端口,而那个端口未必还转发着;
* 提交时可以带方括号,后端会剥掉并标准化后存储(`[2602:FED2::0001]`
  存成 `2602:fed2::1`);
* IPv4 栏填 IPv6、IPv6 栏填 IPv4 都会返回 400 并说明该填到哪一栏;
* **改 `ipv6_address` 既不置 `ssh_changed` 也不置 `needs_deploy`** ——
  它不参与 SSH,也不进节点配置,订阅下次拉取即生效。

### 节点流量额度

```json
{
  "traffic_quota_bytes": 107374182400,
  "traffic_reset_cycle": "MONTHLY",
  "traffic_reset_day": 15,
  "traffic_billing_mode": "BOTH"
}
```

`traffic_quota_bytes = 0` 表示不限量;`traffic_reset_cycle` 只接受
`NONE`(统计节点创建以来的累计流量)与 `MONTHLY`;`traffic_reset_day`
取 1~31,当月没有该日时落到当月最后一天,边界统一取 **UTC 00:00**。

`traffic_billing_mode` 是 VPS 商计量这台机器流量的口径,只接受
`EGRESS`(只计出站,与 sing-box 计数 1:1)与 `BOTH`(进出合计,约两倍)。
更新接口留空表示保持原值。**`traffic_quota_bytes` 按这个口径填**,
也就是商家账单上的那个数字。

`GET /api/traffic/nodes-cycle` 与节点流量接口的返回里因此有两组数:

| 字段 | 含义 |
|---|---|
| `uplink_bytes` / `downlink_bytes` / `proxy_bytes` | sing-box 的原始计数,**永远不乘倍数** |
| `used_bytes` | 折算到主机计费口径之后的量,与 `quota_bytes` 同口径 |
| `billing_mode` / `billing_factor` | 口径与倍数(1 或 2) |

`remaining_bytes`、`usage_percent`、`warning_level`、`exceeded` 全部基于
`used_bytes` —— 分子分母口径不一致是这类统计里最容易出、也最难看出来的错。

改额度、周期与计费口径同样不触发重新部署 —— 它们只用于统计与预警,
一个字节都不进节点配置。

响应里的 `effect`:

* `ssh_changed` —— 连接参数变了,面板已丢弃该节点的 SSH 长连接;
* `needs_deploy` —— 节点上跑的配置已与期望状态不一致;
* `tier_changed` —— 访问等级变了,**面板已自动标脏并会尽快重新部署**。

端口类变更不自动部署(切换时机要与 NAT/nginx 的改动配合),但等级变更会 ——
拖着不部署等于被移出的用户还能继续用。

新增节点时可带 `root_password`:**只用于把面板公钥装进节点的那一次连接,
不落库、不写日志、不进审计详情**。留空则改用主控本机 `~/.ssh` 下的私钥。

### 流量与监控

```
POST /api/nodes/{id}/sync-traffic
GET  /api/traffic/status
GET  /api/traffic/nodes-today          今日各节点流量,一次取回
GET  /api/traffic/nodes-cycle          各节点当前额度周期用量,一次取回
GET  /api/traffic/daily?days=30        全站每日流量,供仪表盘趋势图
GET  /api/nodes/{id}/traffic?days=30   周期汇总 + 每日趋势
GET  /api/metrics/nodes-latest
GET  /api/nodes/{id}/metrics?hours=6      6 / 24 / 72 / 168
POST /api/nodes/{id}/collect-metrics
GET  /api/metrics/status
```

`GET /api/nodes/{id}/traffic` 同时返回 `cycle` 与 `daily`。两者口径不同,
不能互相替代:`daily` 按 UTC 自然日聚合,表达不了"每月 15 日 00:00"
这种非零点的周期边界;`cycle` 直接按时间范围汇总 `traffic_ledger`,
是额度判断的依据。趋势图继续用 `daily`。

**所有 `daily` 系列(全站、按节点、按用户、门户)只返回确实有记录的日子,
不把中间缺的日子补成 0。** 库里没有那一天,可能是那天真的没人用,也可能是
同步任务没跑完 —— 两者长得一模一样,补 0 等于替其中一种下了结论。
前端按日期轴展开,缺的日子画成空心柱 / 灰虚线,明说「未知」。

周期用量对象(`nodes-cycle` 的每一项与 `cycle` 同构):

```json
{
  "node_id": 1,
  "period_start": "2026-07-15T00:00:00Z",
  "next_reset_at": "2026-08-15T00:00:00Z",
  "uplink_bytes": 22548578304,
  "downlink_bytes": 69793218560,
  "used_bytes": 92341796864,
  "quota_bytes": 107374182400,
  "remaining_bytes": 15032385536,
  "usage_percent": 86.0,
  "unlimited": false,
  "exceeded": false,
  "warning_level": "WARNING",
  "reset_cycle": "MONTHLY",
  "reset_day": 15
}
```

* `warning_level` 取 `UNLIMITED` / `NORMAL` / `WARNING`(≥80%)/
  `DANGER`(≥95%)/ `EXCEEDED`(≥100%);
* **不限量节点的 `remaining_bytes` 与 `usage_percent` 是 `null`**,
  不是 0 —— 前端拿到 0 会画成"剩余 0 字节"的红条,与"不限量"正好相反;
* 超额时 `remaining_bytes` 夹到 0,超额本身由 `exceeded` 与 `EXCEEDED` 表达;
* `next_reset_at` 在 `NONE` 周期下为 `null`;
* 统计包含该节点下**全部**用户的上下行,含已停用与已删除用户留下的历史流量,
  也不区分 IPv4 与 IPv6 —— 两个订阅条目指向同一个 sing-box 入站与同一个计数器;
* **超额只预警**:不会停 sing-box、不禁用节点、不关订阅开关,也不删用户凭据。

## 用户门户接口 `/api/portal/*`

```
POST   /api/portal/auth/login
POST   /api/portal/auth/logout
GET    /api/portal/auth/me
POST   /api/portal/auth/password
GET    /api/portal/auth/sessions
DELETE /api/portal/auth/sessions/{id}
POST   /api/portal/auth/logout-all

GET    /api/portal/dashboard
GET    /api/portal/nodes
GET    /api/portal/traffic?days=7|30
GET    /api/portal/subscription
POST   /api/portal/subscription/regenerate
GET    /api/portal/adjustments
```

### 三条不可动摇的规则

1. **所有接口从会话取 `proxy_user_id`,不接受任何前端传入的用户标识。**
   改 URL 或请求参数都拿不到别人的数据,因为根本没有一个参数可以改;

2. **响应是显式白名单 DTO,不复用管理端结构体。** 节点内部名称、SSH 用户与
   端口、SSH 私钥、REALITY 私钥、V2Ray API 端口、部署路径、部署错误明细与
   主机资源监控数据都不在这些结构体里 —— 不是"记得别填",而是压根没有
   位置可填;

3. **强制改密期间除 `/auth/me` 与改密、退出接口外一律 403。** 只靠前端弹窗
   拦不住直接调接口的人,而这条限制的意义正是在密码改掉之前不让初始口令
   换到订阅地址。

### 登录

过期与超额的用户**仍然可以登录**,只是 `dashboard.serviceable` 为 false 并
带上 `reason`,订阅区域显示不可用原因。挡住他们只会让人连"为什么断了"都
看不到,只能来问管理员。唯一能挡住登录的是管理员停用了该账号。

账号不存在与密码错误返回**同一个错误**,并消耗等量的哈希计算时间 ——
否则这个接口就是账号枚举器。

### 首页字段

额度为 0 时 `used_percent` 为 `null`、`remaining` 为 0,由前端显示"不限量" ——
不做除零,也不编造一个百分比。

`days` 只接受 7 与 30,其他值一律回落到 30,不让前端传个 100000 把整张表扫出来。

### 我的节点

`status` 只有三种对用户有意义的取值:`normal` / `maintenance` / `disabled`。
不下发 `DEPLOY_FAILED` 这类运维状态 —— 用户对它无能为力,只会平添一次
"是不是我的问题"的追问。

未部署或已下架的节点仍会出现在列表里但标成 `maintenance`,直接隐藏会让
用户以为节点被删了。

### 重新生成订阅

只换 Token,不触发部署:节点上的 UUID 没变,用户的连接不会断,变的只是
拉订阅的地址。前端负责在点击前弹出确认。
