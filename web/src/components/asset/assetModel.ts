import type {
  BillingCard,
  CardPick,
  CostRow,
  DashboardCards,
  ExpiryKind,
  ExpiryView,
  ExternalCheckStatus,
  ExternalProxy,
  Node,
  NetRate,
  NodeCycleUsage,
  NodeMetrics,
  ProbeSummary,
  ProxySource,
} from '@/api/client'
import { isMetricsStale, isPreChange } from '@/components/lb/derive'
import { formatBytes } from '@/utils/format'

/**
 * 资产卡片的数据模型。**只有这一处**把仪表盘已经读到的几份数据(节点、资源采样、周期流量、
 * 外部代理、代理源)与卡片聚合接口(估值、标签、探测)拼成一张张卡片;筛选、排序、概览也在这里。
 * 全是纯函数,组件只渲染结果。
 *
 * 几条载重的规矩:
 *
 *   - **没有的数据显示「不可用」,不显示 0。** 外部代理没有 CPU / 内存,画一条 0% 的条是在伪造监控;
 *   - **在线 / 离线、资源数据过期、网络检测失败、已关机、监测关闭是五种不同的状态**,各给各的标记,
 *     一张卡上可以同时有几种,但绝不合成一个;
 *   - **出站网络质量与入口连通性分开放**:前者是这台机器在本机测公共目标(节点本机),后者是主控连这台
 *     机器(主控探测)。两组数字回答的是不同的问题,合进一个失败率会让人去修错方向;
 *   - **只显示真实存在的探测来源**:地区与运营商只来自 Boce 拨测点接口实际返回的说法,不凭空造「电信 / 联通 / 移动」三行;
 *   - **多币种不相加**。按价格排序时先按币种分组,再在组内比金额。
 */

export type AssetType = 'LANDING' | 'RELAY' | 'EXTERNAL' | 'SOURCE'

export const assetTypeLabel: Record<AssetType, string> = {
  LANDING: '落地节点',
  RELAY: '中转主机',
  EXTERNAL: '外部代理',
  SOURCE: '代理源',
}

/** 状态筛选的取值。一张卡可以同时属于几种(在线 + 资源数据过期 + 网络检测失败)。 */
export type StatusKey =
  | 'ONLINE'
  | 'OFFLINE'
  | 'STOPPED'
  | 'PROBE_FAIL'
  | 'STALE'
  | 'MONITOR_OFF'
  | 'DISABLED'
  | 'EXPIRING'
  | 'EXTERNAL'

export const statusKeyLabel: Record<StatusKey, string> = {
  ONLINE: '在线',
  OFFLINE: '离线 / 部署失败',
  STOPPED: '已关机',
  PROBE_FAIL: '网络检测失败',
  STALE: '数据过期',
  MONITOR_OFF: '监测关闭 / 未配置',
  DISABLED: '已停用',
  EXPIRING: '即将到期 / 已到期',
  EXTERNAL: '外部线路(无资源数据)',
}

/**
 * 「网络」一格的监测状态。UNKNOWN 是聚合接口没读到 —— 与「没接入」「没配置」是三件事,
 * 读失败说成「未配置」会让人去配一个本来就配好了的东西。
 */
export type MonitorState = 'ON' | 'NONE' | 'PAUSED' | 'GLOBAL_OFF' | 'NA' | 'MANUAL' | 'UNKNOWN'

export interface CardResources {
  cpu: number | null
  mem: number | null
  disk: number | null
  memDetail: string
  diskDetail: string
  rx: number | null
  tx: number | null
  /** 采样过期,或说的是改管理地址之前的机器:数字照给,颜色降灰,不当实时看。 */
  stale: boolean
  collectedAt: string | null
  /**
   * 网速来自卡片的实时读数(仪表盘开着时每 30 秒一次)还是资源采样(5 分钟一次)。
   * 前者新鲜时用前者;两者口径相同(除 lo 以外全部网卡),只是窗口与时间不同。
   */
  rateLive: boolean
  /** 网速那一行的时间;rateStale 为真时降灰并写明过期。 */
  rateAt: string | null
  rateStale: boolean
  /** 实时读数的窗口(秒),悬停说明用。 */
  rateWindow: number | null
}

