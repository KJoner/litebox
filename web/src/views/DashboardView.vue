<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import {
  api,
  type DailyPoint,
  type DashboardAlert,
  type DashboardCards,
  type DashboardSummary,
  type DeploymentRecord,
  type ExpiryKind,
  type ExpiryView,
  type ExternalProxy,
  type Node,
  type NodeCycleUsage,
  type NodeHealth,
  type NodeMetrics,
  type ProxySource,
} from '@/api/client'
import { formatBytes, formatRate, formatUTCDay } from '@/utils/format'
import {
  LbEmptyState,
  LbIcon,
  LbInfoTip,
  LbMetricCard,
  LbQuotaBar,
  LbSparkline,
  LbStatusTag,
  LbTimeText,
  LbUsageBar,
  configStatusMeta,
  subscriptionOffMeta,
  type LbIconName,
  type LbPoint,
} from '@/components/lb'
import { configState, isMetricsStale, isPreChange, nodeBadges } from '@/components/lb/derive'
import { summarizeHealth } from '@/components/node/healthSummary'
import { expiryStatusMeta } from '@/components/expiry/expiryMeta'
import ExpiryModal from '@/components/expiry/ExpiryModal.vue'
import AssetCardsView from '@/components/asset/AssetCardsView.vue'
import { buildAssetCards, familyText, type AssetCard } from '@/components/asset/assetModel'
import { metricLabel } from '@/components/asset/probeView'
import { useNarrow } from '@/composables/useNarrow'

/**
 * 仪表盘(UI 2.0)。首屏要回答的只有一个问题:**哪些东西需要我去处理。**
 *
 *   头部      一句话结论(有 N 件事要处理 / 没有需要处理的事)
 *   指标卡    在线节点 · 有效用户 · 本月流量 · 需要处理
 *   中间一行  左:全站流量趋势;右:「需要处理」清单(告警 + 商家到期 + 失败部署)
 *   节点健康  Beszel 式的表:运行 / 配置状态、CPU / 内存 / 磁盘细条、网速、本周期流量、巡检
 *   最近部署
 *
 * 各块独立取数、各自降级 —— 原来是一个 Promise.all 加一个 message.error,任何一个接口
 * 挂掉整页都只弹一条三秒吐司,而卡片渲染的是 `summary?.traffic_month ?? 0`,页面稳稳地
 * 显示「本月流量 0 B」。读不到和真的是零长得一模一样,这是最容易骗到管理员的一种失败。
 *
 * **任何一个来源读失败,结论都不能是「一切正常」**(UI 2.0.1)。原来只有预警读失败才会
 * 改口,而节点(含商家到期)、部署记录、代理源与外部代理的到期读失败时,清单少了那几类
 * 照样说「没有需要处理的事」。现在失败的来源统一收进 failedSources:顶部一条提示列出
 * 是哪几项、给一个只重试它们的按钮;已经读到的严重项照常排在前面,结论仍以它们为准。
 * 刷新时某一项读失败,保留上一次读到的结果 —— 一条已知的「已到期」不该因为一次网络抖动
 * 从清单里消失。
 *
 * **只读库里已有的采样与巡检结果,不触发任何 SSH**(CLAUDE.md「仪表盘节点卡片只读数据库」)。
 *
 * 资产卡片(UI 2.0 之后):页面有两种视图,「卡片」与「原有视图」,偏好存在本机。卡片视图把
 * 节点、外部代理、代理源各画成一张卡(资源、流量、TCP 探测、套餐资产),要的数据与原有视图
 * 是同一批批量接口,外加一次 /api/dashboard/cards —— 不按卡片逐个请求,也不触发探测。
 */
const router = useRouter()
const narrow = useNarrow()

/** 视图偏好:卡片 / 原有视图。与主题、侧栏折叠一样存在本机;读写失败(隐私模式)就用默认。 */
type DashView = 'cards' | 'classic'
const VIEW_KEY = 'lb.dashboard.view'
function readView(): DashView {
  try {
    return localStorage.getItem(VIEW_KEY) === 'classic' ? 'classic' : 'cards'
  } catch {
    return 'cards'
  }
}
const view = ref<DashView>(readView())
watch(view, (v) => {
  try {
    localStorage.setItem(VIEW_KEY, v)
  } catch {
    // 存不下就只在这一次会话里生效。
  }
})
/** 节点健康那张栅格最小 980px:平板宽度下与其横向滚,不如直接用手机那套卡片。 */
const compactNodes = useNarrow(1023)

const summary = ref<DashboardSummary | null>(null)
const summaryError = ref(false)

const alerts = ref<DashboardAlert[]>([])
const alertsError = ref(false)

const nodes = ref<Node[]>([])
const cycles = ref<Record<number, NodeCycleUsage>>({})
const metrics = ref<Record<number, NodeMetrics>>({})
const health = ref<Record<number, NodeHealth>>({})
const syncErrors = ref<Record<number, string>>({})
const nodesError = ref(false)
/** 列级数据读不到时只让那一列显示「—」,并在表头上方说出来(V3「列级失败不能升级成整表失败」)。 */
const columnErrors = ref<string[]>([])

const deploys = ref<DeploymentRecord[]>([])
const deployError = ref(false)

/** 外部代理与代理源的商家到期。它们不在 dashboardAlerts 里(那边只管可用截止时间)。 */
const vendorExpiry = ref<{ key: string; name: string; cat: string; to: string; v: ExpiryView }[]>([])
/** 原始列表,卡片视图要用(外部代理、代理源各一张卡)。读失败时保留上一次的。 */
const proxyList = ref<ExternalProxy[]>([])
const sourceList = ref<ProxySource[]>([])

/** 资产卡片聚合接口:计费估值、地区标签、TCP 探测汇总、成本汇总。 */
const dashCards = ref<DashboardCards | null>(null)
const cardsError = ref(false)
const sourceExpiryError = ref(false)
const proxyExpiryError = ref(false)

const daily = ref<DailyPoint[]>([])
const dailyError = ref(false)
const range = ref(30)

const loading = ref(true)
const loadedAt = ref<string | null>(null)

async function loadSummary() {
  summaryError.value = false
  try {
    summary.value = await api.dashboardSummary()
  } catch {
    summaryError.value = true
  }
}

async function loadAlerts() {
  alertsError.value = false
  try {
    alerts.value = (await api.dashboardAlerts()).items
  } catch {
    // 取不到预警时不能当成「没有预警」显示一句「一切正常」—— 那是在替一个
    // 没读到的列表打包票。清单上明说读取失败;上一次读到的预警留着。
    alertsError.value = true
  }
}

