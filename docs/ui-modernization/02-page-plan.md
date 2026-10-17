# LiteBox UI 2.0 · 02 页面改造计划

> 按批推进,每批一个(或几个)独立提交,做完一批就构建、截图、自检,随时可以回滚到上一批。
> 每批的结果记在 `03-progress.md`。

## 不变量(每批自检都要对照)

* 路由键、地址、API、权限判断、守卫一个不改;节点详情五个 Tab 的路由键
  (`overview` / `entries` / `traffic` / `metrics` / `deploys`)不改。
* 业务口径不改:流量折算、周期边界、排序算法(`entryOrder.ts` / `sortByEntryOrder`)、
  有效节点、状态判定(`statusMeta.ts`、`derive.ts`、`healthSummary.ts`、`serviceState.ts`)。
* 读不到 → 显示「—」与原因,**绝不显示成 0**;缺失样本 `null`,不补 0、不插值。
* 运行状态与配置状态仍是两个独立标签;服务巡检、连接待验证、变更前数据、已停机各自保留原语义。
* 危险操作的确认档次不变(普通确认 / `lbDangerConfirm` / `LbNameConfirm`);
  `NodeOpProgressModal` 跑的时候关不掉、跑完不自动关、关掉之后才重拉数据。
* 门户不出现管理 IP、技术字段、主机资源。

---

## 第一批 · 设计基线(本批)

* 源码审计、页面清单、组件映射、构建体积基线 → `00-audit.md`
* 设计规范 → `01-design-system.md`
* 改造前截图 → `screenshots/before/`(1440 浅 / 深、390 手机,DPR 1,WebP)

## 第二批 · 设计基础

| 任务 | 文件 | 要点 |
|---|---|---|
| 令牌 | `tokens.css`、`theme/tokens.ts` | 新色板、8 级字号、间距、圆角(新增 `--r-btn` 等)、阴影、动效、布局尺寸 |
| 主题三档 | `stores/theme.ts`、`App.vue` | `light / dark / system`;`system` 监听 `prefers-color-scheme` 变化;AntD 与 CSS 读同一个 `resolved` |
| AntD 主题 | `theme/antd.ts` | 字号 14、控件 36、圆角 8 / 12 / 6、按钮去胶囊 |
| 全局覆盖 | `antd-tune.css` | 表格、按钮、输入、下划线 Tabs、分段控件、弹窗(去三栏头)、贴边抽屉、卡片吐司、Alert、反色 Tooltip、下拉 |
| 骨架类 | `base.css` | `.lb-page*`、`.lb-card*`(+ `__head`)、`.lb-metrics`(独立卡片网格)、`.lb-group`、`.lb-chip`(矩形)、`.lb-notice`、`.lb-seg`、`.lb-filter` |
| 字号令牌化 | 全部 `.vue` 的 scoped CSS | 脚本把 `font-size: Npx` 按 01 §3 的表映射到 `var(--fs-*)`;写死的圆角映射到 `var(--r-*)` |
| lb 组件 | `LbMetricCard`(独立卡片 + 图标)、`LbStatusTag`、`LbSectionTitle`、`LbEmptyState`、`LbQuotaBar`、`LbRowCard`、`LbFilterBar`、`LbBatchBar`、`LbInfoTip`、`LbIcon`(补图标) | 只改视觉与少量 props,调用方不用改 |
| 导航壳 | `MainLayout.vue` | 新侧栏(分组、轻选中、手动折叠 + Tooltip)、新顶栏(折叠按钮、面包屑、主题菜单、用户菜单)、手机抽屉 |

验收:所有现有页面能正常渲染、入口都在;深浅主题与跟随系统都正确;`vue-tsc` / `vite build` 通过。

## 第三批 · 核心页面(重点打磨)

### 仪表盘 `DashboardView.vue`

| 行 | 内容 | 数据源(全部是现有接口) |
|---|---|---|
| 头 | 标题「仪表盘」+ 一句话结论(有 N 件事要处理 / 今天没有需要处理的事)+ 刷新 | `dashboardAlerts`、`dashboardSummary` |
| 1 | 四张指标卡:在线节点、有效用户、本月流量、需要处理 | `dashboardSummary`、`nodes`、`deployments` |
| 2 | 左 2/3 全站流量趋势(7 / 30 / 90 天);右 1/3「需要处理」:告警 + 即将到期 + 近 7 天失败部署,空时写「一切正常」 | `siteDailyTraffic`、`dashboardAlerts`、`nodes[].expiry`、`deployments` |
| 3 | 节点健康表(Beszel 式):节点 + 运行 / 配置状态、CPU / 内存 / 磁盘细条、网速、本周期流量、巡检结论、到期 | `nodes`、`nodeMetricsLatest`、`nodesCycleTraffic`、`nodeHealth`、`trafficStatus` —— **只读库里已有的采样,不触发 SSH** |
| 4 | 最近部署(6 条) | `deployments` |

### 自建节点 `NodesView.vue`

保留七列与全部语义;节点名加粗放大、状态标签精简;流量列「数值 + 细条 + 重置时间」;到期列「日期 + 剩余天数 + 自动续费标记」;
操作「主操作 + ···」;搜索 / 筛选 / 排序用新工具条;手机卡片重排。排序仍是 `sort_order` + id 兜底。

### 节点详情 `NodeDetailView.vue`

* 头部:返回、内部名称、编号、运行 / 配置状态、主操作,一行收齐;下划线 Tabs。
* 概览:身份与地址 / 服务现状 / 到期续费 / 云实例 卡片两列,标题与键值层级统一。
* 入口与转发:四张服务卡(`ServiceCard`)按云控制台的「资源卡」重排,入口列表表头与行统一。
* 流量:本周期用量卡(数值 + 进度 + 重置时间)+ 图表卡。
* 资源:Beszel 式四宫格(CPU / 内存 / 磁盘 / 网速当前值 + 细条)+ 趋势图(`MetricsChart`)。
* 部署与记录:配置版本卡 + 部署记录表。
* 修掉 390 宽的横向溢出。

## 第四批 · 其余页面

| 页面 | 要点 |
|---|---|
| 用户管理 + 用户详情抽屉 | 列表、额度条、到期、批量栏统一;抽屉信息分组 |
| 入口管理 | 类型图标 + 文字;来源 / 协议 / 状态 / 排序分层;排序弹窗区分旧方案与全局方案 |
| 外部代理 | 源卡片、条目表;三种到期时间分开显示;自动续费只是登记状态 |
| 订阅配置、部署记录、审计日志 | 表格 / 筛选 / 详情统一 |
| 系统设置 | 分组导航 + 表单卡片统一 |
| 云账号与云实例 | 卡片与进度统一 |
| 登录页(管理 / 门户) | 简洁居中卡片,去掉装饰性渐变 |
| 门户六页 | 独立壳,简洁易用,不出现管理信息 |

## 第五批 · 全面验收

* 类型检查、构建、体积对比(目标:JS + CSS 增长 ≤ 25%)。
* 视口 1440 / 1280 / 768 / 390 / 375 × 浅 / 深截图,存 `screenshots/after/`。
* 状态覆盖:在线 / 离线 / 未知 / 数据过期、运行与配置不一致、即将到期 / 已过期 / 自动续费、
  额度正常 / 告警 / 耗尽、空数据、加载失败、部分接口失败、部署进行中 / 成功 / 失败。
  测试数据不足的状态,用页面内脚本拦截 `fetch` 注入只存在于浏览器里的模拟响应来截图,
  **不为了截图去改真实节点或数据库**。
* 清理失效样式与重复代码;更新 CLAUDE.md 的前端约束。