/** 开机时长的读数:那一刻的秒数 + 读数时间,显示时按「现在」往前推。 */
export interface CardUptime {
  seconds: number
  at: string
}

export interface CardTraffic {
  used: number | null
  quota: number
  warning?: NodeCycleUsage['warning_level']
  source: string
}

export interface AssetCard {
  key: string
  kind: ExpiryKind
  id: number
  name: string
  /** 第二行的小字:内部名称 / 来源 / 协议。 */
  subtitle: string
  type: AssetType
  selfHosted: boolean
  region: string
  tags: string[]
  group: string
  to: string
  node: Node | null
  proxy: ExternalProxy | null
  source: ProxySource | null
  expiry: ExpiryView | null
  /** null = 没有资源数据这回事(外部线路);resources.cpu 为 null = 应该有但还没采到。 */
  resources: CardResources | null
  /** 资源一格不可用时的说明。 */
  resourcesNote: string
  traffic: CardTraffic | null
  trafficNote: string
  /** 出站网络质量(这台机器 → 公共目标),只含此刻生效的序列,按公共目标的排序。 */
  outbound: ProbeSummary[]
  /** 入口连通性(主控 → 这台机器),含暂停的(降灰显示)。 */
  probes: ProbeSummary[]
  /** Boce 外部入站拨测的序列(每个「任务 × 拨测点」一条)。 */
  boce: ProbeSummary[]
  /**
   * 卡片上实际展示的入站 / 出站项目(各最多 3 项,按节点的展示设置或自动推荐)。
   * 这是展示数量,不是检测数量:没展示的序列照样在测,在节点详情里看得到。
   */
  shownIngress: ProbeSummary[]
  shownOutbound: ProbeSummary[]
  /** 手动选了、但那条序列已经删掉的展示项个数(提示去调整,不自动顶上别的)。 */
  shownInvalid: number
  /** 在测但没有展示在卡片上的序列个数。 */
  hiddenCount: number
  monitor: MonitorState
  external: ExternalCheckStatus | null
  billing: BillingCard | null
  /** 聚合接口读到了没有。没读到时套餐那一格写「没有读到」,不写「未设置套餐」。 */
  cardsLoaded: boolean
  statuses: Set<StatusKey>
  cloudStopped: boolean
  /** 排序用。 */
  latency: number | null
  /** 开机时长;只在读数可信(实时读数新鲜,或资源采样没过期)时给,否则 null。 */
  uptime: CardUptime | null
}

export interface AssetInputs {
  nodes: Node[]
  metrics: Record<number, NodeMetrics>
  cycles: Record<number, NodeCycleUsage>
  sources: ProxySource[]
  proxies: ExternalProxy[]
  cards: DashboardCards | null
  /** 卡片实时网速(只在仪表盘开着时有),按节点 ID。没有就退回资源采样里的网速。 */
  netRates?: Record<number, NetRate>
  /** 实时网速的读数间隔(秒);0 表示采样关着。 */
  netRateInterval?: number
  /** 「现在」,判断实时读数新不新鲜用;测试里固定。 */
  now?: number
}

/** 实时读数超过这么久就不算「此刻」:三个间隔,至少 90 秒(轮询偶尔晚一拍不至于来回跳)。 */
export function netRateMaxAgeMs(intervalSec: number | undefined): number {
  return Math.max((intervalSec || 30) * 3, 90) * 1000
}

function pct(used: number, total: number): number | null {
  return total > 0 ? (used / total) * 100 : null
}

function labelsMap(cards: DashboardCards | null) {
  const m = new Map<string, { region: string; tags: string[] }>()
  for (const l of cards?.labels ?? []) m.set(`${l.kind}-${l.object_id}`, l)
  return m
}

function billingMap(cards: DashboardCards | null) {
  const m = new Map<string, BillingCard>()
  for (const b of cards?.billing ?? []) m.set(`${b.kind}-${b.object_id}`, b)
  return m
}

function expiringState(v: ExpiryView | null | undefined): boolean {
  return v?.state === 'SOON' || v?.state === 'OVERDUE'
}

