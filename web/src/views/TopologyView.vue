<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AccessTier,
  type ChainPath,
  type MieruInbound,
  type Node,
  type NodeInbound,
  type NodeRelay,
  type TopoEdge,
  type TopoEntry,
  type TopologySnapshot,
} from '@/api/client'
import { LbInfoTip } from '@/components/lb'
import { regionFlag } from '@/components/asset/assetModel'
import TopoCard from '@/components/topology/TopoCard.vue'
import { roundedPath, type LayoutResult } from '@/components/topology/layout'
import { runLayout } from '@/components/topology/runLayout'
import { AUX_H, AUX_W, CARD_W, cardHeight, inPortY, outPortY } from '@/components/topology/cardMetrics'
import {
  ADDR_MODE_TEXT,
  EDGE_STATE_TEXT,
  edgeKindText,
  endpointText,
  entryTitle,
  familyText,
  kindLabel,
  PENDING_TEXT,
  protocolLabel,
} from '@/components/topology/topoText'
import InboundChainModal from '@/components/node/InboundChainModal.vue'
import MieruChainModal from '@/components/node/MieruChainModal.vue'
import RelayFormModal from '@/components/node/RelayFormModal.vue'
import InboundFormModal from '@/components/node/InboundFormModal.vue'
import MieruInboundFormModal from '@/components/node/MieruInboundFormModal.vue'

/**
 * 链路画布(v23.3):按入口聚合、从左到右分层的全局配置拓扑。
 *
 * - **只是配置拓扑**:库里存的下一跳,不是节点上正在运行的关系;读快照、平移、缩放、筛选一次 SSH 都不会有。
 *   状态只摆已有的证据(配置状态、待下发、部署失败),不把它们合成一个「整条链路正常」的绿灯。
 * - 一个入口一张卡片;同一个入口的多条地址都在这一张里;多个上游共用的中继也只有一张。
 * - 节点位置由布局决定:不能拖节点、拖线连边、拖边改目标,也没有删除快捷键 —— 业务关系只经现有表单改。
 * - 状态刷新只换样式;结构或卡片尺寸变了才重排,而且不自动「适应全部」,视口与选中项都留着。
 */
const route = useRoute()
const router = useRouter()

const snap = ref<TopologySnapshot | null>(null)
const loading = ref(false)
const loadError = ref('')
const lay = ref<LayoutResult | null>(null)
const layoutError = ref('')
const layoutMs = ref(0)
const laying = ref(false)
const fetchMs = ref(0)

const expanded = reactive(new Set<string>())
const selectedKey = ref<string | null>(null)
const selectedEdge = ref<string | null>(null)
const hoverEdge = ref<string | null>(null)
const focusMode = ref(false)
const search = ref('')
const drawerOpen = ref(false)
const problemsOpen = ref(false)
// 图例在手机宽度下默认收起(它会盖住半屏),桌面默认展开。
const legendOpen = ref(typeof window === 'undefined' || window.innerWidth >= 768)

// ---------------------------------------------------------------- 本机视图偏好

interface Prefs {
  minimap: boolean
  kinds: string[]
  regions: string[]
  tiers: number[]
  states: string[]
  hosts: number[]
}
const PREF_KEY = 'lb.topology.prefs'
function readPrefs(): Prefs {
  const base: Prefs = { minimap: true, kinds: [], regions: [], tiers: [], states: [], hosts: [] }
  try {
    const raw = localStorage.getItem(PREF_KEY)
    if (raw) return { ...base, ...(JSON.parse(raw) as Partial<Prefs>) }
  } catch {
    /* 隐私模式、被清掉的站点数据:照默认来 */
  }
  return base
}
const prefs = reactive<Prefs>(readPrefs())
watch(
  prefs,
  (p) => {
    try {
      localStorage.setItem(PREF_KEY, JSON.stringify(p))
    } catch {
      /* 存不了就只在这一次会话里有效 */
    }
  },
  { deep: true },
)

// ---------------------------------------------------------------- 数据

const entries = computed(() => snap.value?.entries ?? [])
const byKey = computed(() => new Map(entries.value.map((e) => [e.key, e])))
const tiersById = computed(() => new Map((snap.value?.tiers ?? []).map((t) => [t.id, t])))
const auxByKey = computed(() => new Map((snap.value?.aux ?? []).map((a) => [a.key, a])))
const edges = computed(() => snap.value?.edges ?? [])
const outEdge = computed(() => new Map(edges.value.map((e) => [e.from, e])))
const inEdges = computed(() => {
  const m = new Map<string, TopoEdge[]>()
  for (const e of edges.value) {
    if (!m.has(e.to)) m.set(e.to, [])
    m.get(e.to)!.push(e)
  }
  return m
})

async function load(silent = false) {
  if (!silent) loading.value = true
  loadError.value = ''
  const started = performance.now()
  try {
    snap.value = await api.topology()
    fetchMs.value = Math.round(performance.now() - started)
  } catch (e) {
    // 读取失败不画成空图、也不写「0 个入口」:那与「真的一个都没有」长得一样。
    loadError.value = e instanceof ApiError ? e.message : '读取链路拓扑失败'
  } finally {
    loading.value = false
  }
}

// ---------------------------------------------------------------- 筛选与聚焦

const kindOptions = ['SINGBOX', 'MIERU', 'NGINX', 'REALM', 'EXTERNAL'].map((k) => ({ value: k, label: kindLabel(k) }))
const stateOptions = [
  { value: 'disabled', label: '已停用' },
  { value: 'undeployed', label: '未部署' },
  { value: 'pending', label: '待下发 / 失败' },
  { value: 'problem', label: '有问题' },
  { value: 'direct', label: '允许直接接入' },
]
const regionOptions = computed(() => {
  const set = new Set(entries.value.map((e) => e.region || ''))
  return [...set].sort().map((r) => ({ value: r, label: r ? `${regionFlag(r)} ${r}` : '地区未设置' }))
})
const tierOptions = computed(() => (snap.value?.tiers ?? []).map((t) => ({ value: t.id, label: t.name })))
const hostOptions = computed(() => (snap.value?.hosts ?? []).map((h) => ({ value: h.id, label: h.display_name })))

const filtering = computed(
  () => prefs.kinds.length + prefs.regions.length + prefs.tiers.length + prefs.states.length + prefs.hosts.length > 0,
)

function matchesFilter(e: TopoEntry): boolean {
  if (prefs.kinds.length && !prefs.kinds.includes(e.kind)) return false
  if (prefs.regions.length && !prefs.regions.includes(e.region || '')) return false
  if (prefs.tiers.length && !prefs.tiers.includes(e.tier_id)) return false
  if (prefs.hosts.length && !prefs.hosts.includes(e.node_id ?? 0)) return false
  if (prefs.states.length) {
    const hit = prefs.states.some(
      (s) =>
        (s === 'disabled' && !e.enabled) ||
        (s === 'undeployed' && !e.deployed && e.kind !== 'EXTERNAL') ||
        (s === 'pending' && !!e.pending) ||
        (s === 'problem' && e.problems.length > 0) ||
        (s === 'direct' && e.direct_access),
    )
    if (!hit) return false
  }
  return true
}

