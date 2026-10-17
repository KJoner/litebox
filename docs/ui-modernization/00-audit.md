# LiteBox UI 2.0 · 00 现状审计

> 审计日期:2026-10-17 · 分支 `feat/ui-2.0` · 基线提交 `165d0c0`(V20 优化完成后)
> 验证环境:本机 scratch 面板(`litebox serve`,端口 18100)+ Vite dev(5181),
> 数据是两台测试机 `sh-kurun` / `djj-ph`(Alpine / OpenRC / NAT)、1 个用户、1 条外部代理。
> 整个审计与改造过程**不对节点执行部署、重启、删除**,只读页面。

---

## 1. 技术栈与构建基线

| 项 | 现状 |
|---|---|
| 框架 | Vue 3.5 + TypeScript 5.7 + Vite 6 + Pinia 2 + Vue Router 4 |
| 组件库 | ant-design-vue 4.2.6(component token 停在 AntD v5.0 一代命名,见 CLAUDE.md「前端约束(V3)」) |
| 图表 | 无第三方库;`LbSparkline`(SVG 面积 / 柱)、`MetricsChart`(SVG 多序列,时间等距) |
| 图标 | 无图标库;`LbIcon` 手抄 43 个 Lucide 路径,`LbShapeIcon` 画 10 种状态形状 |
| 字体 | 系统字体栈 + 自托管 Noto Sans SC 分片(400/500/700),`dropWoffFallback` 剔除 woff |
| 测试能力 | 前端没有单元测试;质量门是 `vue-tsc --noEmit` + `vite build`;页面检查用 Chrome DevTools MCP(截图、视口模拟、执行脚本),未安装 Playwright |

**生产构建体积基线**(`npm run build`,2026-10-17,未改动前):

| 类别 | 原始 | gzip |
|---|---|---|
| JS 合计 | 2128.2 KB | 697.4 KB |
| 其中 `index-*.js`(AntD + Vue + 公共) | 1565.2 KB | 487.2 KB |
| CSS 合计 | 431.2 KB | 150.6 KB |
| 其中 `index-*.css` | 336.5 KB | 128.7 KB |
| 字体(woff2 分片) | 7125.0 KB | — |
| 全部产物 | 9684.8 KB(367 个文件) | 7981.2 KB |

体积控制目标:JS + CSS(不含字体)增长不超过基线的 25%,即 ≤ 3199 KB 原始。

---

## 2. 路由与页面清单

路由键、地址、守卫一个都不改;下表只记现状。`窄屏` 列表示是否有 `<768` 的卡片化处理(`useNarrow` / `LbRowCard`)。

### 2.1 管理后台(`MainLayout`)

| 路由 | 页面文件 | 模板 / 样式行数 | 窄屏 | 备注 |
|---|---|---|---|---|
| `/dashboard` | `DashboardView.vue` | 237 / 190 | 有 | V18 方案 B:h1 是一句话结论、指标条 44px |
| `/users` | `UsersView.vue` + `UserDetailDrawer.vue` | 387 / 104 · 497 / 358 | 有 | 批量操作、详情浮动抽屉(内有 Tabs) |
| `/nodes` | `NodesView.vue` | 464 / 188 | 有 | V20 七列 |
| `/nodes/:id/:tab?` | `NodeDetailView.vue`(3004 行) | 1339 / 670 | 部分 | 五个 Tab:overview / entries / traffic / metrics / deploys |
| `/inbounds` | `InboundsView.vue` | 386 / 61 | 有 | V20 统一入口,五种来源 |
| `/external-proxies` | `ExternalProxiesView.vue` | 333 / 145 | 有 | 源卡片 + 条目表 |
| `/subscription-profiles` | `SubscriptionProfilesView.vue` | 137 / 118 | 有 | |
| `/deployments` | `DeploymentsView.vue` | 197 / 91 | 有 | |
| `/audit-logs` | `AuditLogView.vue` | 184 / 137 | 有 | |
| `/settings` | `SettingsView.vue` | 533 / 337 | 部分 | 分组表单 + 云账号 + 订阅排序 |
| `/login` | `LoginView.vue` | 72 / 197 | 有 | 独立页面,无布局 |

节点详情各 Tab 的主要子组件:`NodeEntriesPanel`(2162 行)、`ServiceCard`、`MetricsChart`、`CloudInstanceCard`、
`NodeTuningPanel`、`DeployStepList`、`NodeOpProgressModal`,以及十余个入口 / 出口 / 排序弹窗。