/** 探测行的显示顺序:对外服务在前,管理通道在后;同类按登记顺序。 */
function sortProbes(list: ProbeSummary[]): ProbeSummary[] {
  return [...list].sort((a, b) => (a.kind === b.kind ? a.id - b.id : a.kind === 'SERVICE' ? -1 : 1))
}

function monitorOf(
  outbound: ProbeSummary[],
  probes: ProbeSummary[],
  boce: ProbeSummary[],
  cards: DashboardCards | null,
): MonitorState {
  if (!cards) return 'UNKNOWN'
  if (!cards.probe.available) return 'NA'
  // Boce 有自己的开关:探测的全局开关关着时,Boce 序列照样要能看见。
  const boceOn = boce.some((p) => p.enabled)
  if (!cards.probe.enabled && !boceOn) return 'GLOBAL_OFF'
  if (!outbound.length && !probes.length && !boce.length) return 'NONE'
  if (!outbound.length && probes.every((p) => !p.enabled) && !boceOn) return 'PAUSED'
  return 'ON'
}

/** 按 card_picks 取展示项;老数据(没有 card_picks)时退回升级前的规则:出站前 3 条、入口前 2 个。 */
function pickShown(
  pick: CardPick | undefined,
  outboundAll: ProbeSummary[],
  outboundOn: ProbeSummary[],
  probes: ProbeSummary[],
  boce: ProbeSummary[],
) {
  if (!pick) {
    return { ingress: probes.slice(0, 2), outbound: outboundOn.slice(0, 3), invalid: 0 }
  }
  const byID = new Map<number, ProbeSummary>()
  for (const p of [...probes, ...boce, ...outboundAll]) byID.set(p.id, p)
  const take = (ids: number[]) => ids.map((id) => byID.get(id)).filter((p): p is ProbeSummary => !!p)
  return {
    ingress: take(pick.ingress_ids),
    outbound: take(pick.outbound_ids),
    invalid: pick.invalid_ingress.length + pick.invalid_outbound.length,
  }
}

/** 最近一次可达且没过期的 RTT 里最小的那个;没有就是 null(排序时排最后)。 */
function minLatency(list: ProbeSummary[]): number | null {
  let best: number | null = null
  for (const p of list) {
    const l = p.last
    if (!p.enabled || p.stale || !l || (l.status !== 'OK' && l.status !== 'LOSS') || l.rtt_ms == null) continue
    if (best === null || l.rtt_ms < best) best = l.rtt_ms
  }
  return best
}

/** 排序用的延迟:出站优先(机器本身的网络),没有出站数据时才看入口,再没有才看 Boce。 */
function latencyOf(outbound: ProbeSummary[], probes: ProbeSummary[], boce: ProbeSummary[]): number | null {
  return minLatency(outbound) ?? minLatency(probes) ?? minLatency(boce)
}

const failing = (p: ProbeSummary) => p.state === 'DOWN' || p.state === 'FAILING'