async function loadNodes() {
  nodesError.value = false
  columnErrors.value = []
  try {
    nodes.value = (await api.nodes()).items
  } catch {
    nodesError.value = true
    return
  }
  // 周期流量、资源采样、巡检、同步状态都是列级信息:读不到只让那一列显示「—」,
  // 不能把整张节点健康表判成失败。
  const miss = (label: string) => {
    if (!columnErrors.value.includes(label)) columnErrors.value = [...columnErrors.value, label]
  }
  await Promise.all([
    api
      .nodesCycleTraffic()
      .then((r) => (cycles.value = Object.fromEntries(r.items.map((c) => [c.node_id, c]))))
      .catch(() => {
        cycles.value = {}
        miss('本周期流量')
      }),
    api
      .nodeMetricsLatest()
      .then((r) => (metrics.value = Object.fromEntries(r.items.map((m) => [m.node_id, m]))))
      .catch(() => {
        metrics.value = {}
        miss('资源采样')
      }),
    api
      .nodeHealth()
      .then((r) => (health.value = Object.fromEntries(r.items.map((h) => [h.node_id, h]))))
      .catch(() => {
        health.value = {}
        miss('服务巡检')
      }),
    api
      .trafficStatus()
      .then((r) => (syncErrors.value = Object.fromEntries(r.failing_nodes.map((f) => [f.node_id, f.error]))))
      .catch(() => {
        // 原来静默清空:同步失败的徽标从巡检那一列消失,看起来就像同步正常。
        syncErrors.value = {}
        miss('流量同步状态')
      }),
  ])
}

async function loadVendorExpiry() {
  // 两类各自成败:一类读失败不让另一类也跟着消失,读失败的那一类保留上一次的条目,
  // 并记进 failedSources —— 原来是静默不列,清单因此少了那一类还说「没有需要处理的事」。
  sourceExpiryError.value = false
  proxyExpiryError.value = false
  const [p, s] = await Promise.allSettled([api.externalProxies({ includeExcluded: false }), api.proxySources()])
  const keep = (prefix: string) => vendorExpiry.value.filter((x) => x.key.startsWith(prefix))
  const out: typeof vendorExpiry.value = []
  if (s.status === 'fulfilled') {
    sourceList.value = s.value.items
    for (const src of s.value.items) {
      if (src.expiry) out.push({ key: `src-${src.id}`, name: src.name, cat: '代理源', to: '/external-proxies', v: src.expiry })
    }
  } else {
    sourceExpiryError.value = true
    out.push(...keep('src-'))
  }
  if (p.status === 'fulfilled') {
    proxyList.value = p.value.items
    // 跟随来源的条目不单独列:来源那一条已经代表了它们(与提醒的去重规则一致)。
    for (const x of p.value.items) {
      if (x.expiry && x.expiry.has_profile && !x.expiry.inherited) {
        out.push({ key: `ext-${x.id}`, name: x.display_name, cat: '外部代理', to: '/external-proxies', v: x.expiry })
      }
    }
  } else {
    proxyExpiryError.value = true
    out.push(...keep('ext-'))
  }
  vendorExpiry.value = out
}

async function loadDeploys() {
  deployError.value = false
  try {
    deploys.value = (await api.deployments(100)).items
  } catch {
    deployError.value = true
  }
}

async function loadDaily() {
  dailyError.value = false
  try {
    daily.value = (await api.siteDailyTraffic(range.value)).daily
  } catch {
    daily.value = []
    dailyError.value = true
  }
}

async function loadCards() {
  cardsError.value = false
  try {
    dashCards.value = await api.dashboardCards()
  } catch {
    // 保留上一次读到的;卡片上估值、标签、探测几块据此显示「不可用」。
    cardsError.value = true
  }
}

async function load() {
  loading.value = true
  await Promise.all([
    loadSummary(),
    loadAlerts(),
    loadNodes(),
    loadDeploys(),
    loadDaily(),
    loadVendorExpiry(),
    loadCards(),
  ])
  loading.value = false
  loadedAt.value = new Date().toISOString()
}

// 切换时间范围只重取曲线,不刷整页 —— 指标卡与节点表跟这个范围无关。
watch(range, loadDaily)
onMounted(load)

// ---------- 部分失败 ----------

interface FailedSource {
  label: string
  /** 这一项是不是「需要处理」清单的来源。是的话,清单与结论都可能不完整。 */
  attention: boolean
  retry: () => Promise<void>
}

const failedSources = computed<FailedSource[]>(() => {
  const out: FailedSource[] = []
  if (alertsError.value) out.push({ label: '预警', attention: true, retry: loadAlerts })
  if (nodesError.value) out.push({ label: '节点列表(含节点商家到期)', attention: true, retry: loadNodes })
  if (deployError.value) out.push({ label: '部署记录', attention: true, retry: loadDeploys })
  if (sourceExpiryError.value) out.push({ label: '代理源到期', attention: true, retry: loadVendorExpiry })
  if (proxyExpiryError.value) out.push({ label: '外部代理到期', attention: true, retry: loadVendorExpiry })
  // 探测告警进「需要处理」,所以它读失败时清单同样可能不完整。
  if (cardsError.value) out.push({ label: '网络探测与套餐估值', attention: true, retry: loadCards })
  if (summaryError.value) out.push({ label: '概览指标', attention: false, retry: loadSummary })
  if (dailyError.value) out.push({ label: '流量趋势', attention: false, retry: loadDaily })
  for (const c of columnErrors.value) out.push({ label: c, attention: false, retry: loadNodes })
  return out
})
const attnFailed = computed(() => failedSources.value.filter((f) => f.attention))
const failedLabels = (list: FailedSource[]) => list.map((f) => f.label).join('、')

const retrying = ref(false)
/** 只重试读失败的那几项;同一个加载函数只跑一次(列级失败都归 loadNodes)。 */
async function retryFailed() {
  retrying.value = true
  try {
    await Promise.all([...new Set(failedSources.value.map((f) => f.retry))].map((fn) => fn()))
  } finally {
    retrying.value = false
  }
}

// ---------- 指标 ----------

const metricState = computed(() =>
  summary.value ? 'ready' : summaryError.value ? 'error' : loading.value ? 'loading' : 'empty',
)

const nodeMetricState = computed(() =>
  nodes.value.length ? 'ready' : nodesError.value ? 'error' : loading.value ? 'loading' : 'empty',
)

const offlineCount = computed(() => nodes.value.filter((n) => n.status === 'OFFLINE').length)
const subOffCount = computed(() => nodes.value.filter((n) => !n.subscription_enabled).length)

/** 近 7 天部署。分母也取这个窗口,否则「2 / 共 100 次」里的 100 是几个月的量。 */
const recentDeploys = computed(() => {
  const since = Date.now() - 7 * 86400000
  return deploys.value.filter((d) => new Date(d.started_at).getTime() >= since)
})
const failedDeploys = computed(() =>
  recentDeploys.value.filter((d) => d.status === 'FAILED' || d.status === 'ROLLED_BACK'),
)

// ---------- 需要处理 ----------

interface AttentionItem {
  key: string
  level: 'bad' | 'warn'
  icon: LbIconName
  title: string
  cat: string
  text: string
  to: string
  action: string
}

const ALERT_CAT: Record<DashboardAlert['category'], { label: string; icon: LbIconName; action: string }> = {
  user: { label: '用户', icon: 'user', action: '查看用户' },
  node: { label: '节点', icon: 'server', action: '查看节点' },
  cloud_account: { label: '云账号', icon: 'cloud', action: '查看云账号' },
  proxy_source: { label: '代理源', icon: 'external-link', action: '查看外部代理' },
  external_proxy: { label: '外部代理', icon: 'external-link', action: '查看外部代理' },
}