/** 一组入口沿配置关系的上下游闭包:筛选与聚焦都保留必要的中间节点,不画一条并不存在的直连边。 */
function closure(seed: Iterable<string>): Set<string> {
  const out = new Set<string>()
  const down = [...seed]
  const up = [...seed]
  for (const k of seed) out.add(k)
  while (down.length) {
    const k = down.pop()!
    const e = outEdge.value.get(k)
    if (e && !out.has(e.to)) {
      out.add(e.to)
      down.push(e.to)
    }
  }
  const seenUp = new Set<string>(seed)
  while (up.length) {
    const k = up.pop()!
    for (const e of inEdges.value.get(k) ?? []) {
      if (!seenUp.has(e.from)) {
        seenUp.add(e.from)
        out.add(e.from)
        up.push(e.from)
      }
    }
  }
  return out
}

const matched = computed(() => entries.value.filter(matchesFilter).map((e) => e.key))
const filteredKeys = computed(() => (filtering.value ? closure(matched.value) : null))
const focusKeys = computed(() => (focusMode.value && selectedKey.value ? closure([selectedKey.value]) : null))
/** 画出来的入口(与辅助节点)。 */
const visibleKeys = computed<Set<string>>(() => {
  const base = focusKeys.value ?? filteredKeys.value
  if (base) return base
  const all = new Set<string>(entries.value.map((e) => e.key))
  for (const a of snap.value?.aux ?? []) all.add(a.key)
  return all
})
const pathKeys = computed(() => (selectedKey.value ? closure([selectedKey.value]) : null))
const counts = computed(() => {
  const total = entries.value.length
  const shown = [...visibleKeys.value].filter((k) => byKey.value.has(k)).length
  const m = filtering.value ? matched.value.length : total
  return { total, matched: m, kept: Math.max(0, shown - m), shown }
})
/** 筛选时被隐藏的上游个数(卡片上写「被 N 个入口引用(隐藏 M)」)。 */
function hiddenUpstream(key: string): number {
  return (inEdges.value.get(key) ?? []).filter((e) => !visibleKeys.value.has(e.from)).length
}

// ---------------------------------------------------------------- 布局

const sizeOf = (e: TopoEntry) => ({ w: CARD_W, h: cardHeight(e, expanded.has(e.key)) })
const hostOrder = computed(() => new Map((snap.value?.hosts ?? []).map((h, i) => [h.id, h.sort_order * 10000 + i])))

const layoutInput = computed(() => {
  const nodes: { id: string; w: number; h: number; order: number }[] = []
  for (const e of entries.value) {
    if (!visibleKeys.value.has(e.key)) continue
    const { w, h } = sizeOf(e)
    const base = e.node_id ? (hostOrder.value.get(e.node_id) ?? 0) : 9_000_000
    nodes.push({ id: e.key, w, h, order: base + e.sort_order * 10 + (e.id % 10) / 10 })
  }
  for (const a of snap.value?.aux ?? []) {
    const from = edges.value.find((x) => x.to === a.key)?.from
    if (from && visibleKeys.value.has(from)) nodes.push({ id: a.key, w: AUX_W, h: AUX_H, order: 9_500_000 })
  }
  const ids = new Set(nodes.map((n) => n.id))
  const es = edges.value
    .filter((e) => ids.has(e.from) && ids.has(e.to))
    .map((e) => {
      const from = byKey.value.get(e.from)!
      const to = byKey.value.get(e.to)
      return {
        id: e.id,
        from: e.from,
        to: e.to,
        fromY: outPortY(from, expanded.has(from.key)),
        toY: to ? inPortY(to, e.endpoint_id, expanded.has(to.key)) : AUX_H / 2,
      }
    })
  return { nodes, edges: es }
})
/** 结构签名:卡片集合、尺寸与边。只有它变了才重排 —— 状态刷新不重排、不缩放。 */
const signature = computed(() =>
  JSON.stringify([layoutInput.value.nodes.map((n) => [n.id, n.h]), layoutInput.value.edges.map((e) => [e.id, e.to, e.toY])]),
)

let firstLayout = true
async function relayout() {
  laying.value = true
  layoutError.value = ''
  try {
    const { result, ms } = await runLayout(layoutInput.value.nodes, layoutInput.value.edges)
    lay.value = result
    layoutMs.value = Math.round(ms)
    if (firstLayout) {
      firstLayout = false
      await nextTick()
      const focus = typeof route.query.focus === 'string' ? route.query.focus : ''
      if (focus && result.nodes[focus]) focusOn(focus)
      else fitView()
    }
  } catch (e) {
    // 失败或超时:保留上一次可用的视图,给出重试。
    layoutError.value = e instanceof Error ? e.message : '布局失败'
  } finally {
    laying.value = false
  }
}
watch(signature, () => {
  if (snap.value) void relayout()
})

// ---------------------------------------------------------------- 视口

const viewport = ref<HTMLElement | null>(null)
const view = reactive({ x: 0, y: 0, k: 1 })
const MIN_K = 0.15
const MAX_K = 2
const compact = computed(() => view.k < 0.55)

function clampK(k: number) {
  return Math.min(MAX_K, Math.max(MIN_K, k))
}
function zoomAt(k: number, cx: number, cy: number) {
  const nk = clampK(k)
  view.x = cx - ((cx - view.x) * nk) / view.k
  view.y = cy - ((cy - view.y) * nk) / view.k
  view.k = nk
}
function zoomBy(f: number) {
  const el = viewport.value
  if (!el) return
  zoomAt(view.k * f, el.clientWidth / 2, el.clientHeight / 2)
}
/** 适应全部:上面让出工具条、下面让出图例,卡片不被它们盖住。 */
const FIT_TOP = 56
const FIT_BOTTOM = 52
function fitView() {
  const el = viewport.value
  const l = lay.value
  if (!el || !l || !l.width) return
  const h = el.clientHeight - FIT_TOP - FIT_BOTTOM
  const k = clampK(Math.min((el.clientWidth - 32) / l.width, h / l.height, 1))
  view.k = k
  view.x = (el.clientWidth - l.width * k) / 2
  view.y = FIT_TOP + Math.max(0, (h - l.height * k) / 2)
}
function resetView() {
  view.k = 1
  view.x = 16
  view.y = FIT_TOP
}
function focusOn(key: string) {
  const el = viewport.value
  const n = lay.value?.nodes[key]
  selectedKey.value = byKey.value.has(key) ? key : null
  if (!el || !n) return
  if (view.k < 0.6) view.k = 0.85
  view.x = el.clientWidth / 2 - (n.x + n.w / 2) * view.k
  view.y = el.clientHeight / 2 - (n.y + n.h / 2) * view.k
}