export function buildAssetCards(inp: AssetInputs): AssetCard[] {
  const labels = labelsMap(inp.cards)
  const billing = billingMap(inp.cards)
  const probesBy = new Map<number, ProbeSummary[]>()
  for (const t of inp.cards?.targets ?? []) {
    const list = probesBy.get(t.node_id) ?? []
    list.push(t)
    probesBy.set(t.node_id, list)
  }
  const outboundBy = new Map<number, ProbeSummary[]>()
  const outboundAllBy = new Map<number, ProbeSummary[]>()
  for (const t of inp.cards?.outbound ?? []) {
    const all = outboundAllBy.get(t.node_id) ?? []
    all.push(t)
    outboundAllBy.set(t.node_id, all)
    if (!t.enabled) continue
    const list = outboundBy.get(t.node_id) ?? []
    list.push(t)
    outboundBy.set(t.node_id, list)
  }
  const boceBy = new Map<number, ProbeSummary[]>()
  for (const t of inp.cards?.boce ?? []) {
    const list = boceBy.get(t.node_id) ?? []
    list.push(t)
    boceBy.set(t.node_id, list)
  }
  const pickBy = new Map<number, CardPick>()
  for (const p of inp.cards?.card_picks ?? []) pickBy.set(p.node_id, p)
  const externalBy = new Map<number, ExternalCheckStatus>()
  for (const e of inp.cards?.external ?? []) externalBy.set(e.proxy_id, e)

  const out: AssetCard[] = []
  for (const n of inp.nodes) {
    const key = `NODE-${n.id}`
    const m = inp.metrics[n.id]
    const probes = sortProbes(probesBy.get(n.id) ?? [])
    const outbound = outboundBy.get(n.id) ?? []
    const boce = boceBy.get(n.id) ?? []
    const shown = pickShown(pickBy.get(n.id), outboundAllBy.get(n.id) ?? [], outbound, probes, boce)
    const activeCount =
      outbound.length + probes.filter((p) => p.enabled).length + boce.filter((p) => p.enabled).length
    const shownActive = [...shown.ingress, ...shown.outbound].filter((p) => p.enabled).length
    const l = labels.get(key)
    const statuses = new Set<StatusKey>()
    const cloudStopped = !!n.cloud && n.cloud.instance_status !== '' && n.cloud.instance_status !== 'Running'
    if (n.status === 'ONLINE') statuses.add('ONLINE')
    if (n.status === 'OFFLINE' || n.status === 'DEPLOY_FAILED') statuses.add('OFFLINE')
    if (n.status === 'DISABLED') statuses.add('DISABLED')
    if (cloudStopped) statuses.add('STOPPED')
    const stale = !!m && (isMetricsStale(m.collected_at) || isPreChange(n, m.collected_at))
    const lr = inp.netRates?.[n.id]
    const now = inp.now ?? Date.now()
    const live =
      lr && !isPreChange(n, lr.at) && now - new Date(lr.at).getTime() <= netRateMaxAgeMs(inp.netRateInterval) ? lr : null
    if (stale || [...outbound, ...probes].some((p) => p.enabled && p.state === 'STALE')) statuses.add('STALE')
    if (outbound.some(failing) || probes.some(failing)) statuses.add('PROBE_FAIL')
    const monitor = monitorOf(outbound, probes, boce, inp.cards)
    if (monitor === 'NONE' || monitor === 'PAUSED' || monitor === 'GLOBAL_OFF') statuses.add('MONITOR_OFF')
    if (expiringState(n.expiry)) statuses.add('EXPIRING')
    const cyc = inp.cycles[n.id]
    out.push({
      key,
      kind: 'NODE',
      id: n.id,
      name: n.display_name,
      subtitle: n.name,
      type: n.role === 'RELAY' ? 'RELAY' : 'LANDING',
      selfHosted: true,
      region: l?.region ?? '',
      tags: l?.tags ?? [],
      group: '自建节点',
      to: `/nodes/${n.id}`,
      node: n,
      proxy: null,
      source: null,
      expiry: n.expiry ?? null,
      resources: {
        cpu: m ? m.cpu_percent : null,
        mem: m ? pct(m.mem_used_kb, m.mem_total_kb) : null,
        disk: m ? pct(m.disk_used_kb, m.disk_total_kb) : null,
        memDetail: m ? `${formatBytes(m.mem_used_kb * 1024)} / ${formatBytes(m.mem_total_kb * 1024)}` : '',
        diskDetail: m ? `${formatBytes(m.disk_used_kb * 1024)} / ${formatBytes(m.disk_total_kb * 1024)}` : '',
        rx: live ? live.rx_bps : m ? m.net_rx_bps : null,
        tx: live ? live.tx_bps : m ? m.net_tx_bps : null,
        stale,
        collectedAt: m?.collected_at ?? null,
        rateLive: !!live,
        rateAt: live ? live.at : (m?.collected_at ?? null),
        rateStale: live ? false : stale,
        rateWindow: live ? live.window_seconds : null,
      },
      resourcesNote: m ? '' : '还没有资源采样',
      traffic:
        n.role === 'RELAY'
          ? null
          : {
              used: cyc ? cyc.used_bytes : null,
              quota: cyc?.quota_bytes ?? n.traffic_quota_bytes,
              warning: cyc?.warning_level,
              source: '本流量周期 · 计费口径',
            },
      trafficNote: n.role === 'RELAY' ? '中转主机,面板不计流量' : '',
      outbound,
      probes,
      boce,
      shownIngress: shown.ingress,
      shownOutbound: shown.outbound,
      shownInvalid: shown.invalid,
      hiddenCount: Math.max(activeCount - shownActive, 0),
      monitor,
      external: null,
      billing: billing.get(key) ?? null,
      cardsLoaded: !!inp.cards,
      statuses,
      cloudStopped,
      latency: latencyOf(outbound, probes, boce),
      uptime: live
        ? { seconds: live.uptime_seconds, at: live.at }
        : m && !stale && m.uptime_seconds > 0
          ? { seconds: m.uptime_seconds, at: m.collected_at }
          : null,
    })
  }

  for (const s of inp.sources) {
    const key = `PROXY_SOURCE-${s.id}`
    const l = labels.get(key)
    const statuses = new Set<StatusKey>(['EXTERNAL'])
    if (!s.enabled) statuses.add('DISABLED')
    if (expiringState(s.expiry)) statuses.add('EXPIRING')
    const seen = !!s.upstream_seen_at
    out.push({
      key,
      kind: 'PROXY_SOURCE',
      id: s.id,
      name: s.name,
      subtitle: `${s.proxy_count} 条线路`,
      type: 'SOURCE',
      selfHosted: false,
      region: l?.region ?? '',
      tags: l?.tags ?? [],
      group: s.name,
      to: '/external-proxies',
      node: null,
      proxy: null,
      source: s,
      expiry: s.expiry ?? null,
      resources: null,
      resourcesNote: '购买的外部订阅,没有资源数据',
      traffic: seen ? { used: s.upstream_used_bytes, quota: s.upstream_total_bytes, source: '上游订阅报告的用量' } : null,
      trafficNote: seen ? '' : '上游没有报告用量',
      outbound: [],
      probes: [],
      boce: [],
      shownIngress: [],
      shownOutbound: [],
      shownInvalid: 0,
      hiddenCount: 0,
      monitor: 'MANUAL',
      external: null,
      billing: billing.get(key) ?? null,
      cardsLoaded: !!inp.cards,
      statuses,
      cloudStopped: false,
      latency: null,
      uptime: null,
    })
  }

  for (const p of inp.proxies) {
    // 跟随来源的条目不单列:来源那一张卡已经代表了它们(与提醒、计费的去重规则一致)。
    const own = !!p.expiry && p.expiry.has_profile && !p.expiry.inherited
    if (p.source_id != null && !own) continue
    const key = `EXTERNAL_PROXY-${p.id}`
    const l = labels.get(key)
    const statuses = new Set<StatusKey>(['EXTERNAL'])
    if (p.status === 'DISABLED') statuses.add('DISABLED')
    if (expiringState(p.expiry)) statuses.add('EXPIRING')
    const ext = externalBy.get(p.id) ?? null
    out.push({
      key,
      kind: 'EXTERNAL_PROXY',
      id: p.id,
      name: p.final_display_name || p.display_name,
      subtitle: p.source_name ? `来自 ${p.source_name} · ${p.protocol}` : `手工添加 · ${p.protocol}`,
      type: 'EXTERNAL',
      selfHosted: false,
      region: l?.region ?? '',
      tags: l?.tags ?? [],
      group: p.source_name || '手工外部代理',
      to: '/external-proxies',
      node: null,
      proxy: p,
      source: null,
      expiry: p.expiry ?? null,
      resources: null,
      resourcesNote: '外部线路,没有资源数据',
      traffic: null,
      trafficNote: '外部线路,没有流量数据',
      outbound: [],
      probes: [],
      boce: [],
      shownIngress: [],
      shownOutbound: [],
      shownInvalid: 0,
      hiddenCount: 0,
      monitor: 'MANUAL',
      external: ext,
      billing: billing.get(key) ?? null,
      cardsLoaded: !!inp.cards,
      statuses,
      cloudStopped: false,
      latency: ext?.last_check_ok && ext.latency_ms != null ? ext.latency_ms : null,
      uptime: null,
    })
  }
  return out
}