function alertTarget(a: DashboardAlert): string {
  switch (a.category) {
    case 'user':
      return '/users'
    case 'cloud_account':
      return '/settings'
    // 原来这两类落进 default 被当成节点,跳到 /nodes/<源的 id> —— 一台不相干的机器。
    case 'proxy_source':
    case 'external_proxy':
      return '/external-proxies'
    default:
      return a.target_id ? `/nodes/${a.target_id}` : '/nodes'
  }
}

/** 商家到期(V20 的到期档案)。只提醒,面板不代为续费,也不处置。 */
function expiryItem(key: string, name: string, cat: string, to: string, v: ExpiryView): AttentionItem | null {
  if (v.state !== 'SOON' && v.state !== 'OVERDUE') return null
  const meta = expiryStatusMeta(v)
  return {
    key,
    level: v.state === 'OVERDUE' ? 'bad' : 'warn',
    icon: 'calendar',
    title: name,
    cat: `${cat} · 商家到期`,
    text: v.auto_renew ? `${meta.text}。已登记商家自动续费,请确认余额与扣费结果` : `${meta.text},记得在商家处续费后登记新日期`,
    to,
    action: '去续费',
  }
}

const attention = computed<AttentionItem[]>(() => {
  const out: AttentionItem[] = []
  alerts.value.forEach((a, i) => {
    const c = ALERT_CAT[a.category] ?? ALERT_CAT.node
    out.push({
      key: `a${i}`,
      level: a.level === 'error' ? 'bad' : 'warn',
      icon: c.icon,
      title: a.target,
      cat: c.label,
      text: a.message,
      to: alertTarget(a),
      action: c.action,
    })
  })
  for (const n of nodes.value) {
    if (!n.expiry) continue
    const it = expiryItem(`n${n.id}`, n.display_name, '节点', `/nodes/${n.id}`, n.expiry)
    if (it) out.push(it)
  }
  for (const e of vendorExpiry.value) {
    const it = expiryItem(e.key, e.name, e.cat, e.to, e.v)
    if (it) out.push(it)
  }
  // 服务巡检与流量同步(UI 2.0.1)。原来它们不进清单:一台巡检「连不上」、同步一直失败的
  // 机器,结论照样写着「一切正常」。级别按 V10 的口径 —— 服务确实没在跑、自动恢复失败是
  // 严重;SSH 连不上只是「不知道」(可能在重启),同步失败不影响用户连接,两者是预警。
  for (const r of nodeRows.value) {
    const id = r.n.id
    const h = health.value[id]
    if (h && !r.health.preChange) {
      const stopped = r.health.items.filter((i) => i.state === 'STOPPED')
      if (h.recover_error) {
        out.push({
          key: `h${id}`,
          level: 'bad',
          icon: 'alert-triangle',
          title: r.n.display_name,
          cat: '节点 · 服务巡检',
          text: `自动恢复失败:${h.recover_error}`,
          to: `/nodes/${id}`,
          action: '查看节点',
        })
      } else if (stopped.length) {
        out.push({
          key: `h${id}`,
          level: 'bad',
          icon: 'alert-triangle',
          title: r.n.display_name,
          cat: '节点 · 服务巡检',
          text: `${stopped.map((i) => i.name).join('、')} 没在运行`,
          to: `/nodes/${id}`,
          action: '查看节点',
        })
      } else if (r.health.items.some((i) => i.state === 'UNREACHABLE')) {
        out.push({
          key: `h${id}`,
          level: 'warn',
          icon: 'server',
          title: r.n.display_name,
          cat: '节点 · 服务巡检',
          text: '巡检连不上这台机器:SSH 不通,服务是死是活还不知道 —— 可能只是在重启',
          to: `/nodes/${id}`,
          action: '查看节点',
        })
      }
    }
    const syncError = syncErrors.value[id]
    if (syncError) {
      out.push({
        key: `s${id}`,
        level: 'warn',
        icon: 'activity',
        title: r.n.display_name,
        cat: '节点 · 流量同步',
        // 后端的原文已经写了「未修改数据库」,这里不再重复,只取第一行。
        text: `流量同步失败:${syncError.split('\n')[0]}`,
        to: `/nodes/${id}`,
        action: '查看节点',
      })
    }
  }
  // 出站探测连续失败(达到告警阈值)。预警级:这台机器在本机连不上某个公共目标 ——
  // 可能是机器出口、线路或目标本身的问题,不代表用户连不上这台机器。
  // 采集失败(面板连不上机器)不进这里:那不是网络结果,目标那头怎样并不知道。
  for (const t of dashCards.value?.outbound ?? []) {
    if (t.state !== 'DOWN') continue
    out.push({
      key: `o${t.id}`,
      level: 'warn',
      icon: 'activity',
      title: nodeName(t.node_id),
      cat: '节点 · 出站探测',
      text: `到「${t.label}」(${metricLabel(t.metric)},${familyText(t.family)})连续 ${t.consecutive_failures} 次${
        t.metric === 'LOSS' ? '全部丢包' : t.metric === 'SYN' ? '握手全部无应答或被拒' : 'TCP 连接失败'
      } —— 在这台机器本机测的,不代表用户连不上它`,
      to: `/nodes/${t.node_id}/network`,
      action: '查看探测',
    })
  }
  // 入口连通性连续失败(达到告警阈值)。预警级:它只说明从主控连不上那个端口 ——
  // 不等于用户全部断线,也可能只是主控这一侧的网络问题。
  for (const t of dashCards.value?.targets ?? []) {
    if (t.state !== 'DOWN') continue
    out.push({
      key: `p${t.id}`,
      level: 'warn',
      icon: 'activity',
      title: nodeName(t.node_id),
      cat: '节点 · 入口连通性',
      text: `${t.label}(${familyText(t.family)})连续 ${t.consecutive_failures} 次 TCP 连接失败 —— 从主控连不上这个端口,不等于代理不可用`,
      to: `/nodes/${t.node_id}/network`,
      action: '查看探测',
    })
  }
  if (failedDeploys.value.length) {
    const names = [...new Set(failedDeploys.value.map((d) => nodeName(d.node_id)))]
    out.push({
      key: 'deploys',
      level: 'bad',
      icon: 'alert-triangle',
      title: `近 7 天 ${failedDeploys.value.length} 次部署失败`,
      cat: '部署',
      text: names.slice(0, 3).join('、') + (names.length > 3 ? ` 等 ${names.length} 台` : ''),
      to: '/deployments',
      action: '查看记录',
    })
  }
  // error 在前;同级保持各来源原来的顺序(后端已按类别排过)。
  return out.sort((a, b) => (a.level === b.level ? 0 : a.level === 'bad' ? -1 : 1))
})

const badCount = computed(() => attention.value.filter((a) => a.level === 'bad').length)
const warnCount = computed(() => attention.value.filter((a) => a.level === 'warn').length)

