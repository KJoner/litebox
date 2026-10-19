# Boce 外部入站拨测与卡片展示自定义 · 交付说明(V22.3)

需求:`docs/开发计划/v21/探针系统增强.md`;方案:`06-boce-plan.md`。分支 `feat/boce-ingress-probe`(基于 master 3540481)。

**最重要的四条,都由测试钉着:** 未明确启用不花钱;任务配置控制实际检测;卡片配置只控制展示;
第三方故障不污染原有探针数据。

## 一、做了什么

| 块 | 内容 |
|---|---|
| Boce 客户端 `internal/boce` | 拨测点列表(大陆 / 海外)、创建 TCPing、取结果、余额;地址写死;不跟随跳转;响应限 2 MiB、逐字段校验;错误里擦掉 Key 与带 Key 的 URL;创建请求分「确定没发出去 / 发出去了但结果不确定 / 被明确拒绝」三种 |
| 调度 `boce_schedule.go` | 固定间隔(从窗口起点对齐,跨午夜)、固定时间点、星期、每任务时区;错峰偏移 ≤ 3 分钟且不越过窗口终点;错过的时间槽只补 10 分钟内的最近一个 |
| 任务 `boce_tasks.go` | 一个任务 = 一个订阅入口 + 若干拨测点 + 时间表;每个「任务 × 拨测点」一条入站序列(`source = BOCE`);改拨测点只停用 / 启用序列,归档停用全部序列,删除才删历史 |
| 执行与预算 `boce_runs.go` | 幂等键 + 预算预留在同一个事务里(四道:单次、单任务月、日、月);结果按「Boce 任务 ID + 拨测点 ID」去重落库;不扣费的拨测点(not found node)结算时退回 |
| 引擎 `boce_engine.go` | 每 5 秒一拍:取结果(10 秒一次、2 分钟收尾)→ 建到期的任务 → 余额 30 分钟 / 拨测点 24 小时刷新(只在启用后)→ 清理;重启时停在「创建中」的记「不确定」不重发;鉴权失败、波点不足、连续 5 次接口异常自动暂停;预算 80% / 用完、低余额、接口异常与恢复推送 |
| 估算 `boce_estimate.go` | 与调度同一个函数数时间槽,按真实日历月;含其余在跑任务的全站合计、按余额还能跑几天、三道上限会不会被顶到 |
| 卡片偏好 `cardprefs.go` | 每台节点入站 / 出站各 AUTO / MANUAL、0~3 项;自动推荐优先在测且有效、覆盖不同运营商 / 地区;手动选的序列删了单列「失效」,不补位 |
| 设置 `settings/boce.go` | Key 主密钥加密;默认关、日 / 月上限默认 0(= 没有设,不建付费任务);单价与每点波点可改 |
| 接口 `httpapi/boceapi.go` | 见 `docs/API说明.md`「Boce 外部入站拨测与卡片展示设置」;全部在管理端会话下,审计 14 种动作 |
| 前端 | 系统设置「Boce 外部拨测」;节点详情「网络监测」加来源 / 关键词筛选、Boce 任务区(任务、每个拨测点一行、手动检测、执行记录、新建 / 编辑 / 复制 / 归档 / 删除)、「卡片展示设置」;节点列表批量应用;仪表盘卡片按偏好显示入站 / 出站各 3 项;「需要处理」收 Boce 连续失败;审计日志动作名 |

改动文件:后端新增 `internal/boce/`、`internal/probe/boce_*.go`、`internal/probe/cardprefs.go`、
`internal/settings/boce.go`、`internal/httpapi/boceapi.go`、迁移 `0043_boce_ingress.sql`;修改 `internal/probe/`
的 `probe.go`(来源 / 口径 / 三种新状态)、`results.go`(三个新计数)、`targets.go`(入站列表不含 Boce、Boce 序列
不能单独改删)、`engine.go`(主控引擎只取 `source = MASTER`)、`summary.go`、`nodesettings.go`;`notify`(两种事件)、
`httpapi` 的 `server.go` / `probeapi.go` / `assetcards.go`、`cmd/litebox/main.go`。前端新增 `web/src/components/boce/`
六个组件与 `probe/CardDisplayModal.vue`,修改 `client.ts`、`probeView.ts`、`ProbeDots` / `ProbeChart` / `ProbeRow`、
`AssetCard` / `AssetCardsView` / `assetModel.ts`、`NodeNetworkPanel.vue`、`SettingsView` / `NodesView` /
`DashboardView` / `AuditLogView`。

## 二、几条关键规则怎么落地

* **默认不花钱**:`BoceConfig.BlockReason` 是唯一判据(开关、Key、日 / 月上限、自动暂停),定时与手动都过它;
  关着时连余额、拨测点列表都不主动刷新。`TestBoceNothingIsSentUnlessExplicitlyEnabled` 断言四种情况下调用次数为 0;
* **一轮一次 Boce 任务**:同一目标这一轮的全部拨测点放在一次 `create/tcping` 里;
* **不重复扣费**:`boce_runs.idem_key` 唯一;预留与插入同一个事务(数据库单连接,事务串行);
  `httptrace.WroteRequest` 区分「没发出去」(最多重试 3 次,不扣费)与「发出去了没拿到可信响应」(记 UNKNOWN、
  按已扣费算、绝不重发);面板在创建中重启同样记 UNKNOWN。测试覆盖超时、并发、重启、重复请求 ID;