### 2.2 用户门户(`PortalLayout`,独立壳)

| 路由 | 页面文件 | 模板 / 样式行数 |
|---|---|---|
| `/` | `PortalLoginView.vue` | 68 / 197 |
| `/user/dashboard` | `PortalDashboardView.vue` | 146 / 155 |
| `/user/subscription` | `PortalSubscriptionView.vue` | 168 / 161 |
| `/user/nodes` | `PortalNodesView.vue` | 112 / 155 |
| `/user/traffic` | `PortalTrafficView.vue` | 141 / 173 |
| `/user/security` | `PortalSecurityView.vue` | 97 / 124 |

### 2.3 弹窗与抽屉

`a-modal` 出现在 24 个文件里(27 处),`a-drawer` 3 处(用户详情、导航抽屉、门户导航)。
确认类走 `lbDangerConfirm`(`Modal.confirm` 封装)与 `LbNameConfirm`(打字确认)。
节点操作结果统一走 `NodeOpProgressModal`(跑的时候关不掉、跑完不自动关)。

---

## 3. 设计系统现状(V18 iOS 风格)

| 文件 | 职责 |
|---|---|
| `styles/tokens.css` | 浅 / 深两组 CSS 变量 + 12 个关键帧 |
| `theme/tokens.ts` | `palette`(实值,只给 AntD)+ `color`(全是 `var()` 引用)+ 阈值 |
| `theme/antd.ts` | `antdTheme(mode)`,深色换 darkAlgorithm |
| `styles/antd-tune.css`(936 行) | Token 够不到的部分:表格、卡片、按钮、输入、开关、分段式 Tabs、弹窗三栏头、浮动抽屉、胶囊吐司、Alert、Tooltip |
| `styles/base.css`(622 行) | 页面骨架类(`.lb-page*`、`.lb-card`、`.lb-metrics`、`.lb-group`、`.lb-kv`、`.lb-chip`、`.lb-notice`、`.lb-seg`、`.lb-filter`)与工具类 |
| `stores/theme.ts` | 只有 light / dark 两档;首次跟随系统,之后**不再跟随**系统变化 |

V18 的视觉特征(本次要替换的部分):

* 页面底 `#F5F5F7`(深色纯黑 `#000`),卡片**无边框**、只靠阴影浮起,圆角 20 / 弹窗 22;
* 按钮、导航项、筛选 chip 全是 999 胶囊;选中的导航项是**实底品牌蓝 + 白字**;
* 页面标题 34px / 700,指标数字 44px / 700,与数据密集的运维场景不匹配(首屏能放的信息少);
* 顶栏与吐司是毛玻璃,吐司带弹跳入场(`lb-banner`);
* 弹窗用 CSS `:has()` 把「取消 / 确定」搬到标题行做成 iOS 三栏头;抽屉离视口四边 12px 浮动;
* Tabs 全局被重画成分段控件(包括页面级的节点详情五个 Tab)。

---

## 4. 公共组件(`components/lb`)

| 组件 | 职责 | 被引用次数* |
|---|---|---|
| `LbStatusTag` + `statusMeta.ts` | 状态三重编码(形状 + 文案 + 颜色),十余张状态表 | 110 |
| `LbInfoTip` | ⓘ 帮助气泡,Teleport 到 body,可钉住 | 87 |
| `LbEmptyState` | 首次为空 / 筛选为空 / 错误三态 | 72 |
| `LbTimeText` | 相对时间 + 过期着色 | 54 |
| `LbIcon` | 线性图标 | 39 |
| `LbSectionTitle` | 卡片外的分组标题 | 32 |
| `LbMetricCard` | 指标格(ready / loading / empty / error 四态,error 不回退成 0) | 27 |
| `LbRowCard` | 窄屏行卡片外壳 | 24 |
| `LbQuotaBar` | 额度条(不限量不画、读失败显示 —) | 21 |
| `LbCopyField` / `LbSensitiveField` | 复制 / 打码字段 | 14 / 5 |
| `LbSparkline` | 面积 / 柱状迷你图,缺失日 null 虚线跨过 | 13 |
| `LbFilterBar` / `LbBatchBar` | 表格工具条 / 批量栏 | 12 / 6 |
| `LbNameConfirm` / `lbDangerConfirm` / `LbResultList` | 危险确认两档 + 批量结果 | 11 / — / 4 |

\* 含 import 与模板引用,只用来判断影响面。

