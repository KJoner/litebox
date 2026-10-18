# 节点资产探针卡片 —— 开发前审计(现状 / 复用 / 缺口 / 风险 / 修改计划)

需求书在 `docs/开发计划/v20/LiteBox-asset-probe-requirements.md`(本地,不进仓库)。
本文按需求书第 2 节的五个问题逐条回答,再给出数据模型草案、卡片字段清单与分阶段修改清单。

## 1. 现状:已经实现、优先复用的能力

| 能力 | 现有实现 | 本次怎么用 |
|---|---|---|
| CPU / 内存 / 磁盘 / 网速 | `node_metrics`(`node.Monitor` 每 5 分钟经 SSH 采一次,保留 7 天);`GET /api/metrics/nodes-latest` 一次返回全部节点最新一条 | 卡片直接读,**不新增采集**;过期判定沿用 `isMetricsStale` / `isPreChange` |
| 节点周期流量 | `traffic.Querier.NodesCycleUsage`(按计费口径折算后的 `used_bytes` / `quota_bytes`),批量接口 `GET /api/traffic/nodes-cycle` | 卡片流量条直接读;「按流量估算」的剩余量也取它 |
| 代理源流量 | `proxy_sources.upstream_used_bytes / upstream_total_bytes`(上游 Subscription-Userinfo) | 代理源卡片的流量与「按流量估算」 |
| 运行 / 配置 / 巡检状态 | `nodes.status`、`config_state`、`node.Watchdog` 的巡检报告、`healthSummary.ts` | 卡片头部状态沿用同一套判据,不另写 |
| 商家到期、自动续费、提前 7/3/1 天提醒 | V20 `expiry_profiles` + `expiry.Engine`(持久化去重、渠道级重试) | **计费字段加在 `expiry_profiles` 上**,不另建一套「套餐」入口 |
| 续费 / 延长 / 幂等 | `expiry_renewals`(`request_id` 部分唯一索引)、按自然月 `AddMonthsClamped` | 续费记录加「实际支付金额与币种」两列;幂等沿用 |
| 推送 | `notify.Notifier`(Bark / Telegram,事件开关、冷却去重) | 新增两种事件:探测连续失败 / 恢复 |
| 云实例停机 | `cloud` 引擎 + `cloudSkip`(巡检与采集共用的判据) | 探测同样跳过停着的实例,记为「已关机」 |
| 显示偏好 | 主题(`stores/theme.ts`)、侧栏折叠都存 localStorage | 「卡片视图 / 原有视图」同样存 localStorage |

## 2. 四种「订阅 / 线路」在模型里怎么区分

| 业务实体 | 表 | 是不是采购成本 |
|---|---|---|
| 自建节点(VPS) | `nodes` | 是 —— 计费挂 `expiry_profiles(kind = NODE)` |
| 购买的外部订阅(机场) | `proxy_sources` | 是 —— `kind = PROXY_SOURCE`;源下导入的条目默认跟随来源 |
| 付费外部代理(单条线路) | `external_proxies` | 是,但只在它**有自己的档案**时单独计;跟随来源的条目不重复计费 |
| LiteBox 对外生成的订阅 | 用户的 `/sub/{token}` 与 `subscription_profiles` | **不是**。它们是我们发出去的东西,`expiry.Kind` 里根本没有这一种,计费结构也就没有位置可填 |

## 3. Dashboard、主题、API 与迁移约定

* 仪表盘是 `web/src/views/DashboardView.vue` 一个文件,各块独立取数、读失败统一进 `failedSources`;
  组件库是 `components/lb/*`(`LbUsageBar`、`LbQuotaBar`、`LbStatusTag`、`LbMetricCard`…);
* 令牌只在 `styles/tokens.css`(scoped CSS 不写十六进制色与 px 字号),AntD 的实值镜像在 `theme/tokens.ts`;
* 接口都在 `internal/httpapi`,`authed` 子路由统一走管理员会话;数组字段不得为 nil;
* 迁移是 `internal/database/migrations/NNNN_*.sql`,只追加、不改已应用的文件,LF 检出;
* 前端回归(`web/e2e`)全程不连后端,新增的每一个 GET 都要有固定数据,否则用例判失败。

## 4. 可用于主动探测的地址与端口

| 用途 | 地址 | 端口 |
|---|---|---|
| 管理通道 | `nodes.host`(唯一的管理地址,域名只查 A 记录) | `nodes.ssh_port` |
| 对外服务 | 订阅地址池:`host`(兜底)、`node_addresses`(V4 / V6,可为域名) | `inbound_endpoints` 的公网端口(0 = 跟随入口监听端口);中转规则的公网端口 |

两者分开登记、分开统计。探测目标**只能从这台机器已登记的地址 × 端口里选**(见 §7 风险 3)。

## 5. 续费与通知能否扩展