// ---------- 筛选与排序 ----------

export type SortKey = 'DEFAULT' | 'LATENCY' | 'EXPIRY' | 'TRAFFIC' | 'PRICE'

export const sortOptions: { value: SortKey; label: string }[] = [
  { value: 'DEFAULT', label: '默认顺序' },
  { value: 'LATENCY', label: '按延迟(出站优先)' },
  { value: 'EXPIRY', label: '按到期时间' },
  { value: 'TRAFFIC', label: '按流量使用率' },
  { value: 'PRICE', label: '按价格(同币种内)' },
]

export interface AssetFilters {
  q: string
  region: string
  group: string
  tag: string
  status: StatusKey | ''
  type: AssetType | ''
  sort: SortKey
}

export const blankFilters = (): AssetFilters => ({
  q: '',
  region: '',
  group: '',
  tag: '',
  status: '',
  type: '',
  sort: 'DEFAULT',
})

/** 「未设置地区」在筛选里的取值。 */
export const REGION_UNSET = '__unset__'

export function filterCards(cards: AssetCard[], f: AssetFilters): AssetCard[] {
  const q = f.q.trim().toLowerCase()
  return cards.filter((c) => {
    if (q && !`${c.name} ${c.subtitle}`.toLowerCase().includes(q)) return false
    if (f.region === REGION_UNSET ? c.region !== '' : f.region && c.region !== f.region) return false
    if (f.group && c.group !== f.group) return false
    if (f.tag && !c.tags.includes(f.tag)) return false
    if (f.status && !c.statuses.has(f.status)) return false
    if (f.type && c.type !== f.type) return false
    return true
  })
}

