# LiteBox UI 2.0 · 交付总览

> 分支 `feat/ui-2.0`,基线提交 `165d0c0`。五批各自独立提交,可以逐批回滚。
> 目标:把 V18 的 iOS 风格换成 **Modern SaaS Dashboard + Infrastructure Console**,
> 只动视觉、布局与信息层级,不动路由、API、权限与业务口径。

| 文档 | 内容 |
|---|---|
| [00-audit.md](00-audit.md) | 现状审计:技术栈、构建体积基线、页面清单、lb 组件映射、硬编码统计、现状问题与风险 |
| [01-design-system.md](01-design-system.md) | 设计规范:令牌(颜色 / 字号 / 间距 / 圆角 / 阴影 / 动效)与组件规格,附与 V18 的差异 |
| [02-page-plan.md](02-page-plan.md) | 五批任务拆分、数据源与不变量清单 |
| [03-progress.md](03-progress.md) | 逐批交付记录:改了什么、关键决策、构建、自检、遗留 |
| `screenshots/before/` · `screenshots/after/` | 改造前 25 张 / 改造后 39 张(DPR 1,WebP) |

## 提交

| 批次 | 提交 | 内容 |
|---|---|---|
| 1 设计基线 | `7c8fbd8` | 审计、设计规范、页面计划、改造前截图 |
| 2 设计基础 | `f6d42a3` | 令牌、主题三档、AntD 主题与覆盖层、骨架类、lb 组件、导航壳 |
| 3 核心页面 | `443bfd1` | 仪表盘、自建节点、节点详情五个 Tab |
| 4 其余页面 | `461d568` | 用户、入口、外部代理、订阅配置、部署记录、审计、设置、登录、门户 |
| 5 全面验收 | 见 git log | 状态覆盖、多视口截图、溢出检查、清理、CLAUDE.md |

## 改造前后对照

| 页面 | 改造前 | 改造后 |
|---|---|---|
| 仪表盘 | ![](screenshots/before/dashboard-light-1440.webp) | ![](screenshots/after/dashboard-light-1440.webp) |
| 仪表盘 · 深色 | ![](screenshots/before/dashboard-dark-1440.webp) | ![](screenshots/after/dashboard-dark-1440.webp) |
| 自建节点 | ![](screenshots/before/nodes-light-1440.webp) | ![](screenshots/after/nodes-light-1440.webp) |
| 节点详情 · 概览 | ![](screenshots/before/node-overview-light-1440.webp) | ![](screenshots/after/node-overview-light-1440.webp) |
| 节点详情 · 入口与转发 | ![](screenshots/before/node-entries-light-1440.webp) | ![](screenshots/after/node-entries-light-1440.webp) |
| 节点详情 · 资源 | ![](screenshots/before/node-metrics-light-1440.webp) | ![](screenshots/after/node-metrics-light-1440.webp) |
| 用户管理 | ![](screenshots/before/users-light-1440.webp) | ![](screenshots/after/users-light-1440.webp) |
| 入口管理 | ![](screenshots/before/inbounds-light-1440.webp) | ![](screenshots/after/inbounds-light-1440.webp) |
| 系统设置 | ![](screenshots/before/settings-light-1440.webp) | ![](screenshots/after/settings-light-1440.webp) |
| 登录 | ![](screenshots/before/admin-login-light-1440.webp) | ![](screenshots/after/admin-login-error-light-1440.webp) |
| 门户概览 | ![](screenshots/before/portal-dashboard-light-1440.webp) | ![](screenshots/after/portal-dashboard-light-1440.webp) |
| 节点列表 · 手机 | ![](screenshots/before/nodes-light-390.webp) | ![](screenshots/after/nodes-light-390.webp) |
| 节点详情 · 手机 | ![](screenshots/before/node-overview-light-390.webp) | ![](screenshots/after/node-overview-light-390.webp) |

改造后的仪表盘、节点列表、用户列表截图里有一部分行是**浏览器里注入的模拟数据**
(离线、部署失败、待部署、停发订阅、即将到期 / 已到期 / 自动续费、额度告警 / 耗尽、CPU 打满、
采样过期、服务未运行、部署进行中 / 失败 / 已回滚),用来覆盖测试环境里造不出来的状态;
模拟只在页面内拦截 `fetch`,没有改动任何节点或数据库。错误态截图(`state-*`)同理。

## 验收

| 项 | 结果 |
|---|---|
| 类型检查 / 生产构建 | `vue-tsc --noEmit`、`vite build` 通过;`go build ./...` 通过(前端产物照常嵌入) |
| 构建体积 | JS 2153.8 KB(基线 2128.2,**+1.2%**),CSS 451.3 KB(基线 431.2,**+4.7%**),字体不变,**没有新增依赖** |
| 视口 | 1440 / 1280 / 768 / 390 / 375;390 与 375 下全部后台路由 `scrollWidth` 等于视口宽(无横向溢出) |
| 主题 | 浅色 / 深色 / 跟随系统;深色抽查仪表盘、节点列表、节点详情、入口管理、门户流量 |
| 状态 | 在线 / 离线 / 待初始化 / 数据过期 / 变更前数据;运行与配置不一致;即将到期 / 已到期 / 自动续费;额度正常 / 告警 / 耗尽 / 不限;空数据;整页加载失败;部分接口失败;部署进行中 / 成功 / 失败 / 已回滚 |
| 交互 | 新增节点、续费、删除节点(名称确认,只打开不执行)弹窗;导航抽屉;登录错误与走错门导流 |
| 业务不变量 | 路由键与地址、API、确认档次、进度弹窗规则、「读不到不显示 0」、运行 / 配置两个标签、门户无技术字段 —— 逐批自检,未改动 |

## 顺带修正的既有问题

* 仪表盘把代理源 / 外部代理的预警当成节点,跳到 `/nodes/<源的 id>`。
* 审计日志把认不出前缀的动作全部归为「登录」,且 70 多个动作没有中文名。
* 节点详情在 390 宽有横向溢出;手机上页面头部的按钮组把最后一颗挤出屏幕。
* `size="small"` 表格的首末列 16px 内距一直没生效。
* `LbSparkline` 每个实例的渐变 id 都是 `lb-spark-1`。
* 开发代理把 `/subscription-profiles` 转给后端,开发环境下这一页打开是旧前端。

## 遗留

* **768 宽(平板竖屏)的大表格**仍是桌面表格 + 横向滚动(节点名与操作列固定在两侧);
  卡片化的断点是 <768,与全站 `useNarrow` / 媒体查询一致,没有为这一档单独调整。
* 节点详情里的各类结果弹窗(探测、配置比对、TCP 调优、引导)只换了全局规格,没有逐个重排版式。
* 门户「我的节点」每个节点的「本月 / 累计」与概览里「本月流量」口径不同(账本 vs 额度周期用量),
  是既有的数据口径,不在 UI 范围。
* 侧栏底部「1 个节点同步失败 · 上次 尚未运行」在面板刚重启时前后矛盾(失败数与时间来自两个来源),
  属于数据层,记录在案。
* 没有引入 Playwright;页面检查用 Chrome DevTools MCP 完成(截图、视口模拟、页内脚本),
  没有沉淀成可重复运行的端到端用例。