// 拖动空白处平移;滚轮缩放(以光标为中心);两指捏合缩放。卡片上的点击不触发平移。
const pointers = new Map<number, { x: number; y: number }>()
let pinch: { d: number; k: number } | null = null
let dragged = false
function onPointerDown(ev: PointerEvent) {
  if ((ev.target as HTMLElement).closest('.tc, .tv__aux, button, a')) return
  viewport.value?.setPointerCapture(ev.pointerId)
  pointers.set(ev.pointerId, { x: ev.clientX, y: ev.clientY })
  dragged = false
  if (pointers.size === 2) {
    const [a, b] = [...pointers.values()]
    pinch = { d: Math.hypot(a.x - b.x, a.y - b.y), k: view.k }
  }
}
function onPointerMove(ev: PointerEvent) {
  const p = pointers.get(ev.pointerId)
  if (!p) return
  if (pointers.size === 2 && pinch) {
    pointers.set(ev.pointerId, { x: ev.clientX, y: ev.clientY })
    const [a, b] = [...pointers.values()]
    const rect = viewport.value!.getBoundingClientRect()
    zoomAt((pinch.k * Math.hypot(a.x - b.x, a.y - b.y)) / pinch.d, (a.x + b.x) / 2 - rect.left, (a.y + b.y) / 2 - rect.top)
    return
  }
  const dx = ev.clientX - p.x
  const dy = ev.clientY - p.y
  if (Math.abs(dx) + Math.abs(dy) > 2) dragged = true
  view.x += dx
  view.y += dy
  pointers.set(ev.pointerId, { x: ev.clientX, y: ev.clientY })
}
function onPointerUp(ev: PointerEvent) {
  pointers.delete(ev.pointerId)
  if (pointers.size < 2) pinch = null
}
function onBackgroundClick() {
  if (dragged) return
  selectedKey.value = null
  selectedEdge.value = null
  menu.open = false
}
function onWheel(ev: WheelEvent) {
  ev.preventDefault()
  const rect = viewport.value!.getBoundingClientRect()
  zoomAt(view.k * Math.exp(-ev.deltaY * 0.0015), ev.clientX - rect.left, ev.clientY - rect.top)
}
function onKey(ev: KeyboardEvent) {
  const step = 48
  if (ev.key === 'ArrowLeft') view.x += step
  else if (ev.key === 'ArrowRight') view.x -= step
  else if (ev.key === 'ArrowUp') view.y += step
  else if (ev.key === 'ArrowDown') view.y -= step
  else if (ev.key === '+' || ev.key === '=') zoomBy(1.2)
  else if (ev.key === '-') zoomBy(1 / 1.2)
  else if (ev.key === '0') fitView()
  else if (ev.key === 'Escape') {
    selectedKey.value = null
    selectedEdge.value = null
    focusMode.value = false
    menu.open = false
  } else return
  ev.preventDefault()
}

// ---------------------------------------------------------------- 边

interface DrawnEdge {
  e: TopoEdge
  d: string
  back: boolean
  mid: [number, number]
}
const drawnEdges = computed<DrawnEdge[]>(() => {
  const l = lay.value
  if (!l) return []
  const out: DrawnEdge[] = []
  for (const e of edges.value) {
    const r = l.edges[e.id]
    if (!r || !r.points.length) continue
    const pts = r.points
    const mi = Math.floor(pts.length / 2)
    const a = pts[mi - 1] ?? pts[0]
    const b = pts[mi]
    out.push({ e, d: roundedPath(pts), back: r.back, mid: [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2] })
  }
  return out
})
function edgeOnPath(e: TopoEdge): boolean {
  const p = pathKeys.value
  return !!p && p.has(e.from) && p.has(e.to)
}
function selectEdge(e: TopoEdge) {
  selectedEdge.value = e.id
  // 选中的边接在目标卡片的哪一行:那一行在折叠区里时自动展开。
  const to = byKey.value.get(e.to)
  if (to && e.endpoint_id && !expanded.has(to.key)) {
    const idx = to.endpoints.findIndex((x) => x.id === e.endpoint_id)
    if (idx >= 3) expanded.add(to.key)
  }
}
const activeEdge = computed(() => {
  const id = selectedEdge.value ?? hoverEdge.value
  return id ? (edges.value.find((x) => x.id === id) ?? null) : null
})
function highlightFor(key: string): number | undefined {
  const e = activeEdge.value
  if (!e || e.to !== key) return undefined
  return e.endpoint_id ?? 0
}

// ---------------------------------------------------------------- 搜索

const searchHits = computed(() => {
  const q = search.value.trim().toLowerCase()
  if (!q) return []
  return entries.value
    .filter((e) =>
      [e.name, e.node_name ?? '', e.source ?? '', ...e.endpoints.map((x) => endpointText(x)), ...e.endpoints.map((x) => String(x.port))]
        .join(' ')
        .toLowerCase()
        .includes(q),
    )
    .slice(0, 20)
})
function jump(key: string) {
  search.value = ''
  if (!visibleKeys.value.has(key)) {
    focusMode.value = false
    prefs.kinds = []
    prefs.regions = []
    prefs.tiers = []
    prefs.states = []
    prefs.hosts = []
  }
  void nextTick(() => setTimeout(() => focusOn(key), 50))
}

// ---------------------------------------------------------------- 菜单与详情

const menu = reactive({ open: false, x: 0, y: 0, key: '' })
const menuEntry = computed(() => byKey.value.get(menu.key) ?? null)
function openMenu(key: string, ev: MouseEvent) {
  const rect = viewport.value!.getBoundingClientRect()
  menu.key = key
  menu.x = Math.min(ev.clientX - rect.left, rect.width - 220)
  menu.y = Math.min(ev.clientY - rect.top, rect.height - 260)
  menu.open = true
  selectedKey.value = key
}
const selected = computed(() => (selectedKey.value ? (byKey.value.get(selectedKey.value) ?? null) : null))
const selectedPath = ref<ChainPath | null>(null)
watch(selectedKey, async (k) => {
  selectedPath.value = null
  const e = k ? byKey.value.get(k) : null
  if (!e || e.kind === 'EXTERNAL') return
  try {
    selectedPath.value = await api.chainPath(e.kind, e.id)
  } catch {
    selectedPath.value = null
  }
})
const upstreamOf = computed(() =>
  selected.value ? (inEdges.value.get(selected.value.key) ?? []).map((e) => byKey.value.get(e.from)).filter(Boolean) as TopoEntry[] : [],
)

function copyAddresses(e: TopoEntry) {
  const text = e.endpoints.map((x) => endpointText(x)).join('\n')
  void navigator.clipboard?.writeText(text).then(
    () => message.success(`已复制 ${e.endpoints.length} 条地址`),
    () => message.error('复制失败,请在详情里手动复制'),
  )
}

// ---------------------------------------------------------------- 复用现有表单