function trafficPercent(c: AssetCard): number | null {
  if (!c.traffic || c.traffic.used == null || c.traffic.quota <= 0) return null
  return c.traffic.used / c.traffic.quota
}

function expiryTime(c: AssetCard): number | null {
  const v = c.expiry?.expires_at
  if (!v) return null
  const t = new Date(v).getTime()
  return Number.isNaN(t) ? null : t
}

/** null 一律排在最后,不论升序降序 —— 没数据的不该冒充「最快」或「最便宜」。 */
function cmpNullable(a: number | null, b: number | null, desc = false): number {
  if (a === null && b === null) return 0
  if (a === null) return 1
  if (b === null) return -1
  return desc ? b - a : a - b
}

export function sortCards(cards: AssetCard[], sort: SortKey): AssetCard[] {
  const list = [...cards]
  switch (sort) {
    case 'LATENCY':
      return list.sort((a, b) => cmpNullable(a.latency, b.latency))
    case 'EXPIRY':
      return list.sort((a, b) => cmpNullable(expiryTime(a), expiryTime(b)))
    case 'TRAFFIC':
      return list.sort((a, b) => cmpNullable(trafficPercent(a), trafficPercent(b), true))
    case 'PRICE':
      // 不同币种不比大小:先按币种分组(没填金额的排最后),组内按月均成本、再按本期金额降序。
      return list.sort((a, b) => {
        const ca = a.billing?.has_billing ? a.billing.valuation.currency : ''
        const cb = b.billing?.has_billing ? b.billing.valuation.currency : ''
        if (ca !== cb) return ca === '' ? 1 : cb === '' ? -1 : ca.localeCompare(cb)
        const ma = a.billing?.valuation.monthly_cost_minor ?? (a.billing?.valuation.amount_minor ?? -1)
        const mb = b.billing?.valuation.monthly_cost_minor ?? (b.billing?.valuation.amount_minor ?? -1)
        return cmpNullable(ma < 0 ? null : ma, mb < 0 ? null : mb, true)
      })
  }
  return list
}

export function filterOptions(cards: AssetCard[]) {
  const regions = [...new Set(cards.map((c) => c.region).filter(Boolean))].sort()
  const groups = [...new Set(cards.map((c) => c.group))]
  const tags = [...new Set(cards.flatMap((c) => c.tags))].sort((a, b) => a.localeCompare(b))
  const hasUnsetRegion = cards.some((c) => !c.region)
  return { regions, groups, tags, hasUnsetRegion }
}

// ---------- 概览 ----------

export interface AssetOverview {
  nodeTotal: number
  online: number
  offline: number
  /** 实时 ↓ / ↑ 只加没过期的采样;过期的台数另给,不偷偷当实时。 */
  rx: number
  tx: number
  rateSamples: number
  rateStale: number
  /** 本周期流量合计(计费口径);读到的台数另给。 */
  cycleUsed: number
  cycleNodes: number
  expiring7d: number | null
  overdue: number | null
  costs: CostRow[]
  /** 生效的探测序列数与其中失败的(出站 + 入口,分开计) */
  outboundTargets: number
  outboundFailing: number
  probeTargets: number
  probeFailing: number
  /** Boce 外部入站拨测:在测的序列与其中失败的(单列,不并进上面两个数) */
  boceTargets: number
  boceFailing: number
}