页面骨架类的引用:`lb-group__row` 49、`lb-btn-ghost` 33、`lb-card` 26、`lb-chip` 19、`lb-page*` 14 页。
**结论:视觉改造的杠杆在令牌、`antd-tune.css`、`base.css` 与 lb 组件**——这四处改对,
绝大多数页面不用逐页覆盖样式;逐页工作集中在布局与信息层级。

---

## 5. 硬编码与不一致

| 问题 | 数据 | 处理 |
|---|---|---|
| 字号全是写死的 px | `font-size` 声明 500+ 处、23 种取值(10 / 10.5 / 11 / 11.5 / 12 / 12.5 / 13 / 13.5 / 14 / 15 / 16 / 17 / 18 / 19 / 20 / 22 / 26 / 28 / 30 / 32 / 34 / 36 / 44) | 建立 8 级字号令牌,脚本把散落值映射到令牌(见 01) |
| 圆角半数走变量、半数写死 | 变量 128 处;写死 9 / 8 / 7 / 5 / 16 / 10 / 6 / 17 / 14 / 11 / 4 / 3 px 共 40 处 | 写死值收进 `--r-*` |
| `--r-pill` 语义混用 | 同一个变量既给状态标签(应保留胶囊)又给导航项、按钮、筛选 chip(应改成轻圆角) | 新增 `--r-btn`,`--r-pill` 只留给标签与计数徽标 |
| 十六进制色 | 令牌外仅 12 处(`InboundFormModal` 3、登录页 2、`LbInfoTip` 2 等) | 收进令牌 |
| `rgba()` 字面量 | `MainLayout` 4、两个登录页各 3 | 收进令牌 |
| 状态色阴影写死浅色值 | `ml__sync-dot` 的 `rgba(27,122,75,.15)` 在深色模式下不对 | 改用 `--ok-bg` / `--bad-bg` |

---

## 6. 现状问题(改造前截图里看得到的)

截图在 `screenshots/before/`(1440 × 900 浅色 / 深色,390 × 844 手机;DPR 1)。

1. **首屏信息密度低**:仪表盘在 1440 × 900 下,首屏只放得下一句话标题、四个 44px 数字和一张图,
   「哪些节点要处理」要往下滚一屏才看得到;节点健康是一列竖排,看不到 CPU / 内存 / 磁盘。
2. **节点详情 390 宽有横向溢出**:`innerWidth` 被撑到 423(`node-overview-light-390.webp`),
   概览里某张卡片的最小宽度没有让步。
3. **选中导航是整块实底蓝**,在一屏里是最重的元素,抢走了页面内容的注意力。
4. **深色模式底色纯黑 `#000` + 卡片 `#1C1C1E`**,两层之间对比靠阴影(深色下几乎看不见),
   表格与卡片边界模糊。
5. **弹窗三栏头**依赖 `:has()` 判断「恰好两枚按钮」,一旦某个弹窗底栏多一个按钮,
   版式就整个切回另一套,同一类弹窗出现两种形态。
6. **侧栏同步状态的说法前后矛盾**:刚重启面板时显示「1 个节点同步失败 · 上次 尚未运行」——
   失败数来自持久化的上一轮结果、时间来自本进程。这是数据层的既有现象,不在本次 UI 范围,记录在案。
7. **主题只有两档**:首次跟随系统,之后系统切换深浅色时面板不跟;需求要求「浅色 / 深色 / 跟随系统」三档。

---

## 7. 改造风险

| 风险 | 说明 | 对策 |
|---|---|---|
| 业务约束写在 CLAUDE.md 里 | V3 / V18 / V19 / V20 有大量「为什么这样显示」的规则(状态三重编码、读不到不显示 0、运行与配置两个标签、进度弹窗关不掉…) | 只换视觉与布局;每批自检时对照这些条款 |
| 全局 CSS 牵一发动全身 | `antd-tune.css` / `base.css` 的一条规则影响 24 个弹窗、11 张表 | 每批完成后逐页截图核对 |
| `:has()` 三栏头移除 | 标准两键弹窗回到底栏,右上角 × 重新出现 | 一次性在全局处理,不逐个弹窗改 |
| 字号令牌化是机械批量替换 | 500+ 处 | 用脚本做确定性映射,替换后逐页看 |
| 节点详情体量大(3004 + 2162 行) | 一次改太多不好回滚 | 先改全局与骨架,再逐 Tab 改,单独提交 |
| 构建体积 | 不引新依赖,只增图标路径与少量组件 | 每批对比 `dist` 体积 |