const nodesCache = ref<Node[]>([])
const tiersCache = ref<AccessTier[]>([])
const relaysCache = ref<NodeRelay[]>([])
async function ensureForms() {
  if (nodesCache.value.length) return
  const [ns, ts, rs] = await Promise.all([api.nodes(), api.accessTiers(), api.relays()])
  nodesCache.value = ns.items ?? []
  tiersCache.value = ts.items ?? []
  relaysCache.value = rs.items ?? []
}
const formNode = ref<Node | null>(null)
const chainInbound = ref<NodeInbound | null>(null)
const chainOpen = ref(false)
const mieruTarget = ref<MieruInbound | null>(null)
const mieruChainOpen = ref(false)
const relayTarget = ref<NodeRelay | null>(null)
const relayOpen = ref(false)
const inboundFormOpen = ref(false)
const mieruFormOpen = ref(false)

async function act(action: string, e: TopoEntry) {
  menu.open = false
  try {
    switch (action) {
      case 'detail':
        selectedKey.value = e.key
        drawerOpen.value = true
        return
      case 'path':
        selectedKey.value = e.key
        focusMode.value = true
        return
      case 'host':
        if (e.node_id) void router.push(`/nodes/${e.node_id}/entries`)
        return
      case 'list':
        void router.push(`/inbounds?focus=${encodeURIComponent(e.key)}`)
        return
      case 'copy':
        copyAddresses(e)
        return
      case 'external':
        void router.push('/external-proxies')
        return
    }
    await ensureForms()
    formNode.value = nodesCache.value.find((n) => n.id === e.node_id) ?? null
    if (!formNode.value) {
      message.error('找不到这个入口所在的机器,请刷新')
      return
    }
    if (e.kind === 'SINGBOX') {
      chainInbound.value = formNode.value.inbounds.find((i) => i.id === e.id) ?? null
      if (action === 'next') chainOpen.value = true
      else inboundFormOpen.value = true
    } else if (e.kind === 'MIERU') {
      mieruTarget.value = formNode.value.mieru_inbounds.find((m) => m.id === e.id) ?? null
      if (action === 'next') mieruChainOpen.value = true
      else mieruFormOpen.value = true
    } else {
      relayTarget.value = relaysCache.value.find((r) => r.id === e.id) ?? null
      relayOpen.value = true
    }
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '打开表单失败')
  }
}

/** 保存之后只刷新数据:视口、选中项与抽屉都留着,不自动「适应全部」。 */
async function afterSave() {
  nodesCache.value = []
  await load(true)
}

const menuItems = computed(() => {
  const e = menuEntry.value
  if (!e) return []
  const items: { key: string; label: string; disabled?: string }[] = [{ key: 'detail', label: '查看详情' }]
  if (e.kind === 'EXTERNAL') {
    items.push({ key: 'external', label: '到外部代理页编辑' })
    items.push({ key: 'next', label: '设置下一跳', disabled: '外部代理是别人的机器,面板不给它设下一跳' })
  } else if (e.kind === 'SINGBOX' || e.kind === 'MIERU') {
    items.push({ key: 'edit', label: '编辑入口' })
    items.push({ key: 'next', label: e.kind === 'MIERU' ? '设置出口' : '设置下一跳' })
  } else {
    items.push({ key: 'edit', label: '编辑 / 设置转发目标' })
  }
  items.push({ key: 'path', label: '查看完整路径' })
  if (e.node_id) items.push({ key: 'host', label: '查看所属主机' })
  items.push({ key: 'list', label: '定位到入口列表' })
  items.push({ key: 'copy', label: '复制地址' })
  return items
})

// ---------------------------------------------------------------- 小地图

const MINI_W = 180
const miniScale = computed(() => {
  const l = lay.value
  if (!l || !l.width) return 0
  return Math.min(MINI_W / l.width, 120 / l.height)
})
const viewRect = computed(() => {
  const el = viewport.value
  const s = miniScale.value
  if (!el || !s) return null
  return { x: (-view.x / view.k) * s, y: (-view.y / view.k) * s, w: (el.clientWidth / view.k) * s, h: (el.clientHeight / view.k) * s }
})
function miniJump(ev: MouseEvent) {
  const el = viewport.value
  const s = miniScale.value
  if (!el || !s) return
  const r = (ev.currentTarget as SVGElement).getBoundingClientRect()
  const wx = (ev.clientX - r.left) / s
  const wy = (ev.clientY - r.top) / s
  view.x = el.clientWidth / 2 - wx * view.k
  view.y = el.clientHeight / 2 - wy * view.k
}

// ---------------------------------------------------------------- 低频刷新(页面可见时)

let timer: ReturnType<typeof setInterval> | undefined
const hidden = ref(document.visibilityState === 'hidden')
function onVisibility() {
  hidden.value = document.visibilityState === 'hidden'
  if (!hidden.value) void load(true)
}
onMounted(async () => {
  document.addEventListener('visibilitychange', onVisibility)
  timer = setInterval(() => {
    if (!hidden.value && !chainOpen.value && !relayOpen.value && !mieruChainOpen.value) void load(true)
  }, 60_000)
  await load()
  if (snap.value) await relayout()
})
onBeforeUnmount(() => {
  document.removeEventListener('visibilitychange', onVisibility)
  clearInterval(timer)
})

const problems = computed(() => snap.value?.problems ?? [])
const problemKindText: Record<string, string> = {
  MISSING_TARGET: '目标缺失',
  LEGACY_ADDR: '待确认旧地址',
  LOOP: '成环',
  PENDING: '待下发',
  DEPLOY_FAILED: '部署失败',
  UNKNOWN: '状态未知',
  INCOMPATIBLE: '能力不兼容',
}
function locateProblem(p: { entry?: string; edge?: string }) {
  problemsOpen.value = false
  if (p.edge) selectedEdge.value = p.edge
  if (p.entry) jump(p.entry)
}

function pos(key: string) {
  const n = lay.value?.nodes[key]
  return n ? { left: `${n.x}px`, top: `${n.y}px`, width: `${n.w}px`, height: `${n.h}px` } : { display: 'none' }
}
</script>