能。`expiry_profiles` 主键就是「对象种类 + id」,正好是资产的键;续费历史与提醒去重都已持久化。
兼容迁移只做 `ADD COLUMN ... DEFAULT`:存量行的新列全是「未设置」,旧节点照常显示,
到期提醒的行为一个字不变。

## 6. 缺口

1. 没有价格、币种、计费周期、本期起点、实际支付金额 —— P0-B 补;
2. 没有地区与标签 —— 新表 `asset_labels`(**不放进 `expiry_profiles`**:外部代理一旦有了自己的
   档案行就不再跟随来源,只想打个标签却改变了到期继承,是一个静默的副作用);
3. 没有任何主动网络探测 —— P0-C 新建 `probe_*` 三张表与后台任务;
4. 没有按币种的成本汇总、没有探测告警 —— P1。

## 7. 风险与兼容方案

1. **CLAUDE.md 明文规定外部代理的连通性检查只手动、不定时跑**(「定时跑出一堆红灯只会训练管理员忽略这个列表」)。
   兼容方案:定时 TCP 探测**只覆盖自建节点**;外部代理与代理源的卡片显示「仅手动检查」与上次手动检查的结果。
2. **对 sshd 的反复 TCP 建连会被 OpenSSH ≥ 9.8 的 `PerSourcePenalties` 记作 noauth。**
   每分钟一次的 1 秒惩罚远低于默认的 15 秒封禁门槛,但叠加部署拨测仍有风险 —— 管理通道目标不默认创建,
   界面上写明这一点;推荐优先探测对外服务端口。
3. **SSRF / 内网扫描**:目标不收自由输入,只存「引用」(管理通道 / 某条订阅地址条目),每轮按引用解析出
   当前的地址与端口。机器改了地址,引用跟着走、统计口径自动换代;引用失效(地址或入口被删)就停探并标「目标已失效」,
   绝不继续打一个可能已经属于别人的旧地址。
4. **升级后不能凭空开始打生产机器**:迁移不创建任何探测目标,由管理员在节点详情里逐台开启。
5. **单连接 SQLite**:探测结果每轮一个事务批量写;明细保留 7 天、小时汇总保留 90 天,各自定时清理。
6. **金额精度**:只存最小货币单位的整数,前后端都用十进制字符串交换,不经浮点。
7. **多币种**:不换汇、不出现跨币种的单一总计。

## 8. 数据模型草案

```
expiry_profiles  + plan_name, billing_cycle, cycle_count, cycle_unit, currency, price_minor,
                   purchased_at, period_start, period_paid_minor, period_carry_minor, traffic_unlimited
expiry_renewals  + amount_minor, currency, period_start
asset_labels     (kind, object_id) → region, tags
probe_targets    node_id, kind(MGMT/SERVICE), ref, family, label, source, enabled,
                 resolved_host/port, generation, alert_state, alert_notified, ...
probe_results    target_id, generation, source, at, status, rtt_us, dns_us, error   (明细 7 天)
probe_hourly     target_id, generation, hour, attempts, ok, dns_errors, skipped, rtt 统计  (90 天)
```

派生量(剩余未摊销价值、日均 / 月均成本)不入库,每次读取时现算。

## 9. 卡片字段清单

| 区块 | 字段 | 来源 |
|---|---|---|
| 头部 | 地区、名称、类型(落地 / 中转 / 外部代理 / 代理源)、自建 / 外部、运行状态、到期剩余天数 | `asset_labels`、`nodes`、`expiry` |
| 资源与流量 | CPU / 内存 / 磁盘、周期流量、实时 ↓/↑ | `node_metrics`、`NodesCycleUsage`、`upstream_*` |
| 网络质量 | 来源、目标、协议、最近 RTT、1h / 24h 失败率、点阵 | `probe_*` |
| 套餐资产 | 周期、金额与币种、下次续费 / 到期、自动续费、未摊销价值(按时间 / 按流量) | `expiry_profiles` + 现算 |
| 操作 | 详情、编辑套餐、续费、历史 | 复用 `ExpiryModal` 与节点详情 |

## 10. 分阶段修改清单

* **P0-A** 迁移 `0039`(asset_labels);`GET /api/dashboard/cards` 聚合接口;仪表盘卡片视图、筛选、排序、概览、视图切换;
* **P0-B** 迁移 `0039` 的计费列;`expiry` 包的计费字段、`Valuate` 估值、金额解析;续费带金额;`ExpiryModal` 的套餐区块;
* **P0-C** 迁移 `0040`(probe 三张表);`internal/probe`(目标白名单、拨号分类、调度、汇总、清理);卡片网络区块;
* **P1** 节点详情「网络监测」Tab(1h / 24h / 7d)、探测告警(连续失败 / 恢复 / 冷却 / 免打扰)、按币种成本汇总、到期提醒文案细化;
* **P2** 不实施:ICMP 需要额外权限、远端探测要占节点 SSH、三网需要真实测点。只在模型里预留 `source`,界面只显示真实存在的来源。