* **结果只保留 5 分钟**:拿到一个拨测点就落库;任务 ID 失效记 EXPIRED;
* **状态分开**:目标连不上 = `TIMEOUT`(进比例、进告警);部分丢包 = `LOSS`;拨测点异常 = `VANTAGE_ERROR`、
  接口异常 = `API_ERROR`、没取得结果 = `EXPIRED`、预算 / 关机 = `SKIPPED`,这四种不进分母、不改连续失败计数、
  点阵上画成灰色点线框;
* **口径分开**:Boce 序列 `metric = BOCE`,界面叫「TCPing 丢包率(Boce)」,丢包率由 `packets_transmitted /
  packets_received` 自己算(上游 `packet_loss` 的单位文档没写,只存档);主控仍叫「TCP 连接失败率」、节点 ICMP 仍叫「丢包率」;
* **IPv6**:TCPing 文档没有说明支持 IPv6,IPv6 入口在选择器里列出但选不了、写明原因,不改用 IPv4;
* **被测目标只能是这台节点的订阅入口**(与入口连通性同一份白名单,不含管理通道),地址与端口每轮由服务端解析,
  变了就换代;
* **卡片只控制展示**:`PUT /api/nodes/{id}/probe-card` 只写 `probe_card_prefs`;接口测试与 Playwright 都断言
  改展示时 Boce 的调用次数不变。

## 三、验证

全部用假的 Boce(`httptest` / 测试替身)与临时库,**没有调用真实 Boce 接口**。

| 范围 | 结果 |
|---|---|
| `go test ./internal/...` | 全部通过。新增 `internal/boce`(8 个:两区列表与坏行、Key 不进错误、创建成功 / 拒绝 / 本地拦截、不确定的四种形态、文档示例解析、字段校验、跳转拒绝)、`boce_schedule_test.go`(16 轮 / 天、夜间 0 轮、10 月 496 轮、跨午夜与星期、固定时间点、校验、错峰不越界)、`boce_engine_test.go`(17 个:默认不花钱、完整链路与去重、一天 16 轮且夜间无任务、5 台独立配置、预算拦截与并发原子性与幂等、不确定不重发、重启恢复、拨测点异常不算丢包、鉴权自动暂停、连续异常自动暂停、超时与失效、关机不花钱、任务规则与历史保留、换地址换代、估算 80 / 2400 波点与 ¥4.80 / ¥28.80、卡片偏好、拨测点同步保留缓存)、`boceapi_test.go`、`settings/boce_test.go`、`database/boce_upgrade_test.go` |
| Playwright 功能用例 | 125 个通过(+8:`boce.spec.ts`;`assets.spec.ts` 按新的入站分组改了两条) |
| 截图基线 | 42 张通过;8 张更新(卡片视图与网络监测 Tab 的各宽度、深色)、4 张新增(Boce 设置浅 / 深、新建任务弹窗、卡片展示设置),逐张看过 |
| 升级冒烟 | master 二进制建 v42 库并灌入入口 / 出站序列与 500 条明细 → 本分支迁移到 v43:数据一行不少、外键与完整性检查通过、没有 Boce 任务与设置;master 二进制再打开 v43 库照常通过 |
| 启动冒烟 | 新二进制 `serve`:健康检查 200,日志「Boce 外部拨测引擎已启动(未启用时不发任何付费请求)」,无错误 |
| 构建 | `go vet`、`gofmt`、`vue-tsc`、`vite build`、e2e 类型检查通过 |

**还没做的:真实 Boce 接口验证。** 需要管理员授权并提供 Key,用 1 个拨测点、1 个目标、最少次数跑一遍
(校验 → 同步 → 建任务 → 手动检测一次 → 看结果与执行记录),并记录实际扣的波点。几个只有真接口才能确认的点:
`packet_loss` 的单位、TCPing 每轮发几个包、`done=false` 时是否会先返回部分结果、实际单价。

## 四、配置与使用

1. 系统设置 → Boce 外部拨测:填 API Key(只在服务端加密保存,页面只显示末 4 位)→ 校验连通性 → 同步节点列表;
2. 设每日 / 每月波点上限(0 = 不建任何付费任务)、单次上限(默认 10)、提醒线(默认 80%)、低余额门槛、预算时区;
   单价默认每拨测点每次 1 波点、¥0.002 / 波点,只用于估算;
3. 打开「启用 Boce 外部拨测」(会先确认,写审计);
4. 节点详情 → 网络监测 → 新建拨测任务:选订阅入口、拨测点(大陆 / 海外、三网快捷筛选)、时间表(默认
   Asia/Shanghai、08:00–24:00、每 60 分钟),看下方估算;保存后默认不开自动检测,确认费用后再打开;
5. 多台节点:节点列表勾选 → 批量操作「Boce 拨测任务」,每台各建一个,之后可以单独改;
6. 卡片上显示哪几项:网络监测 → 卡片展示设置(入站 / 出站各最多 3 项,自动推荐或手动选择)。

推送:设置 → 监控与推送里多了「Boce 外部拨测异常与恢复」「Boce 预算与余额提醒」两种事件(默认随「全开」一起收)。

回滚到 V22.2 之前先把 Boce 任务全部归档,见 `docs/升级说明.md`。