export function buildOverview(cards: AssetCard[], dash: DashboardCards | null): AssetOverview {
  const nodes = cards.filter((c) => c.kind === 'NODE')
  const ov: AssetOverview = {
    nodeTotal: nodes.length,
    online: nodes.filter((c) => c.node?.status === 'ONLINE').length,
    offline: nodes.filter((c) => c.node?.status === 'OFFLINE' || c.node?.status === 'DEPLOY_FAILED').length,
    rx: 0,
    tx: 0,
    rateSamples: 0,
    rateStale: 0,
    cycleUsed: 0,
    cycleNodes: 0,
    expiring7d: dash ? dash.expiring_7d : null,
    overdue: dash ? dash.overdue : null,
    costs: dash?.cost_summary ?? [],
    outboundTargets: 0,
    outboundFailing: 0,
    probeTargets: 0,
    probeFailing: 0,
    boceTargets: 0,
    boceFailing: 0,
  }
  for (const c of nodes) {
    const r = c.resources
    if (r && r.rx != null && r.tx != null) {
      if (r.rateStale) ov.rateStale++
      else {
        ov.rx += r.rx
        ov.tx += r.tx
        ov.rateSamples++
      }
    }
    if (c.traffic && c.traffic.used != null) {
      ov.cycleUsed += c.traffic.used
      ov.cycleNodes++
    }
    for (const p of c.outbound) {
      ov.outboundTargets++
      if (failing(p)) ov.outboundFailing++
    }
    for (const p of c.probes) {
      if (!p.enabled) continue
      ov.probeTargets++
      if (failing(p)) ov.probeFailing++
    }
    for (const p of c.boce) {
      if (!p.enabled) continue
      ov.boceTargets++
      if (failing(p)) ov.boceFailing++
    }
  }
  return ov
}

// ---------- 显示 ----------

export function money(amount: string, currency: string): string {
  if (!amount) return ''
  return currency ? `${amount} ${currency}` : amount
}

/** 「4.79 GB」→「4.79G」,「500.0 GB」→「500G」:半宽的格子里放「已用 / 总量」。 */
export function shortBytes(n: number): string {
  const s = formatBytes(n)
  const m = /^([\d.]+) ([KMGTP]?)B$/.exec(s)
  if (!m) return s
  const v = m[1].includes('.') ? m[1].replace(/\.?0+$/, '') : m[1]
  return `${v}${m[2] || 'B'}`
}

/** 开机时长的短说法:满一天说天,满一小时说小时,否则说分钟。 */
export function uptimeText(seconds: number): string {
  if (seconds >= 86400) return `${Math.floor(seconds / 86400)} 天`
  if (seconds >= 3600) return `${Math.floor(seconds / 3600)} 小时`
  return `${Math.max(1, Math.floor(seconds / 60))} 分钟`
}

/** 读数那一刻的开机秒数按「现在」往前推。读数时间不可解析时不推。 */
export function uptimeNow(u: CardUptime, now = Date.now()): number {
  const at = new Date(u.at).getTime()
  return Number.isNaN(at) ? u.seconds : u.seconds + Math.max(0, (now - at) / 1000)
}

/**
 * 地区代码(ISO 3166-1 alpha-2,后端已校验成两位大写字母)→ 国旗。
 * 两个「区域指示符」拼成一面旗;字形由 styles/flags.css 的字体给,Windows 上也看得到。
 * 不是两位字母时返回空串,调用方画「未设置」。
 */
export function regionFlag(code: string): string {
  if (!/^[A-Z]{2}$/.test(code)) return ''
  return String.fromCodePoint(...[...code].map((ch) => 0x1f1e6 + ch.charCodeAt(0) - 65))
}

// 比例、延迟、地址族的文字在 probeView.ts,这里转出去给原来的调用方。
export { familyText, rateText, rttText } from './probeView'