<template>
  <div class="tv" :class="{ 'is-hidden': hidden }">
    <header class="tv__bar">
      <div class="tv__title">
        <h1>链路画布</h1>
        <span class="tv__tag">配置拓扑</span>
        <LbInfoTip
          text="画的是库里保存的下一跳,不是节点上正在运行的关系;连线只表达配置方向,不代表真实流量或链路健康。打开、平移、缩放、筛选都不连节点、不拨测、不下发。"
        />
      </div>
      <div class="tv__search">
        <a-input
          v-model:value="search"
          size="small"
          allow-clear
          placeholder="搜索入口名、主机名、IP、域名或端口"
          aria-label="搜索入口"
          data-testid="topo-search"
        />
        <ul v-if="searchHits.length" class="tv__hits" role="listbox">
          <li v-for="h in searchHits" :key="h.key">
            <button type="button" class="tv__hit" @click="jump(h.key)">
              <b>{{ h.name }}</b>
              <span>{{ h.node_name || h.source || '外部入口' }} · {{ kindLabel(h.kind) }}</span>
            </button>
          </li>
        </ul>
      </div>
      <div class="tv__filters">
        <a-select v-model:value="prefs.kinds" mode="multiple" size="small" :options="kindOptions" placeholder="类型" class="tv__f" max-tag-count="responsive" aria-label="按类型筛选" />
        <a-select v-model:value="prefs.regions" mode="multiple" size="small" :options="regionOptions" placeholder="地区" class="tv__f" max-tag-count="responsive" aria-label="按地区筛选" />
        <a-select v-model:value="prefs.tiers" mode="multiple" size="small" :options="tierOptions" placeholder="等级" class="tv__f" max-tag-count="responsive" aria-label="按访问等级筛选" />
        <a-select v-model:value="prefs.states" mode="multiple" size="small" :options="stateOptions" placeholder="状态" class="tv__f" max-tag-count="responsive" aria-label="按状态筛选" />
        <a-select v-model:value="prefs.hosts" mode="multiple" size="small" :options="hostOptions" placeholder="主机" class="tv__f" max-tag-count="responsive" show-search option-filter-prop="label" aria-label="按主机筛选" />
      </div>
      <div class="tv__counts" data-testid="topo-counts">
        <template v-if="filtering || focusKeys">
          匹配 {{ counts.matched }} · 关联保留 {{ counts.kept }} · 全图 {{ counts.total }}
        </template>
        <template v-else>全图 {{ counts.total }} 个入口 · {{ edges.length }} 条关系</template>
      </div>
      <div class="tv__actions">
        <a-button size="small" :class="{ 'is-warn': problems.length }" data-testid="topo-problems" @click="problemsOpen = true">
          异常 {{ problems.length }}
        </a-button>
        <a-button v-if="focusMode" size="small" @click="focusMode = false">回到全图</a-button>
        <a-button size="small" :loading="loading" @click="load()">刷新</a-button>
        <RouterLink to="/inbounds" class="tv__link">返回入口管理</RouterLink>
      </div>
    </header>

    <div v-if="loadError" class="tv__error" role="alert">
      读取链路拓扑失败:{{ loadError }}
      <a-button size="small" @click="load()">重试</a-button>
    </div>

    <div
      ref="viewport"
      class="tv__viewport"
      tabindex="0"
      aria-label="链路画布:拖动空白处平移,滚轮或双指缩放,方向键平移,加减号缩放,0 适应全部"
      data-testid="topo-viewport"
      @pointerdown="onPointerDown"
      @pointermove="onPointerMove"
      @pointerup="onPointerUp"
      @pointercancel="onPointerUp"
      @click="onBackgroundClick"
      @wheel="onWheel"
      @keydown="onKey"
    >
      <div v-if="!snap && loading" class="tv__empty">正在读取…</div>
      <div v-else-if="snap && !entries.length" class="tv__empty">还没有任何入口。去「自建节点」或「外部代理」添加。</div>
      <div v-if="layoutError" class="tv__lerr" role="alert">
        {{ layoutError }}(保留了上一次的视图)
        <a-button size="small" @click="relayout">重试布局</a-button>
      </div>

      <div
        v-if="lay"
        class="tv__world"
        :style="{ transform: `translate(${view.x}px, ${view.y}px) scale(${view.k})`, width: `${lay.width}px`, height: `${lay.height}px` }"
      >
        <div
          v-for="(b, i) in lay.blocks"
          :key="'b' + i"
          class="tv__block"
          :class="{ 'is-iso': b.kind === 'isolated' }"
          :style="{ left: `${b.x}px`, top: `${b.y}px`, width: `${b.w}px`, height: `${b.h}px` }"
        >
          <span v-if="b.kind === 'isolated'" class="tv__blabel">独立入口(没有上游与下一跳,直连出网或没有去向)· {{ b.count }}</span>
        </div>
        <svg class="tv__edges" :width="lay.width" :height="lay.height" aria-hidden="true">
          <defs>
            <marker id="tv-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
              <path d="M0,0 L10,5 L0,10 z" class="tv__arrowhead" />
            </marker>
          </defs>
          <g
            v-for="de in drawnEdges"
            :key="de.e.id"
            class="tv__edge"
            :class="[
              `is-${de.e.kind.toLowerCase()}`,
              `st-${de.e.state.toLowerCase()}`,
              {
                'is-back': de.back,
                'is-on': edgeOnPath(de.e) || selectedEdge === de.e.id,
                'is-dim': !!pathKeys && !edgeOnPath(de.e),
              },
            ]"
            :data-testid="`edge-${de.e.id}`"
            @mouseenter="hoverEdge = de.e.id"
            @mouseleave="hoverEdge = null"
            @click.stop="selectEdge(de.e)"
          >
            <path :d="de.d" class="tv__hit-path" />
            <path :d="de.d" class="tv__line" marker-end="url(#tv-arrow)" />
          </g>
        </svg>
        <div
          v-if="activeEdge && lay.edges[activeEdge.id]"
          class="tv__elabel"
          :style="{
            left: `${drawnEdges.find((d) => d.e.id === activeEdge!.id)?.mid[0] ?? 0}px`,
            top: `${drawnEdges.find((d) => d.e.id === activeEdge!.id)?.mid[1] ?? 0}px`,
          }"
          data-testid="edge-label"
        >
          <b>{{ edgeKindText(activeEdge.kind) }}</b>
          <span v-if="activeEdge.dial" class="lb-mono">{{ activeEdge.dial }}</span>
          <span>{{ activeEdge.addr_mode ? ADDR_MODE_TEXT[activeEdge.addr_mode] : EDGE_STATE_TEXT[activeEdge.state] }}</span>
          <span v-if="activeEdge.state !== 'OK'" class="tv__ewarn">{{ activeEdge.problem || EDGE_STATE_TEXT[activeEdge.state] }}</span>
        </div>

        <template v-for="e in entries" :key="e.key">
          <TopoCard
            v-if="lay.nodes[e.key]"
            :style="pos(e.key)"
            :entry="e"
            :tier="tiersById.get(e.tier_id)"
            :expanded="expanded.has(e.key)"
            :selected="selectedKey === e.key"
            :dimmed="!!pathKeys && !pathKeys.has(e.key)"
            :highlight-endpoint="highlightFor(e.key)"
            :compact="compact"
            :hidden-upstream="hiddenUpstream(e.key)"
            @select="selectedKey = e.key"
            @menu="(ev: MouseEvent) => openMenu(e.key, ev)"
            @toggle="expanded.has(e.key) ? expanded.delete(e.key) : expanded.add(e.key)"
          />
        </template>
        <template v-for="a in snap?.aux ?? []" :key="a.key">
          <div v-if="lay.nodes[a.key]" class="tv__aux" :class="`is-${a.kind.toLowerCase()}`" :style="pos(a.key)" :title="a.detail">
            <b>{{ a.kind === 'ADDRESS' ? '指定地址' : '缺失的目标' }}</b>
            <span class="lb-mono">{{ a.label }}</span>
            <span class="tv__auxd">{{ a.kind === 'ADDRESS' ? '后端未知' : '已删除或不存在' }}</span>
          </div>
        </template>
      </div>

      <!-- 右键与卡片「⋯」共用的菜单:触屏与键盘用户不必依赖右键。 -->
      <div v-if="menu.open && menuEntry" class="tv__menu" :style="{ left: `${menu.x}px`, top: `${menu.y}px` }" role="menu" data-testid="topo-menu" @click.stop @pointerdown.stop>
        <div class="tv__mhead">{{ entryTitle(menuEntry) }}</div>
        <button
          v-for="it in menuItems"
          :key="it.key"
          type="button"
          role="menuitem"
          class="tv__mitem"
          :disabled="!!it.disabled"
          :title="it.disabled || ''"
          :data-testid="`menu-${it.key}`"
          @click="act(it.key, menuEntry)"
        >
          {{ it.label }}
          <span v-if="it.disabled" class="tv__mwhy">{{ it.disabled }}</span>
        </button>
      </div>

      <div class="tv__tools" @pointerdown.stop @click.stop>
        <span class="tv__zoom">{{ Math.round(view.k * 100) }}%</span>
        <a-button size="small" aria-label="放大" @click="zoomBy(1.2)">+</a-button>
        <a-button size="small" aria-label="缩小" @click="zoomBy(1 / 1.2)">−</a-button>
        <a-button size="small" @click="fitView">适应全部</a-button>
        <a-button size="small" @click="resetView">默认视图</a-button>
        <a-button size="small" :loading="laying" @click="relayout">重新整理</a-button>
        <a-button size="small" class="tv__opt" @click="prefs.minimap = !prefs.minimap">{{ prefs.minimap ? '隐藏' : '显示' }}小地图</a-button>
        <a-button size="small" @click="legendOpen = !legendOpen">图例</a-button>
      </div>

      <div v-if="legendOpen" class="tv__legend" @pointerdown.stop @click.stop>
        <span class="tv__lg"><svg width="28" height="8"><line x1="0" y1="4" x2="28" y2="4" class="lg-proxy" /></svg>代理连接</span>
        <span class="tv__lg"><svg width="28" height="8"><line x1="0" y1="4" x2="28" y2="4" class="lg-fwd" /></svg>透明转发</span>
        <span class="tv__lg"><svg width="28" height="8"><line x1="0" y1="4" x2="28" y2="4" class="lg-warn" /></svg>待确认 / 后端未知</span>
        <span class="tv__lg"><svg width="28" height="8"><line x1="0" y1="4" x2="28" y2="4" class="lg-bad" /></svg>缺失 / 成环</span>
        <span class="tv__lg"><i class="tv__bar1" />左侧色条 = 访问等级</span>
      </div>

      <svg
        v-if="prefs.minimap && lay && miniScale"
        class="tv__mini"
        :width="lay.width * miniScale"
        :height="lay.height * miniScale"
        aria-label="小地图,点击跳转"
        @pointerdown.stop
        @click.stop="miniJump"
      >
        <rect
          v-for="(n, id) in lay.nodes"
          :key="id"
          :x="n.x * miniScale"
          :y="n.y * miniScale"
          :width="n.w * miniScale"
          :height="n.h * miniScale"
          :class="{ 'is-sel': selectedKey === id }"
        />
        <rect v-if="viewRect" class="tv__minivp" :x="viewRect.x" :y="viewRect.y" :width="viewRect.w" :height="viewRect.h" />
      </svg>

      <div v-if="selected && !drawerOpen" class="tv__sel" @pointerdown.stop @click.stop>
        <b>{{ entryTitle(selected) }}</b>
        <a-button size="small" @click="drawerOpen = true">详情</a-button>
        <a-button size="small" @click="focusMode = !focusMode">{{ focusMode ? '回到全图' : '只看这条链路' }}</a-button>
      </div>
      <div class="tv__perf" aria-hidden="true">接口 {{ fetchMs }} ms · 布局 {{ layoutMs }} ms</div>
    </div>

    <a-drawer
      v-model:open="drawerOpen"
      :title="selected ? entryTitle(selected) : '入口详情'"
      width="420"
      placement="right"
      :mask="false"
      data-testid="topo-drawer"
    >
      <template v-if="selected">
        <dl class="tv__dl">
          <dt>种类</dt>
          <dd>{{ kindLabel(selected.kind) }}<template v-if="selected.protocol"> · {{ protocolLabel(selected.protocol) }}</template></dd>
          <dt>所属</dt>
          <dd>{{ selected.node_name || selected.source || '外部入口(面板不管理)' }}</dd>
          <dt>访问</dt>
          <dd>
            {{ selected.access_mode === 'RESTRICTED' ? '仅指定用户' : (tiersById.get(selected.tier_id)?.name ?? '—') }}
            · 此刻能直接用的用户 {{ selected.direct_users }} 个
            <template v-if="!selected.subscription_enabled">· 订阅里不公开</template>
          </dd>
          <dt>状态</dt>
          <dd>
            {{ selected.enabled ? '启用' : '已停用' }}
            · {{ selected.deployed || selected.kind === 'EXTERNAL' ? '已部署过' : '未部署' }}
            <template v-if="selected.pending">· {{ PENDING_TEXT[selected.pending] }}</template>
          </dd>
          <dt>可连接的地址</dt>
          <dd>
            <ul class="tv__eplist">
              <li v-for="ep in selected.endpoints" :key="ep.id + ep.address">
                <span class="lb-mono">{{ endpointText(ep) }}</span>
                <span class="tv__dim">{{ familyText(ep.family) }}{{ ep.label ? ' · ' + ep.label : '' }}</span>
                <span v-if="ep.referenced" class="tv__dim">被 {{ ep.referenced }} 个入口选作目的地址</span>
              </li>
            </ul>
            <a-button size="small" @click="copyAddresses(selected)">复制全部地址</a-button>
            <p class="tv__note">这些是「可以被连的地址」,不是已验证的互联网出口 IP。</p>
          </dd>
          <dt>下一跳</dt>
          <dd>
            <template v-if="selected.next">
              {{ byKey.get(selected.next.target) ? entryTitle(byKey.get(selected.next.target)!) : (auxByKey.get(selected.next.target)?.label ?? selected.next.target) }}
              <span v-if="selected.next.dial" class="lb-mono">· {{ selected.next.dial }}</span>
              <span v-if="selected.next.addr_mode" class="tv__dim">({{ ADDR_MODE_TEXT[selected.next.addr_mode] }})</span>
            </template>
            <template v-else-if="selected.egress === 'DIRECT'">没有下一跳:本机直连出网(按配置推导)</template>
            <template v-else-if="selected.egress === 'EXTERNAL'">之后的路径由外部服务决定</template>
            <template v-else>没有去向</template>
          </dd>
          <dt>上游</dt>
          <dd>
            <template v-if="upstreamOf.length">
              <button v-for="u in upstreamOf" :key="u.key" type="button" class="lb-linkbtn tv__up" @click="focusOn(u.key)">
                {{ entryTitle(u) }}
              </button>
            </template>
            <template v-else>没有入口把它当下一跳</template>
          </dd>
          <template v-if="selectedPath">
            <dt>完整路径</dt>
            <dd>
              <ol class="tv__path">
                <li v-for="(h, i) in selectedPath.hops" :key="i">
                  {{ h.node_name ? h.node_name + ' / ' : '' }}{{ h.name }}
                  <span class="tv__dim">{{ kindLabel(h.kind) }}{{ h.transparent ? '(只搬字节)' : '' }}</span>
                  <span v-if="h.dial" class="lb-mono tv__dim">← {{ h.dial }}</span>
                  <span v-if="h.problem" class="tv__bad">{{ h.problem }}</span>
                </li>
              </ol>
              <p class="tv__note">{{ selectedPath.egress }}</p>
              <p class="tv__note">链路拨测只验 TCP;UDP 与真实出口 IP 都没有在这里验证过。</p>
            </dd>
          </template>
          <template v-if="selected.note">
            <dt>说明</dt>
            <dd>{{ selected.note }}</dd>
          </template>
          <template v-if="selected.problems.length">
            <dt>问题</dt>
            <dd>
              <p v-for="p in selected.problems" :key="p" class="tv__bad">{{ p }}</p>
            </dd>
          </template>
        </dl>
        <div class="tv__dactions">
          <a-button v-for="it in menuItems.filter((m) => m.key !== 'detail')" :key="it.key" size="small" :disabled="!!it.disabled" :title="it.disabled || ''" @click="menu.key = selected.key; act(it.key, selected)">
            {{ it.label }}
          </a-button>
        </div>
      </template>
    </a-drawer>

    <a-drawer v-model:open="problemsOpen" title="异常" width="420" placement="right" data-testid="topo-problems-drawer">
      <p v-if="!problems.length" class="tv__note">没有发现异常(只按库里已有的配置与状态判断)。</p>
      <ul class="tv__plist">
        <li v-for="(p, i) in problems" :key="i">
          <button type="button" class="tv__pitem" @click="locateProblem(p)">
            <span class="tv__pkind">{{ problemKindText[p.kind] ?? p.kind }}</span>
            <span>{{ p.message }}</span>
          </button>
        </li>
      </ul>
    </a-drawer>

    <InboundChainModal
      v-if="formNode && chainInbound"
      v-model:open="chainOpen"
      :inbound="chainInbound"
      :node="formNode"
      @applied="afterSave"
    />
    <InboundFormModal
      v-if="formNode && chainInbound"
      v-model:open="inboundFormOpen"
      :inbound="chainInbound"
      :node="formNode"
      :tiers="tiersCache"
      :existing-count="formNode.inbounds.length"
      @saved="afterSave"
    />
    <MieruChainModal v-if="formNode && mieruTarget" v-model:open="mieruChainOpen" :inbound="mieruTarget" :node="formNode" @applied="afterSave" />
    <MieruInboundFormModal
      v-if="formNode && mieruTarget"
      v-model:open="mieruFormOpen"
      :inbound="mieruTarget"
      :node="formNode"
      :tiers="tiersCache"
      :existing-count="formNode.mieru_inbounds.length"
      @saved="afterSave"
    />
    <RelayFormModal
      v-if="formNode && relayTarget"
      v-model:open="relayOpen"
      :node-id="formNode.id"
      :node-host="formNode.host"
      :relay="relayTarget"
      :tiers="tiersCache"
      @saved="afterSave"
    />
  </div>