/**
 * 一句话结论。严重项才算「要处理」;只有预警时说清楚没有紧急事项。
 *
 * 已经读到的严重项优先 —— 部署记录读失败不该把「有 2 件事需要处理」降成一句含糊的
 * 「部分读取失败」。没有严重项时,任何一项读失败都不能说「一切正常」:清单的来源读失败
 * 是「不知道有没有」,其余几项读失败是「不知道是不是全都正常」。
 */
const headline = computed(() => {
  if (loading.value && !loadedAt.value) return { tone: 'muted', text: '正在汇总各项数据…' }
  if (badCount.value) return { tone: 'bad', text: `有 ${badCount.value} 件事需要处理` }
  if (attnFailed.value.length) {
    return warnCount.value
      ? { tone: 'warn', text: `${warnCount.value} 条预警;部分数据读取失败,清单可能不完整` }
      : { tone: 'warn', text: '部分数据读取失败,无法确认是否有需要处理的事' }
  }
  if (warnCount.value) return { tone: 'warn', text: `${warnCount.value} 条预警,暂无紧急事项` }
  if (failedSources.value.length) return { tone: 'warn', text: '部分数据读取失败,无法确认一切正常' }
  return { tone: 'ok', text: '一切正常,没有需要处理的事' }
})

const nodeLine = computed(() => {
  if (!summary.value) return ''
  const { node_online: online, node_total: total } = summary.value
  if (total === 0) return '还没有添加任何节点'
  return online === total ? `${total} 台节点全部在线` : `${online} / ${total} 台节点在线`
})

const today = computed(() => {
  const d = new Date()
  const week = ['日', '一', '二', '三', '四', '五', '六'][d.getDay()]
  return `${d.getFullYear()} 年 ${d.getMonth() + 1} 月 ${d.getDate()} 日 · 星期${week}`
})

// ---------- 趋势 ----------

/**
 * 缺的日子传 null,不补 0,也不插值。
 * 补 0 会让当天看起来「没人用」,插值会凭空造出一个从未存在的数字 ——
 * 而 traffic_daily 里没有那一行,本来就同时意味着「没流量」和「同步没跑完」。
 */
const points = computed<LbPoint[]>(() => {
  const byDay = new Map(daily.value.map((d) => [d.day, d.total]))
  const out: LbPoint[] = []
  for (let i = range.value - 1; i >= 0; i--) {
    const key = new Date(Date.now() - i * 86400000).toISOString().slice(0, 10)
    out.push({ at: key, value: byDay.has(key) ? (byDay.get(key) as number) : null })
  }
  return out
})

const axisLabels = computed(() => {
  const p = points.value
  if (p.length < 2) return []
  const idx = [0, Math.floor(p.length / 4), Math.floor(p.length / 2), Math.floor((p.length * 3) / 4), p.length - 1]
  return idx.map((i) => p[i].at.slice(5))
})

const peak = computed(() => {
  let best: DailyPoint | null = null
  for (const d of daily.value) if (!best || d.total > best.total) best = d
  return best
})

const rangeTotal = computed(() => daily.value.reduce((s, d) => s + d.total, 0))

const monthStart = computed(() => {
  const d = new Date()
  return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, '0')}-01`
})

// ---------- 节点健康 ----------

function nodeName(id: number): string {
  return nodes.value.find((n) => n.id === id)?.display_name ?? `节点 ${id}`
}

interface NodeRow {
  n: Node
  m: NodeMetrics | undefined
  /** 采样过期或说的是改地址之前的机器:数字照给,颜色降成中性。 */
  stale: boolean
  cpu: number | null
  mem: number | null
  memDetail: string
  disk: number | null
  diskDetail: string
  health: ReturnType<typeof summarizeHealth>
}

const nodeRows = computed<NodeRow[]>(() =>
  nodes.value.map((n) => {
    const m = metrics.value[n.id]
    const pct = (used: number, total: number) => (total > 0 ? (used / total) * 100 : null)
    return {
      n,
      m,
      stale: !!m && (isMetricsStale(m.collected_at) || isPreChange(n, m.collected_at)),
      cpu: m ? m.cpu_percent : null,
      mem: m ? pct(m.mem_used_kb, m.mem_total_kb) : null,
      memDetail: m ? `${formatBytes(m.mem_used_kb * 1024)} / ${formatBytes(m.mem_total_kb * 1024)}` : '',
      disk: m ? pct(m.disk_used_kb, m.disk_total_kb) : null,
      diskDetail: m ? `${formatBytes(m.disk_used_kb * 1024)} / ${formatBytes(m.disk_total_kb * 1024)}` : '',
      health: summarizeHealth(n, health.value[n.id], { syncError: syncErrors.value[n.id], cloud: n.cloud ?? null }),
    }
  }),
)

// ---------- 资产卡片 ----------

const assetCards = computed<AssetCard[]>(() =>
  buildAssetCards({
    nodes: nodes.value,
    metrics: metrics.value,
    cycles: cycles.value,
    sources: sourceList.value,
    proxies: proxyList.value,
    cards: dashCards.value,
  }),
)

/** 「套餐与续费」弹窗。卡片上的编辑套餐、续费、续费历史都开它。 */
const assetModal = ref<{
  kind: ExpiryKind
  id: number
  name: string
  sourceName?: string
  focus: 'billing' | 'renew' | 'history'
} | null>(null)

function openAsset(c: AssetCard, focus: 'billing' | 'renew' | 'history') {
  assetModal.value = {
    kind: c.kind,
    id: c.id,
    name: c.name,
    sourceName: c.proxy?.source_name || undefined,
    focus,
  }
}

/** 改了套餐 / 续了费 / 改了标签:重读卡片要的那几份。不刷整页。 */
async function onAssetChanged() {
  const kind = assetModal.value?.kind
  await Promise.all([loadCards(), kind === 'NODE' ? loadNodes() : loadVendorExpiry()])
}

/** 运行状态的小圆点颜色。只是扫读用的记号,文字标签仍在旁边。 */
function dotColor(n: Node): string {
  switch (n.status) {
    case 'ONLINE':
      return 'var(--ok)'
    case 'OFFLINE':
    case 'DEPLOY_FAILED':
      return 'var(--bad)'
    default:
      return 'var(--text3)'
  }
}

/** 部署记录的一行结论。失败要分两种说法 —— 见 DeploymentsView 里的说明。 */
function conclusion(d: DeploymentRecord): string {
  if (d.status === 'SUCCESS') return `成功 · ${d.steps?.length ?? 0} 步`
  if (d.status === 'RUNNING') {
    const done = (d.steps ?? []).filter((s) => s.status !== 'SKIPPED').length
    return `部署中 · 步骤 ${done}/${d.steps?.length ?? '?'}`
  }
  return d.error_message || '失败'
}
</script>

<template>
  <div class="lb-page dv">
    <div class="lb-page__head">
      <div class="lb-page__title-wrap">
        <div class="lb-page__eyebrow">{{ today }}</div>
        <h1 class="lb-page__title">
          <span>仪表盘</span>
          <LbInfoTip
            :text="`结论取自预警、商家到期、近 7 天失败部署、服务巡检与流量同步:有严重项时写「有 N 件事需要处理」。周期边界均为 UTC 00:00,本月自 ${monthStart} 起;节点额度只预警,不会停服。`"
            :width="320"
          />
        </h1>
        <div class="lb-page__summary dv__headline" data-testid="dashboard-headline">
          <span class="lb-dot" :class="`dv__dot--${headline.tone}`" />
          <span class="dv__headline-text">{{ headline.text }}</span>
          <span v-if="nodeLine" class="dv__headline-sub">· {{ nodeLine }}</span>
        </div>
      </div>
      <div class="lb-page__actions">
        <div class="lb-seg lb-seg--sm" role="tablist" aria-label="仪表盘视图" data-testid="dashboard-view-toggle">
          <button
            type="button"
            role="tab"
            class="lb-seg__item"
            :class="{ 'lb-seg__item--on': view === 'cards' }"
            :aria-selected="view === 'cards'"
            @click="view = 'cards'"
          >
            卡片视图
          </button>
          <button
            type="button"
            role="tab"
            class="lb-seg__item"
            :class="{ 'lb-seg__item--on': view === 'classic' }"
            :aria-selected="view === 'classic'"
            @click="view = 'classic'"
          >
            原有视图
          </button>
        </div>
        <span v-if="loadedAt && !narrow" class="dv__updated">更新于 <LbTimeText :value="loadedAt" /></span>
        <a-button :loading="loading" @click="load">
          <template #icon><LbIcon name="refresh" :size="15" /></template>
          刷新
        </a-button>
      </div>
    </div>

    <!-- 部分失败:说清楚是哪几项没读到,只重试它们。已经读到的照常显示。 -->
    <div
      v-if="failedSources.length && !(loading && !loadedAt)"
      class="lb-notice dv__partial"
      role="alert"
      data-testid="dashboard-partial-failure"
    >
      <span class="lb-notice__icon lb-notice__icon--warn"><LbIcon name="circle-alert" :size="18" /></span>
      <div class="lb-notice__body">
        <div class="lb-notice__title">部分数据读取失败</div>
        <div class="lb-notice__text">
          {{ failedLabels(failedSources) }}没有读到。{{
            attnFailed.length ? '「需要处理」清单与上方结论可能不完整;' : ''
          }}已经读到的数据照常显示,读失败的部分保留上一次的结果或显示为「—」,不是零。
        </div>
      </div>
      <div class="lb-notice__actions">
        <a-button size="small" :loading="retrying" @click="retryFailed">
          <template #icon><LbIcon name="refresh" :size="14" /></template>
          重试失败项
        </a-button>
      </div>
    </div>

    <!-- 卡片视图:筛选 → 概览 → 卡片网格。数据与原有视图是同一批,另加一次卡片聚合接口。 -->
    <AssetCardsView
      v-if="view === 'cards'"
      :cards="assetCards"
      :dash="dashCards"
      :loading="loading"
      :dash-error="cardsError && !dashCards"
      @asset="openAsset"
      @retry="loadCards"
    />

    <!-- 指标卡 -->
    <section v-if="view === 'classic'" class="lb-metrics">
      <LbMetricCard
        label="在线节点"
        icon="server"
        :state="nodeMetricState"
        :value="summary?.node_online ?? nodes.filter((n) => n.status === 'ONLINE').length"
        :total="summary?.node_total ?? nodes.length"
        empty-hint="尚未添加节点"
        :tone="offlineCount ? 'danger' : 'default'"
      >
        <template #foot>
          <LbStatusTag v-if="offlineCount" kind="node" status="OFFLINE" :suffix="String(offlineCount)" />
          <LbStatusTag v-if="subOffCount" :meta="subscriptionOffMeta" :suffix="String(subOffCount)" />
          <span v-if="!offlineCount && !subOffCount && summary && summary.node_online < summary.node_total">
            {{ summary.node_total - summary.node_online }} 台待初始化或已禁用
          </span>
          <span v-else-if="!offlineCount && !subOffCount">全部正常运行</span>
        </template>
      </LbMetricCard>
      <LbMetricCard
        label="有效用户"
        icon="users"
        :state="metricState"
        :value="summary?.user_active"
        :total="summary?.user_total"
        :tone="summary && summary.quota_exceeded ? 'warning' : 'default'"
        :hint="
          summary
            ? summary.quota_exceeded + summary.expiring_soon
              ? `${summary.quota_exceeded} 人超额 · ${summary.expiring_soon} 人即将到期`
              : '没有人过期或超额'
            : undefined
        "
      />
      <LbMetricCard
        label="本月流量"
        icon="activity"
        tip="全站用户流量,按 UTC 自然月;不含中转链路的重复计数与不计流量入口。"
        :state="metricState"
        :value="summary ? formatBytes(summary.traffic_month).split(' ')[0] : undefined"
        :unit="summary ? formatBytes(summary.traffic_month).split(' ')[1] : undefined"
        :hint="summary ? `今日 ${formatBytes(summary.traffic_today)}` : undefined"
      />
      <LbMetricCard
        label="需要处理"
        icon="bell"
        tip="预警(用户到期 / 超额、节点额度、监控过期、云账号、外部代理)+ 商家到期 + 近 7 天失败部署 + 服务巡检(服务没在跑、连不上)+ 流量同步失败。"
        :state="attnFailed.length && !attention.length ? 'error' : loading && !loadedAt ? 'loading' : 'ready'"
        :value="attention.length"
        unit="项"
        :tone="badCount ? 'danger' : attnFailed.length ? 'default' : warnCount ? 'warning' : 'success'"
      >
        <template #foot>
          <template v-if="attention.length">
            <span v-if="badCount" class="dv__count dv__count--bad">{{ badCount }} 项严重</span>
            <span v-if="warnCount" class="dv__count dv__count--warn">{{ warnCount }} 项预警</span>
          </template>
          <span v-if="attnFailed.length" class="dv__count dv__count--warn">清单不完整</span>
          <span v-else-if="!attention.length">没有待处理事项</span>
        </template>
      </LbMetricCard>
    </section>

    <!-- 趋势 + 需要处理(卡片视图里只留「需要处理」,放在卡片下面) -->
    <div class="dv__main" :class="{ 'dv__main--solo': view === 'cards' }">
      <section v-if="view === 'classic'" class="lb-card dv__trend">
        <div class="lb-card__head">
          <div>
            <h2 class="lb-card__title">
              全站流量
              <LbInfoTip text="按 UTC 日聚合,全站上下行合计。虚线跨过的日子没有记录 —— 不补 0 也不插值。悬停查看当日流量。" :width="300" />
            </h2>
            <div class="lb-card__desc">
              近 {{ range }} 天合计 <b class="lb-tabular dv__strong">{{ formatBytes(rangeTotal) }}</b>
              <template v-if="peak"> · 峰值 {{ formatBytes(peak.total) }}({{ formatUTCDay(peak.day) }})</template>
            </div>
          </div>
          <!-- 范围切换用分段控件,不用下拉:三个选项摊开比藏起来快。 -->
          <div class="lb-seg lb-seg--sm" role="tablist" aria-label="时间范围">
            <button
              v-for="r in [7, 30, 90]"
              :key="r"
              type="button"
              role="tab"
              class="lb-seg__item"
              :class="{ 'lb-seg__item--on': range === r }"
              :aria-selected="range === r"
              @click="range = r"
            >
              {{ r }} 天
            </button>
          </div>
        </div>

        <LbEmptyState
          v-if="dailyError"
          variant="error"
          title="无法读取流量汇总"
          description="图表保持空白而不是画一条零线 —— 零线会被误读成「今天没人用」。"
          @retry="loadDaily"
        />
        <LbEmptyState
          v-else-if="!loading && daily.length === 0"
          variant="empty"
          title="这段时间没有流量记录"
          description="订阅还没有被任何客户端拉取过,或者流量同步尚未跑过一轮。"
        />
        <template v-else>
          <LbSparkline :points="points" type="line" :height="200" />
          <div class="dv__axis lb-tabular">
            <span v-for="(l, i) in axisLabels" :key="i">{{ l }}</span>
          </div>
        </template>
      </section>

      <section class="lb-card lb-card--flush dv__attn">
        <div class="lb-card__head">
          <h2 class="lb-card__title">
            需要处理
            <span v-if="attention.length" class="dv__attn-count lb-tabular">{{ attention.length }}</span>
          </h2>
        </div>
        <div v-if="loading && !loadedAt" class="dv__attn-skel">
          <a-skeleton active :title="false" :paragraph="{ rows: 4 }" />
        </div>
        <template v-else>
          <div v-if="attnFailed.length" class="dv__attn-error" role="alert">
            <LbIcon name="circle-alert" :size="15" />
            <span>{{ failedLabels(attnFailed) }}读取失败,清单可能不完整</span>
            <a-button type="link" size="small" class="dv__attn-retry" :loading="retrying" @click="retryFailed">
              重试
            </a-button>
          </div>
          <ul v-if="attention.length" class="dv__attn-list">
            <!-- 整条是一个真链接:Tab 能到、回车能点、能在新标签页打开。 -->
            <li v-for="a in attention" :key="a.key">
              <RouterLink class="dv__attn-item" :to="a.to">
              <span class="dv__attn-icon" :class="`dv__attn-icon--${a.level}`">
                <LbIcon :name="a.icon" :size="15" />
              </span>
              <div class="dv__attn-body">
                <div class="dv__attn-title">
                  <span class="lb-ellipsis">{{ a.title }}</span>
                  <span class="dv__attn-cat">{{ a.cat }}</span>
                </div>
                <div class="dv__attn-text lb-clamp-2">{{ a.text }}</div>
              </div>
              <span class="dv__attn-go">
                {{ a.action }}
                <LbIcon name="chevron-right" :size="14" />
              </span>
              </RouterLink>
            </li>
          </ul>
          <div v-else-if="!attnFailed.length" class="dv__attn-ok">
            <span class="dv__attn-ok-icon"><LbIcon name="circle-check" :size="20" /></span>
            <div class="dv__attn-ok-title">没有需要处理的事</div>
            <div class="dv__attn-ok-text">预警、商家到期、近 7 天的部署、服务巡检与流量同步都正常。</div>
          </div>
          <div v-else class="dv__attn-partial">
            已读到的数据里没有需要处理的事,但上面几项没有读到,不能据此认为一切正常。
          </div>
        </template>
      </section>
    </div>

    <!-- 节点健康 -->
    <section v-if="view === 'classic'" class="lb-card lb-card--flush">
      <div class="lb-card__head">
        <div>
          <h2 class="lb-card__title">
            节点健康
            <LbInfoTip
              text="资源取自最近一次采样(每 5 分钟),巡检取自最近一轮服务巡检 —— 这一页只读库里已有的结果,不会去连任何一台机器。采样过期的数字仍会显示,但颜色降成灰色。"
              :width="320"
            />
          </h2>
          <div class="lb-card__desc">CPU / 内存 / 磁盘取最近一次采样;过期或改过管理地址之前的数据显示为灰色</div>
        </div>
        <RouterLink class="dv__link" to="/nodes">全部节点 <LbIcon name="chevron-right" :size="14" /></RouterLink>
      </div>

      <div v-if="nodesError && nodes.length" class="dv__colerr">
        <LbIcon name="info" :size="14" />
        节点列表本次刷新读取失败,下面是上一次读到的结果。
      </div>
      <div v-else-if="columnErrors.length && !nodesError" class="dv__colerr">
        <LbIcon name="info" :size="14" />
        {{ columnErrors.join('、') }}读取失败,对应的列显示为「—」,不是零。
      </div>

      <LbEmptyState
        v-if="nodesError && !nodes.length"
        variant="error"
        title="无法加载节点列表"
        description="不显示「暂无数据」—— 那会被读成一台机器都没有。"
        @retry="loadNodes"
      />
      <div v-else-if="loading && !nodes.length" class="dv__attn-skel">
        <a-skeleton active :title="false" :paragraph="{ rows: 3 }" />
      </div>
      <LbEmptyState
        v-else-if="!nodes.length"
        variant="empty"
        title="还没有任何节点"
        description="添加第一台 VPS 后,这里会显示节点健康与本周期流量。"
      >
        <template #action>
          <a-button type="primary" size="small" @click="router.push('/nodes')">添加节点</a-button>
        </template>
      </LbEmptyState>

      <!-- 桌面:表格式栅格 -->
      <div v-else-if="!compactNodes" class="dv__nodes" role="table" aria-label="节点健康">
        <div class="dv__nrow dv__nrow--head" role="row">
          <span role="columnheader">节点</span>
          <span role="columnheader">CPU</span>
          <span role="columnheader">内存</span>
          <span role="columnheader">磁盘</span>
          <span role="columnheader">网络 ↓ / ↑</span>
          <span role="columnheader">本周期流量</span>
          <span role="columnheader">服务巡检</span>
        </div>
        <div v-for="r in nodeRows" :key="r.n.id" class="dv__nrow" role="row" @click="router.push(`/nodes/${r.n.id}`)">
          <div class="dv__ncell-name" role="cell">
            <!-- 整行点击只照顾鼠标;键盘与读屏走名字上这个真链接。stop 免得行上再跳一次。 -->
            <RouterLink class="dv__nname" :to="`/nodes/${r.n.id}`" @click.stop>
              <span class="lb-dot" :style="{ '--dot': dotColor(r.n) }" />
              <span class="lb-ellipsis">{{ r.n.display_name }}</span>
            </RouterLink>
            <!-- 运行与配置分两个标签:一台在跑旧配置、部署失败的机器,
                 挤在一格里只显示「部署失败」,看不出它其实还在服务用户。 -->
            <div class="dv__ntags">
              <LbStatusTag kind="node" :status="r.n.status" />
              <LbStatusTag :meta="configStatusMeta[configState(r.n)]" />
              <LbStatusTag v-for="(b, i) in nodeBadges(r.n, r.m?.collected_at)" :key="i" :meta="b" />
            </div>
          </div>
          <div role="cell"><LbUsageBar :percent="r.cpu" :stale="r.stale" /></div>
          <div role="cell"><LbUsageBar :percent="r.mem" :detail="r.memDetail" :stale="r.stale" /></div>
          <div role="cell"><LbUsageBar :percent="r.disk" :detail="r.diskDetail" :stale="r.stale" /></div>
          <div role="cell" class="dv__net lb-tabular" :class="{ 'dv__net--stale': r.stale }">
            <template v-if="r.m">
              <span><LbIcon name="arrow-down" :size="12" />{{ formatRate(r.m.net_rx_bps) }}</span>
              <span><LbIcon name="arrow-up" :size="12" />{{ formatRate(r.m.net_tx_bps) }}</span>
            </template>
            <span v-else class="lb-muted">—</span>
          </div>
          <div role="cell">
            <span v-if="r.n.role === 'RELAY'" class="lb-muted dv__small">中转主机,不计流量</span>
            <LbQuotaBar
              v-else
              :used-bytes="cycles[r.n.id]?.used_bytes ?? null"
              :quota-bytes="cycles[r.n.id]?.quota_bytes ?? r.n.traffic_quota_bytes"
              :warning-level="cycles[r.n.id]?.warning_level"
            />
          </div>
          <div role="cell" class="dv__nhealth">
            <LbStatusTag :meta="r.health.meta" />
            <LbStatusTag v-if="r.health.badges[0]" :meta="r.health.badges[0]" />
          </div>
        </div>
      </div>

      <!-- 手机与平板:每台一张紧凑卡 -->
      <div v-else class="dv__ncards">
        <RouterLink v-for="r in nodeRows" :key="r.n.id" class="dv__ncard" :to="`/nodes/${r.n.id}`">
          <div class="dv__nname">
            <span class="lb-dot" :style="{ '--dot': dotColor(r.n) }" />
            <span class="lb-ellipsis">{{ r.n.display_name }}</span>
            <LbIcon name="chevron-right" :size="14" class="dv__ncard-go" />
          </div>
          <div class="dv__ntags">
            <LbStatusTag kind="node" :status="r.n.status" />
            <LbStatusTag :meta="configStatusMeta[configState(r.n)]" />
            <LbStatusTag :meta="r.health.meta" />
          </div>
          <div class="dv__ncard-usage">
            <div><span class="dv__ncard-k">CPU</span><LbUsageBar :percent="r.cpu" :stale="r.stale" :min-width="40" /></div>
            <div><span class="dv__ncard-k">内存</span><LbUsageBar :percent="r.mem" :stale="r.stale" :min-width="40" /></div>
            <div><span class="dv__ncard-k">磁盘</span><LbUsageBar :percent="r.disk" :stale="r.stale" :min-width="40" /></div>
          </div>
          <LbQuotaBar
            v-if="r.n.role !== 'RELAY'"
            :used-bytes="cycles[r.n.id]?.used_bytes ?? null"
            :quota-bytes="cycles[r.n.id]?.quota_bytes ?? r.n.traffic_quota_bytes"
            :warning-level="cycles[r.n.id]?.warning_level"
          />
        </RouterLink>
      </div>
    </section>

    <!-- 最近部署 -->
    <section v-if="view === 'classic'" class="lb-card lb-card--flush">
      <div class="lb-card__head">
        <h2 class="lb-card__title">最近部署</h2>
        <RouterLink class="dv__link" to="/deployments">全部记录 <LbIcon name="chevron-right" :size="14" /></RouterLink>
      </div>
      <div v-if="deployError && deploys.length" class="dv__colerr">
        <LbIcon name="info" :size="14" />
        部署记录本次刷新读取失败,下面是上一次读到的结果。
      </div>
      <LbEmptyState v-if="deployError && !deploys.length" variant="error" title="无法加载部署记录" @retry="loadDeploys" />
      <LbEmptyState
        v-else-if="!loading && deploys.length === 0"
        variant="empty"
        title="还没有部署记录"
        description="添加节点并执行第一次部署后,这里会记录每一步的结果。"
      />
      <div v-else class="dv__deploys">
        <div v-for="d in deploys.slice(0, 6)" :key="d.id" class="dv__deploy">
          <LbStatusTag kind="deploy" :status="d.status" />
          <div class="dv__deploy-body">
            <div class="dv__deploy-title">
              <RouterLink :to="`/nodes/${d.node_id}/deploys`">{{ nodeName(d.node_id) }}</RouterLink>
              <span class="lb-chip">rev {{ d.revision }}</span>
            </div>
            <div class="dv__deploy-msg lb-clamp-2">{{ conclusion(d) }}</div>
            <div v-if="d.rollback_result" class="dv__deploy-rb">{{ d.rollback_result }}</div>
          </div>
          <LbTimeText :value="d.started_at" class="dv__deploy-time" />
        </div>
      </div>
    </section>

    <ExpiryModal
      v-if="assetModal"
      :open="true"
      :kind="assetModal.kind"
      :object-id="assetModal.id"
      :name="assetModal.name"
      :source-name="assetModal.sourceName"
      :focus="assetModal.focus"
      @update:open="(v: boolean) => !v && (assetModal = null)"
      @changed="onAssetChanged"
    />
  </div>
</template>

<style scoped>
.dv__headline {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  flex-wrap: wrap;
}
.dv__headline-text {
  font-weight: 500;
  color: var(--text);
}
.dv__headline-sub {
  color: var(--text3);
}
.dv__dot--ok {
  --dot: var(--ok);
}
.dv__dot--warn {
  --dot: var(--warn);
}
.dv__dot--bad {
  --dot: var(--bad);
}
.dv__dot--muted {
  --dot: var(--text3);
}

@media (max-width: 767px) {
  .dv__partial {
    flex-wrap: wrap;
  }
  .dv__partial .lb-notice__actions {
    margin-left: 44px;
  }
}

.dv__updated {
  font-size: var(--fs-xs);
  color: var(--text3);
  white-space: nowrap;
}

.dv__count {
  font-weight: 500;
}
.dv__count--bad {
  color: var(--bad);
}
.dv__count--warn {
  color: var(--warn);
}

.dv__link {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  font-size: var(--fs-sm);
  font-weight: 500;
  cursor: pointer;
}

.dv__strong {
  color: var(--text);
  font-weight: 600;
}

/* ---------- 趋势 + 需要处理 ---------- */

.dv__main {
  display: grid;
  grid-template-columns: minmax(0, 1.75fr) minmax(320px, 1fr);
  gap: var(--sp-4);
  align-items: stretch;
}
/* 卡片视图里没有趋势图,「需要处理」独占一行。 */
.dv__main--solo {
  grid-template-columns: minmax(0, 1fr);
}

.dv__axis {
  display: flex;
  justify-content: space-between;
  margin-top: var(--sp-2);
  font-size: var(--fs-2xs);
  color: var(--text3);
}

.dv__attn {
  display: flex;
  flex-direction: column;
  min-height: 0;
}
.dv__attn-count {
  min-width: 20px;
  padding: 0 6px;
  border-radius: var(--r-pill);
  background: var(--fill);
  color: var(--text2);
  font-size: var(--fs-2xs);
  font-weight: 600;
  line-height: 18px;
  text-align: center;
}
.dv__attn-skel {
  padding: var(--sp-4) var(--sp-5);
}
.dv__attn-error {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  margin: var(--sp-3) var(--sp-4) 0;
  padding: 8px 10px;
  border-radius: var(--r-btn);
  background: var(--warn-bg);
  color: var(--warn);
  font-size: var(--fs-xs);
}
.dv__attn-retry {
  height: auto;
  padding: 0 2px;
  font-size: var(--fs-xs);
  font-weight: 500;
}
.dv__attn-partial {
  padding: var(--sp-4) var(--sp-5) var(--sp-5);
  font-size: var(--fs-sm);
  color: var(--text3);
}

.dv__attn-list {
  list-style: none;
  margin: 0;
  padding: var(--sp-2);
  overflow-y: auto;
  max-height: 340px;
}
.dv__attn-item {
  display: flex;
  align-items: flex-start;
  gap: var(--sp-3);
  padding: 10px var(--sp-3);
  border-radius: var(--r-btn);
  color: var(--text);
  cursor: pointer;
  transition: background var(--dur-fast);
}
.dv__attn-item:hover {
  background: var(--surface2);
}
.dv__attn-icon {
  flex: none;
  width: 28px;
  height: 28px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: var(--r-tile);
}
.dv__attn-icon--bad {
  background: var(--bad-bg);
  color: var(--bad);
}
.dv__attn-icon--warn {
  background: var(--warn-bg);
  color: var(--warn);
}
.dv__attn-body {
  flex: 1;
  min-width: 0;
}
.dv__attn-title {
  display: flex;
  align-items: baseline;
  gap: var(--sp-2);
  min-width: 0;
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--text);
}
.dv__attn-cat {
  flex: none;
  font-size: var(--fs-2xs);
  font-weight: 400;
  color: var(--text3);
}
.dv__attn-text {
  margin-top: 1px;
  font-size: var(--fs-xs);
  line-height: 1.55;
  color: var(--text2);
}
.dv__attn-go {
  flex: none;
  display: inline-flex;
  align-items: center;
  gap: 2px;
  margin-top: 4px;
  font-size: var(--fs-xs);
  font-weight: 500;
  color: var(--text3);
  white-space: nowrap;
}
.dv__attn-item:hover .dv__attn-go {
  color: var(--brand);
}

.dv__attn-ok {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 6px;
  padding: var(--sp-8) var(--sp-5);
  text-align: center;
}
.dv__attn-ok-icon {
  width: 40px;
  height: 40px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: 50%;
  background: var(--ok-bg);
  color: var(--ok);
  margin-bottom: 4px;
}
.dv__attn-ok-title {
  font-size: var(--fs-base);
  font-weight: 600;
}
.dv__attn-ok-text {
  font-size: var(--fs-sm);
  color: var(--text3);
}

/* ---------- 节点健康 ---------- */

.dv__colerr {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 8px var(--sp-5);
  background: var(--warn-bg);
  color: var(--warn);
  font-size: var(--fs-xs);
  border-bottom: 1px solid var(--sep2);
}

.dv__nodes {
  overflow-x: auto;
}
.dv__nrow {
  display: grid;
  grid-template-columns:
    minmax(220px, 1.6fr) minmax(96px, 0.8fr) minmax(96px, 0.8fr) minmax(96px, 0.8fr)
    minmax(120px, 0.9fr) minmax(150px, 1.1fr) minmax(120px, 0.9fr);
  gap: var(--sp-4);
  align-items: center;
  min-width: 980px;
  padding: 12px var(--sp-5);
  border-bottom: 1px solid var(--sep2);
  cursor: pointer;
  transition: background var(--dur-fast);
}
.dv__nrow:last-child {
  border-bottom: none;
}
.dv__nrow:not(.dv__nrow--head):hover {
  background: var(--surface2);
}
.dv__nrow--head {
  padding-top: 9px;
  padding-bottom: 9px;
  background: var(--surface2);
  border-bottom-color: var(--sep);
  font-size: var(--fs-xs);
  font-weight: 500;
  color: var(--text3);
  cursor: default;
}

.dv__ncell-name {
  min-width: 0;
}
.dv__nname {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  min-width: 0;
  font-size: var(--fs-base);
  font-weight: 600;
  color: var(--text);
}
.dv__ntags {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  margin-top: 6px;
}

.dv__net {
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: var(--fs-xs);
  color: var(--text2);
}
.dv__net span {
  display: inline-flex;
  align-items: center;
  gap: 3px;
}
.dv__net :deep(.lb-icon) {
  color: var(--text3);
}
.dv__net--stale {
  color: var(--text3);
}

.dv__nhealth {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}

.dv__small {
  font-size: var(--fs-xs);
}

.dv__ncards {
  display: flex;
  flex-direction: column;
}
.dv__ncard {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 14px var(--sp-4);
  border-bottom: 1px solid var(--sep2);
  color: var(--text);
}
.dv__ncard:last-child {
  border-bottom: none;
}
.dv__ncard .dv__ntags {
  margin-top: 0;
}
.dv__ncard-go {
  margin-left: auto;
  color: var(--text3);
}
.dv__ncard-usage {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: var(--sp-3);
}
.dv__ncard-k {
  display: block;
  margin-bottom: 2px;
  font-size: var(--fs-2xs);
  color: var(--text3);
}

/* ---------- 最近部署 ---------- */

.dv__deploys {
  display: flex;
  flex-direction: column;
}
.dv__deploy {
  display: grid;
  grid-template-columns: 92px minmax(0, 1fr) auto;
  align-items: start;
  gap: var(--sp-3);
  padding: 12px var(--sp-5);
  border-bottom: 1px solid var(--sep2);
}
.dv__deploy:last-child {
  border-bottom: none;
}
.dv__deploy > :first-child {
  justify-self: start;
}
.dv__deploy-body {
  min-width: 0;
}
.dv__deploy-title {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  font-size: var(--fs-base);
  font-weight: 600;
}
.dv__deploy-title a {
  color: var(--text);
}
.dv__deploy-title a:hover {
  color: var(--brand);
}
.dv__deploy-msg {
  margin-top: 2px;
  font-size: var(--fs-xs);
  color: var(--text3);
}
.dv__deploy-rb {
  margin-top: 2px;
  font-size: var(--fs-xs);
  color: var(--warn);
}
.dv__deploy-time {
  font-size: var(--fs-xs);
  white-space: nowrap;
}

@media (max-width: 1100px) {
  .dv__main {
    grid-template-columns: 1fr;
  }
  .dv__attn-list {
    max-height: none;
  }
}

@media (max-width: 767px) {
  .dv__deploy {
    grid-template-columns: minmax(0, 1fr) auto;
    padding: 12px var(--sp-4);
  }
  .dv__deploy > :first-child {
    grid-column: 1 / -1;
    justify-self: start;
  }
}
</style>