</template>

<style scoped>
.tv {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
  height: calc(100vh - var(--header-h) - var(--page-pad) * 2);
  min-height: 520px;
}
.tv__bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2) var(--sp-3);
}
.tv__title {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}
.tv__title h1 {
  margin: 0;
  font-size: var(--fs-xl);
}
.tv__tag {
  padding: 0 8px;
  border-radius: var(--r-xs);
  background: var(--brand-bg);
  color: var(--brand);
  font-size: var(--fs-2xs);
}
.tv__search {
  position: relative;
  width: 260px;
}
.tv__hits {
  position: absolute;
  z-index: 20;
  top: 100%;
  left: 0;
  right: 0;
  margin: 4px 0 0;
  padding: 4px;
  list-style: none;
  max-height: 320px;
  overflow: auto;
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
  background: var(--surface);
  box-shadow: var(--shadow-lg);
}
.tv__hit {
  display: flex;
  flex-direction: column;
  width: 100%;
  padding: 6px 8px;
  border: 0;
  border-radius: var(--r-sm);
  background: transparent;
  color: var(--text);
  font: inherit;
  font-size: var(--fs-xs);
  text-align: left;
  cursor: pointer;
}
.tv__hit:hover,
.tv__hit:focus-visible {
  background: var(--fill);
  outline: none;
}
.tv__hit span {
  color: var(--text3);
}
.tv__filters {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-1);
}
.tv__f {
  min-width: 96px;
  max-width: 160px;
}
.tv__counts {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.tv__actions {
  margin-left: auto;
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}
.tv__actions .is-warn {
  color: var(--warn);
  border-color: var(--warn-border);
}
.tv__link {
  font-size: var(--fs-xs);
}
.tv__error,
.tv__lerr {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-2) var(--sp-3);
  border-radius: var(--r-group);
  background: var(--bad-bg);
  color: var(--bad);
  font-size: var(--fs-xs);
}
.tv__lerr {
  position: absolute;
  z-index: 5;
  top: 12px;
  left: 12px;
}
.tv__viewport {
  position: relative;
  flex: 1;
  overflow: hidden;
  border: 1px solid var(--sep);
  border-radius: var(--r-card);
  background-color: var(--bg);
  background-image: radial-gradient(var(--sep) 1px, transparent 1px);
  background-size: 22px 22px;
  touch-action: none;
  cursor: grab;
  outline: none;
}
.tv__viewport:focus-visible {
  box-shadow: var(--focus-ring);
}
.tv__viewport:active {
  cursor: grabbing;
}
.tv__empty {
  position: absolute;
  inset: 0;
  display: grid;
  place-items: center;
  color: var(--text3);
  font-size: var(--fs-sm);
}
.tv__world {
  position: absolute;
  left: 0;
  top: 0;
  transform-origin: 0 0;
  transition: transform var(--dur) var(--ease);
}
.tv__viewport:active .tv__world {
  transition: none;
}
.tv__block {
  position: absolute;
  border: 1px dashed var(--sep2);
  border-radius: var(--r-card);
  pointer-events: none;
}
.tv__block.is-iso {
  border-color: var(--sep);
}
.tv__blabel {
  position: absolute;
  top: -10px;
  left: 16px;
  padding: 0 6px;
  background: var(--bg);
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.tv__edges {
  position: absolute;
  left: 0;
  top: 0;
  overflow: visible;
}
.tv__edge {
  cursor: pointer;
}
.tv__hit-path {
  fill: none;
  stroke: transparent;
  stroke-width: 12;
}
.tv__line {
  fill: none;
  stroke: var(--text3);
  stroke-width: 1.6;
  transition:
    stroke var(--dur-fast) var(--ease),
    opacity var(--dur) var(--ease);
}
.tv__arrowhead {
  fill: var(--text3);
}
.tv__edge.is-forward .tv__line {
  stroke-dasharray: 7 4;
}
.tv__edge.st-legacy .tv__line,
.tv__edge.st-unknown_backend .tv__line {
  stroke: var(--warn);
}
.tv__edge.st-broken .tv__line,
.tv__edge.st-loop .tv__line {
  stroke: var(--bad);
  stroke-dasharray: 2 4;
}
.tv__edge.is-on .tv__line {
  stroke: var(--brand);
  stroke-width: 2.4;
  stroke-dasharray: 8 6;
  animation: tv-flow 1.2s linear infinite;
}
.tv__edge.is-dim .tv__line {
  opacity: 0.25;
}
@keyframes tv-flow {
  to {
    stroke-dashoffset: -28;
  }
}
/* 方向光点只表达配置方向;动效关闭、系统减弱动画、页面不可见时都停。 */
:root[data-motion='off'] .tv__edge.is-on .tv__line,
:root[data-motion='reduce'] .tv__edge.is-on .tv__line,
.tv.is-hidden .tv__edge.is-on .tv__line {
  animation: none;
}
@media (prefers-reduced-motion: reduce) {
  .tv__edge.is-on .tv__line {
    animation: none;
  }
  .tv__world {
    transition: none;
  }
}
.tv__elabel {
  position: absolute;
  z-index: 3;
  transform: translate(-50%, -120%);
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 6px 8px;
  border: 1px solid var(--sep);
  border-radius: var(--r-sm);
  background: var(--surface);
  box-shadow: var(--shadow-hover);
  font-size: var(--fs-2xs);
  white-space: nowrap;
  pointer-events: none;
}
.tv__ewarn {
  color: var(--warn);
}
.tv__aux {
  position: absolute;
  box-sizing: border-box;
  display: flex;
  flex-direction: column;
  justify-content: center;
  gap: 2px;
  padding: 8px 12px;
  border: 1.5px dashed var(--warn-border);
  border-radius: var(--r-group);
  background: var(--warn-bg);
  font-size: var(--fs-xs);
}
.tv__aux.is-missing {
  border-color: var(--bad-border);
  background: var(--bad-bg);
}
.tv__auxd {
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.tv__menu {
  position: absolute;
  z-index: 30;
  min-width: 200px;
  padding: 4px;
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
  background: var(--surface);
  box-shadow: var(--shadow-lg);
}
.tv__mhead {
  padding: 6px 8px;
  font-size: var(--fs-2xs);
  color: var(--text3);
  border-bottom: 1px solid var(--sep2);
  margin-bottom: 4px;
}
.tv__mitem {
  display: flex;
  flex-direction: column;
  width: 100%;
  padding: 6px 8px;
  border: 0;
  border-radius: var(--r-sm);
  background: transparent;
  color: var(--text);
  font: inherit;
  font-size: var(--fs-xs);
  text-align: left;
  cursor: pointer;
}
.tv__mitem:hover:not(:disabled),
.tv__mitem:focus-visible {
  background: var(--fill);
  outline: none;
}
.tv__mitem:disabled {
  color: var(--text3);
  cursor: not-allowed;
}
.tv__mwhy {
  font-size: var(--fs-2xs);
}
.tv__tools {
  position: absolute;
  right: 12px;
  top: 12px;
  max-width: calc(100% - 24px);
  flex-wrap: wrap;
  justify-content: flex-end;
  display: flex;
  align-items: center;
  gap: 4px;
  padding: 4px;
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
  background: var(--surface);
  box-shadow: var(--shadow);
}
.tv__zoom {
  min-width: 44px;
  text-align: center;
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.tv__legend {
  position: absolute;
  left: 12px;
  bottom: 12px;
  display: flex;
  flex-wrap: wrap;
  gap: 4px 12px;
  max-width: 60%;
  padding: 6px 10px;
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
  background: var(--surface);
  font-size: var(--fs-2xs);
  color: var(--text2);
}
.tv__lg {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
.lg-proxy,
.lg-fwd,
.lg-warn,
.lg-bad {
  stroke-width: 2;
  stroke: var(--text3);
}
.lg-fwd {
  stroke-dasharray: 7 4;
}
.lg-warn {
  stroke: var(--warn);
}
.lg-bad {
  stroke: var(--bad);
  stroke-dasharray: 2 4;
}
.tv__bar1 {
  display: inline-block;
  width: 4px;
  height: 12px;
  border-radius: 2px;
  background: var(--brand);
}
.tv__mini {
  position: absolute;
  right: 12px;
  bottom: 12px;
  border: 1px solid var(--sep);
  border-radius: var(--r-sm);
  background: var(--surface);
  cursor: pointer;
}
.tv__mini rect {
  fill: var(--sep);
}
.tv__mini rect.is-sel {
  fill: var(--brand);
}
.tv__mini .tv__minivp {
  fill: var(--brand-bg);
  stroke: var(--brand);
  stroke-width: 1;
}
.tv__sel {
  position: absolute;
  left: 50%;
  top: 12px;
  transform: translateX(-50%);
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: 6px 10px;
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
  background: var(--surface);
  box-shadow: var(--shadow);
  font-size: var(--fs-xs);
  max-width: 60%;
}
.tv__perf {
  position: absolute;
  left: 12px;
  top: 18px;
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.tv__dl {
  margin: 0;
}
.tv__dl dt {
  margin-top: var(--sp-3);
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.tv__dl dd {
  margin: 2px 0 0;
  font-size: var(--fs-sm);
}
.tv__eplist,
.tv__path,
.tv__plist {
  margin: 0;
  padding-left: 18px;
}
.tv__eplist li {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.tv__plist {
  padding: 0;
  list-style: none;
}
.tv__pitem {
  display: flex;
  flex-direction: column;
  gap: 2px;
  width: 100%;
  padding: 8px;
  border: 0;
  border-bottom: 1px solid var(--sep2);
  background: transparent;
  color: var(--text);
  font: inherit;
  font-size: var(--fs-xs);
  text-align: left;
  cursor: pointer;
}
.tv__pitem:hover,
.tv__pitem:focus-visible {
  background: var(--fill);
  outline: none;
}
.tv__pkind {
  font-size: var(--fs-2xs);
  color: var(--warn);
}
.tv__dim {
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.tv__bad {
  margin: 2px 0;
  font-size: var(--fs-xs);
  color: var(--bad);
}
.tv__note {
  margin: 4px 0 0;
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.tv__up {
  display: block;
}
.tv__dactions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-2);
  margin-top: var(--sp-4);
}
@media (max-width: 767px) {
  .tv {
    height: auto;
  }
  .tv__viewport {
    min-height: 70vh;
  }
  .tv__search {
    width: 100%;
  }
  .tv__legend {
    max-width: calc(100% - 24px);
  }
  .tv__mini,
  .tv__perf,
  .tv__opt {
    display: none;
  }
  .tv__sel {
    top: auto;
    bottom: 12px;
    max-width: calc(100% - 24px);
  }
}
</style>
