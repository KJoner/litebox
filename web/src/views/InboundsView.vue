<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { message, Modal } from 'ant-design-vue'
import {
  api,
  ApiError,
  PROTOCOL_LABEL,
  type AccessTier,
  type DeployResult,
  type DeploymentRecord,
  type ExternalProxy,
  type MieruInbound,
  type Node,
  type NodeInbound,
  type NodeRelay,
  type OrderScheme,
} from '@/api/client'
import DeployStepList from '@/components/DeployStepList.vue'
import { LbEmptyState, LbRowCard, LbStatusTag, configStatusMeta, lbDangerConfirm,
  LbInfoTip,
  LbSectionTitle,
  LbIcon,
} from '@/components/lb'
import type { LbStatusMeta } from '@/components/lb/statusMeta'
import { configState, needsDeploy } from '@/components/lb/derive'
import InboundChainModal from '@/components/node/InboundChainModal.vue'
import InboundDestModal from '@/components/node/InboundDestModal.vue'
import InboundFormModal from '@/components/node/InboundFormModal.vue'
import EntrySortModal, { type SortTarget } from '@/components/node/EntrySortModal.vue'
import { globalValue, sortByEntryOrder, type EntryOrderKey } from '@/components/node/entryOrder'
import MieruChainModal from '@/components/node/MieruChainModal.vue'
import MieruInboundFormModal from '@/components/node/MieruInboundFormModal.vue'
import ExternalProxyModal from '@/components/external/ExternalProxyModal.vue'
import ExpiryModal from '@/components/expiry/ExpiryModal.vue'
import { expiryStatusMeta } from '@/components/expiry/expiryMeta'
import {
  addressFamilyMeta,
  confirmRemoveInbound,
  confirmRemoveMieruInbound,
  inboundEnabledMeta,
  inboundHasIPv6Entry,
  inboundProtocolMeta,
  portText,
} from '@/components/node/inboundOps'
import { confirmDeployNode, confirmRestartNode, nodeLabel } from '@/components/node/nodeOps'
import { useNarrow } from '@/composables/useNarrow'

/**
 * 入口管理:把**能出现在用户订阅里的每一条线路**摊平成一张表(V20)。
 *
 * 为什么要有这一页 —— 机器多起来之后,「这套系统一共对外开了哪些口子」
 * 这个问题只能靠一台台点进节点详情去拼,而它恰恰是最常被问到的:
 * 排查一条线路、核对某个等级放出去了几个入口、找出哪些还没部署,
 * 全都是**横着看**的问题,而节点详情是竖着切的。
 *
 * V20 之前这里只有 sing-box 与 Mieru 入口;nginx / realm 转发线路与外部代理
 * 同样会进订阅,漏掉它们的话「统一入口」仍然少一截 —— 排序尤其如此:
 * 订阅里的先后是全部四类一起算的(见 entryOrder.ts),只看其中两类
 * 看不出一条外部代理会插在哪里。
 *
 * **列表统一不等于部署方式统一。** 操作按类型分派:sing-box 整台下发、
 * Mieru 逐入口下发、nginx 只 reload、realm 要 restart、外部代理根本没有
 * 「下发」这件事 —— 它们只有编辑、检查、订阅开关、排序与续费。
 * 外部代理的源、同步、导入仍在「外部代理」页,两处操作的是同一份记录。
 *
 * **数据不复制**:自建入口从 GET /api/nodes 带出的 Node.inbounds /
 * mieru_inbounds 来,转发从 GET /api/relays,外部代理从 GET /api/external-proxies
 * —— 都是各自页面在用的同一个接口。
 */

const router = useRouter()
const narrow = useNarrow()

const nodes = ref<Node[]>([])
const relays = ref<NodeRelay[]>([])
const externals = ref<ExternalProxy[]>([])
const tiers = ref<AccessTier[]>([])
const scheme = ref<OrderScheme>('LEGACY')
const externalPosition = ref<'BEFORE' | 'AFTER'>('AFTER')
const loading = ref(false)
const loadError = ref('')
/** 列级失败:转发或外部代理读不到时其余行照常显示,但要说出来。 */
const partialError = ref('')
const running = ref('')

/**
 * 一行 = 一条线路 + 它所属的东西。自建的带着机器(操作都落在机器上),
 * 外部代理带着它自己。
 *
 * 四类在同一张表里,因为这一页要回答的是同一个问题:
 * 「这套系统一共对外开了哪些口子、它们在订阅里是什么顺序」。
 * 但它们的下发方式差得很远,所以操作按 kind 分支,确认文案也各走各的。
 */
type SingBoxRow = { key: string; kind: 'singbox'; inbound: NodeInbound; node: Node }
type MieruRow = { key: string; kind: 'mieru'; mieru: MieruInbound; node: Node }
type RelayRow = { key: string; kind: 'nginx' | 'realm'; relay: NodeRelay; node: Node }
type ExternalRow = { key: string; kind: 'external'; proxy: ExternalProxy }
type Row = SingBoxRow | MieruRow | RelayRow | ExternalRow

/** 排序键:与后端 subscription.EntryOrder 同一套字段。 */
function orderKey(r: Row): EntryOrderKey {
  switch (r.kind) {
    case 'singbox':
      return { nodeSort: r.node.sort_order, nodeId: r.node.id, sort: r.inbound.sort_order, kind: 'singbox', id: r.inbound.id }
    case 'mieru':
      return { nodeSort: r.node.sort_order, nodeId: r.node.id, sort: r.mieru.sort_order, kind: 'mieru', id: r.mieru.id }
    case 'nginx':
    case 'realm':
      return { nodeSort: r.node.sort_order, nodeId: r.node.id, sort: r.relay.sort_order, kind: r.kind, id: r.relay.id }
    default:
      return { nodeSort: 0, nodeId: 0, sort: r.proxy.sort_order, kind: 'external', id: r.proxy.id }
  }
}

// **四类按订阅里的规则一起排**,判据与后端订阅那一侧一致(见 entryOrder.ts)。
// 这一屏跨机器,所以机器那两个键必须给真值 —— 只按 sort_order 排的话,
// 两台机器的 0 号入口会交错在一起,而管理员是按机器分配那个数字的。
const rows = computed<Row[]>(() => {
  const nodeByID = new Map(nodes.value.map((n) => [n.id, n]))
  const list: Row[] = nodes.value.flatMap((n) => [
    ...(n.inbounds ?? []).map(
      (i): Row => ({ key: `${n.id}-i${i.id}`, kind: 'singbox', inbound: i, node: n }),
    ),
    ...(n.mieru_inbounds ?? []).map(
      (m): Row => ({ key: `${n.id}-m${m.id}`, kind: 'mieru', mieru: m, node: n }),
    ),
  ])
  for (const r of relays.value) {
    const n = nodeByID.get(r.node_id)
    // 机器读不到(刚被删)就不显示这条:它已经不进订阅了。
    if (!n) continue
    list.push({ key: `${n.id}-r${r.id}`, kind: r.engine === 'REALM' ? 'realm' : 'nginx', relay: r, node: n })
  }
  for (const p of externals.value) {
    list.push({ key: `x${p.id}`, kind: 'external', proxy: p })
  }
  return sortByEntryOrder<Row>(list, orderKey, scheme.value, externalPosition.value)
})

// ---------- 四类的取值差异都收在这里 ----------
//
// 模板里逐处 `r.kind === 'singbox' ? ... : ...` 的话,加一列就要在模板里
// 再写一次分支,而漏掉一处的表现是那一列对某一类行显示空白 ——
// 看起来像"这个入口没配这一项"。

const kindLabel: Record<Row['kind'], string> = {
  singbox: '自建 · sing-box',
  mieru: '自建 · Mieru',
  nginx: '自建 · nginx 转发',
  realm: '自建 · realm 转发',
  external: '外部代理',
}
const isSelf = (r: Row): r is SingBoxRow | MieruRow | RelayRow => r.kind !== 'external'
const isRelay = (r: Row): r is RelayRow => r.kind === 'nginx' || r.kind === 'realm'

function rowName(r: Row): string {
  switch (r.kind) {
    case 'singbox':
      return r.inbound.display_name
    case 'mieru':
      return r.mieru.display_name
    case 'nginx':
    case 'realm':
      return r.relay.display_name
    default:
      return r.proxy.final_display_name || r.proxy.display_name
  }
}
/** 第二行小字:sing-box 的 tag、Mieru 的类型说明、转发的落地、外部代理的来源。 */
function rowSub(r: Row): string {
  switch (r.kind) {
    case 'singbox':
      return r.inbound.tag
    case 'mieru':
      return 'Mieru · mita 实例'
    case 'nginx':
    case 'realm':
      return r.relay.target_kind === 'ADDRESS'
        ? `指定地址 ${r.relay.target_host}:${r.relay.target_port}(不进订阅)`
        : `落地 ${r.relay.target_name || `${r.relay.target_host}:${r.relay.target_port}`}`
    default:
      return r.proxy.source_name ? `来源 ${r.proxy.source_name}` : '手工条目'
  }
}
/** 所属:自建行是机器,外部代理是订阅源(或手工)。 */
function rowOwner(r: Row): string {
  return isSelf(r) ? nodeLabel(r.node) : r.proxy.source_name || '手工条目'
}
function rowTier(r: Row): string {
  switch (r.kind) {
    case 'singbox':
      return r.inbound.access_tier_name
    case 'mieru':
      return r.mieru.access_tier_name
    case 'nginx':
    case 'realm':
      return r.relay.access_tier_name
    default:
      return r.proxy.access_tier_name
  }
}
function rowEnabled(r: Row): boolean {
  switch (r.kind) {
    case 'singbox':
      return r.inbound.enabled
    case 'mieru':
      return r.mieru.enabled
    case 'nginx':
    case 'realm':
      return r.relay.enabled
    default:
      return r.proxy.status === 'ACTIVE'
  }
}
function rowInSub(r: Row): boolean {
  switch (r.kind) {
    case 'singbox':
      return r.inbound.subscription_enabled
    case 'mieru':
      return r.mieru.subscription_enabled
    case 'nginx':
    case 'realm':
      return r.relay.target_kind !== 'ADDRESS' && r.relay.subscription_enabled
    default:
      return r.proxy.subscription_enabled
  }
}
/** 这一行在订阅里会不会多出一条 IPv6 条目。转发按机器有没有 IPv6 展开;外部代理没有。 */
function rowHasIPv6(r: Row): boolean {
  switch (r.kind) {
    case 'singbox':
      return inboundHasIPv6Entry(r.inbound, r.node)
    case 'mieru':
      return !!r.node.ipv6_address && r.mieru.ipv6_enabled
    case 'nginx':
    case 'realm':
      return !!r.node.ipv6_address
    default:
      return false
  }
}

/** Mieru 行的「待下发」标记。颜色取 tokens 里的 warning 那一组。 */
const mieruPendingMeta: LbStatusMeta = {
  text: '待下发',
  shape: 'dot',
  fg: 'var(--warn)',
  bg: 'var(--warn-bg)',
}

/** 已经上过节点没有。转发与外部代理没有这个概念,按「已就绪」看。 */
function rowDeployed(r: Row): boolean {
  switch (r.kind) {
    case 'singbox':
      return !!r.inbound.deployed_protocol
    case 'mieru':
      return !!r.mieru.deployed_transport
    default:
      return true
  }
}

function rowPortText(r: Row): string {
  switch (r.kind) {
    case 'singbox':
      return portText(r.inbound.listen_port, r.inbound.public_port)
    case 'mieru': {
      const m = r.mieru
      const range = m.listen_port_start === m.listen_port_end
        ? String(m.listen_port_start)
        : `${m.listen_port_start}-${m.listen_port_end}`
      const pub = m.public_port_start
        ? m.public_port_start === m.public_port_end
          ? String(m.public_port_start)
          : `${m.public_port_start}-${m.public_port_end}`
        : ''
      return pub && pub !== range ? `${range} → 公网 ${pub}` : range
    }
    case 'nginx':
    case 'realm':
      return portText(r.relay.listen_port, r.relay.public_port)
    default:
      return `${r.proxy.server}:${r.proxy.port}`
  }
}

/** 协议标记。自建两类与原来一样;转发写引擎,外部代理写协议名。 */
function rowProtocolMeta(r: Row): LbStatusMeta {
  switch (r.kind) {
    case 'singbox':
      return inboundProtocolMeta(r.inbound)
    case 'mieru': {
      const m = r.mieru
      const pending = m.deployed_transport && m.deployed_transport !== m.transport
      return {
        text: pending ? `${m.deployed_transport} → ${m.transport} 待下发` : `Mieru ${m.transport}`,
        shape: 'dot',
        fg: pending ? 'var(--warn)' : 'var(--brand)',
        bg: pending ? 'var(--warn-bg)' : 'var(--brand-bg)',
      }
    }
    case 'nginx':
      return { text: 'nginx 透传', shape: 'dot', fg: 'var(--text2)', bg: 'var(--fill)' }
    case 'realm':
      return { text: 'realm 透传', shape: 'dot', fg: 'var(--text2)', bg: 'var(--fill)' }
    default:
      return { text: r.proxy.protocol, shape: 'dot', fg: 'var(--purple)', bg: 'var(--purple-bg)' }
  }
}

/** 旧方案下的位置说明;GLOBAL 方案下直接给数。 */
function globalText(r: Row): string {
  const k = orderKey(r)
  if (scheme.value === 'GLOBAL') return String(globalValue(k))
  if (r.kind === 'external') return `${k.sort}(整块在${externalPosition.value === 'BEFORE' ? '前' : '后'})`
  return `机器 ${k.nodeSort} · 入口 ${k.sort}`
}

function sortTarget(r: Row): SortTarget {
  const k = orderKey(r)
  const kind = (
    { singbox: 'SINGBOX', mieru: 'MIERU', nginx: 'NGINX', realm: 'REALM', external: 'EXTERNAL' } as const
  )[r.kind]
  if (r.kind === 'external') return { kind, id: r.proxy.id, name: rowName(r), sort: k.sort }
  return {
    kind, id: k.id, name: rowName(r), sort: k.sort,
    nodeId: r.node.id, nodeName: nodeLabel(r.node), nodeSort: r.node.sort_order,
  }
}

// ---------------------------------------------------------------- 筛选

type TypeFilter = 'ALL' | 'SINGBOX' | 'MIERU' | 'RELAY' | 'EXTERNAL'
const kw = ref('')
const filterType = ref<TypeFilter>('ALL')
const filterNode = ref<number | undefined>(undefined)
const filterProtocol = ref<string | undefined>(undefined)
const filterTier = ref<number | undefined>(undefined)
const filterExit = ref<'ALL' | 'DIRECT' | 'CHAIN'>('ALL')
const onlyPending = ref(false)

/** 协议筛选:Mieru 用 'MIERU' 这个取值;转发按引擎;外部代理按它的协议名。 */
function rowProtocol(r: Row): string {
  switch (r.kind) {
    case 'singbox':
      return r.inbound.protocol
    case 'mieru':
      return 'MIERU'
    case 'nginx':
      return 'NGINX'
    case 'realm':
      return 'REALM'
    default:
      return r.proxy.protocol
  }
}
function rowTierID(r: Row): number {
  switch (r.kind) {
    case 'singbox':
      return r.inbound.access_tier_id
    case 'mieru':
      return r.mieru.access_tier_id
    case 'nginx':
    case 'realm':
      return r.relay.access_tier_id
    default:
      return r.proxy.access_tier_id
  }
}
function rowChained(r: Row): boolean {
  if (r.kind === 'singbox') return !!r.inbound.chain_target_kind
  if (r.kind === 'mieru') return !!r.mieru.chain_target_kind
  return false
}
function matchesType(r: Row): boolean {
  switch (filterType.value) {
    case 'SINGBOX':
      return r.kind === 'singbox'
    case 'MIERU':
      return r.kind === 'mieru'
    case 'RELAY':
      return isRelay(r)
    case 'EXTERNAL':
      return r.kind === 'external'
    default:
      return true
  }
}

const filtered = computed(() =>
  rows.value.filter((r) => {
    if (!matchesType(r)) return false
    if (filterNode.value && (!isSelf(r) || r.node.id !== filterNode.value)) return false
    if (filterProtocol.value && rowProtocol(r) !== filterProtocol.value) return false
    if (filterTier.value && rowTierID(r) !== filterTier.value) return false
    if (filterExit.value === 'DIRECT' && rowChained(r)) return false
    if (filterExit.value === 'CHAIN' && !rowChained(r)) return false
    // 「只看待部署」= 这个入口自己还没上过节点,或者机器整体有未下发的变更。
    if (onlyPending.value && (!isSelf(r) || (rowDeployed(r) && !needsDeploy(r.node)))) return false
    const q = kw.value.trim().toLowerCase()
    if (!q) return true
    return [rowName(r), rowSub(r), rowOwner(r), isSelf(r) ? r.node.host : r.proxy.server, rowPortText(r)]
      .join(' ')
      .toLowerCase()
      .includes(q)
  }),
)

/**
 * 协议筛选的选项。**Mieru、两种转发引擎与外部代理的协议都要在里面** ——
 * 漏掉的话这一页会出现"筛选之后总数对不上"而看不出为什么。
 */
const protocolOptions = computed(() => {
  const seen = new Set<string>()
  const out: { value: string; label: string }[] = []
  for (const [v, l] of Object.entries(PROTOCOL_LABEL)) {
    seen.add(v)
    out.push({ value: v, label: l })
  }
  out.push({ value: 'MIERU', label: 'Mieru' }, { value: 'NGINX', label: 'nginx 透传' }, { value: 'REALM', label: 'realm 透传' })
  seen.add('MIERU').add('NGINX').add('REALM')
  for (const p of externals.value) {
    if (!seen.has(p.protocol)) {
      seen.add(p.protocol)
      out.push({ value: p.protocol, label: `${p.protocol}(外部)` })
    }
  }
  return out
})

const nodeOptions = computed(() =>
  nodes.value
    .filter((n) => n.role !== 'RELAY')
    .map((n) => ({ value: n.id, label: nodeLabel(n) })),
)
const nodeFilterOptions = computed(() => nodes.value.map((n) => ({ value: n.id, label: nodeLabel(n) })))

/** 摘要放在表格上方:这一页存在的理由就是一眼看清全局。 */
const summary = computed(() => {
  const all = rows.value
  return {
    total: all.length,
    self: all.filter(isSelf).length,
    external: all.filter((r) => r.kind === 'external').length,
    relays: all.filter(isRelay).length,
    nodes: new Set(all.filter(isSelf).map((r) => (r as SingBoxRow).node.id)).size,
    chained: all.filter(rowChained).length,
    pending: all.filter((r) => isSelf(r) && (!rowDeployed(r) || needsDeploy(r.node))).length,
    disabled: all.filter((r) => !rowEnabled(r)).length,
  }
})

async function load() {
  loading.value = true
  loadError.value = ''
  partialError.value = ''
  try {
    const [ns, ts] = await Promise.all([api.nodes(), api.accessTiers()])
    nodes.value = ns.items ?? []
    tiers.value = ts.items ?? []
  } catch (e) {
    // 读不到时表格保持空白,不显示「暂无数据」—— 那会被读成「一个入口都没有」,
    // 而那正是管理员打开这一页最不该被骗到的地方。
    nodes.value = []
    loadError.value = e instanceof ApiError ? e.message : '读取入口失败'
    loading.value = false
    return
  }
  // 转发、外部代理与排序方案各自降级:读不到就少一类,但要说出来 ——
  // 一整类静默消失看起来像"没有配过"。
  const missing: string[] = []
  const [rs, xs, st] = await Promise.allSettled([api.relays(), api.externalProxies(), api.settings()])
  if (rs.status === 'fulfilled') relays.value = rs.value.items ?? []
  else { relays.value = []; missing.push('转发线路') }
  if (xs.status === 'fulfilled') externals.value = xs.value.items ?? []
  else { externals.value = []; missing.push('外部代理') }
  if (st.status === 'fulfilled') {
    scheme.value = st.value.subscription_order_scheme ?? 'LEGACY'
    externalPosition.value = st.value.subscription_external_position ?? 'AFTER'
  } else {
    missing.push('排序方案(按旧方案显示)')
  }
  if (missing.length) partialError.value = `${missing.join('、')}暂时读不到,其余行正常`
  loading.value = false
}

onMounted(load)

// ---------------------------------------------------------------- 显示

/** 出口去向。名字只在这一页解析不了(链式落地可能在另一台机器上),
 *  所以按 id 在全量入口里找 —— 数据本来就都在手上。 */
function exitText(r: Row): string {
  if (isRelay(r)) {
    return r.relay.target_kind === 'ADDRESS'
      ? `透传到 ${r.relay.target_host}:${r.relay.target_port}`
      : `透传到 ${r.relay.target_name || '落地'}`
  }
  if (r.kind === 'external') return '成品线路,出口由对方决定'
  const kind = r.kind === 'singbox' ? r.inbound.chain_target_kind : r.mieru.chain_target_kind
  const inboundID =
    r.kind === 'singbox' ? r.inbound.chain_target_inbound_id : r.mieru.chain_target_inbound_id
  const externalID =
    r.kind === 'singbox' ? r.inbound.chain_target_external_id : r.mieru.chain_target_external_id
  // Mieru 的出口要经本机 sing-box 转一跳(mita 的出口代理只认 SOCKS5)。
  // 那一跳必须说出来:不说的话,管理员不明白改个 Mieru 出口为什么
  // 还要重启 sing-box,而那个问题没有答案就只能被读成面板有 bug。
  const via = r.kind === 'mieru' ? '经本机 sing-box → ' : '经 '
  if (kind === 'INBOUND') {
    const hit = rows.value.find((x) => x.kind === 'singbox' && x.inbound.id === inboundID)
    return hit ? `${via}${nodeLabel((hit as SingBoxRow).node)} / ${rowName(hit)}` : `${via}入口 #${inboundID}`
  }
  if (kind === 'EXTERNAL') {
    const hit = externals.value.find((p) => p.id === externalID)
    return hit ? `${via}外部代理 ${hit.final_display_name || hit.display_name}` : `${via}外部代理 #${externalID}`
  }
  return '本机直连'
}

function goNode(r: Row) {
  if (!isSelf(r)) return
  router.push(`/nodes/${r.node.id}/entries`)
}
function goExternal() {
  router.push('/external-proxies')
}

// ---------------------------------------------------------------- 弹窗

const formOpen = ref(false)
const chainOpen = ref(false)
const destOpen = ref(false)
// **弹窗的 target 只装 sing-box 行。** Mieru 有自己的一套弹窗
// (MieruInboundFormModal / MieruChainModal),它们收的是 MieruInbound。
// 合成一个 target 会让每个弹窗都要先判一次"这是哪一类",
// 而判漏的表现是把一个 Mieru 入口的 id 传给了 sing-box 的接口。
const target = ref<SingBoxRow | null>(null)
const mieruTarget = ref<MieruRow | null>(null)
/** 新增与编辑共用一个 target,靠这个标记区分 —— 表单组件收 null 表示新增。 */
const creating = ref(false)

/** 弹窗都要一个 node —— 新增时也是。没有选中行时给一个占位不安全:
 *  那会让「新增」落到一台管理员没看的机器上。所以只在有 target 时渲染。 */
const targetNode = computed(() => target.value?.node ?? null)

/**
 * 新增入口必须先挑机器。
 *
 * 不给一个"默认机器" —— 那会让新增落到管理员没在看的那一台上,
 * 而下一次部署会重启它、踢掉上面全部入口的在线连接。
 */
function openCreate(n: Node) {
  target.value = {
    key: `new-${n.id}`,
    kind: 'singbox',
    inbound: null as unknown as NodeInbound,
    node: n,
  }
  creating.value = true
  formOpen.value = true
}

function openEdit(r: Row) {
  if (r.kind === 'mieru') {
    mieruTarget.value = r
    mieruFormOpen.value = true
    return
  }
  if (r.kind === 'external') {
    externalTarget.value = r.proxy
    externalFormOpen.value = true
    return
  }
  if (isRelay(r)) {
    // 转发规则的表单在节点详情的「入口与转发」里(它要先知道这台机器上
    // 已有哪些端口与落地),这里只跳过去。
    goNode(r)
    return
  }
  creating.value = false
  target.value = r
  formOpen.value = true
}
function openChain(r: SingBoxRow | MieruRow) {
  if (r.kind === 'mieru') {
    mieruTarget.value = r
    mieruChainOpen.value = true
    return
  }
  creating.value = false
  target.value = r
  chainOpen.value = true
}
function openDest(r: SingBoxRow) {
  creating.value = false
  target.value = r
  destOpen.value = true
}
function remove(r: SingBoxRow | MieruRow) {
  if (r.kind === 'mieru') {
    confirmRemoveMieruInbound(
      r.mieru,
      nodeLabel(r.node),
      (fn: () => Promise<void>) => {
        running.value = '正在删除入口'
        void fn().finally(() => (running.value = ''))
      },
      load,
    )
    return
  }
  confirmRemoveInbound(
    r.inbound,
    nodeLabel(r.node),
    (fn) => {
      running.value = '正在删除入口'
      void fn().finally(() => (running.value = ''))
    },
    load,
  )
}

const mieruFormOpen = ref(false)
const mieruChainOpen = ref(false)

// ---------- 外部代理:只给适用的几样,不显示「安装 sing-box」「重启节点」 ----------

const externalFormOpen = ref(false)
const externalTarget = ref<ExternalProxy | null>(null)
const expiryTarget = ref<ExternalProxy | null>(null)

async function checkExternal(p: ExternalProxy) {
  running.value = '测试连通性'
  try {
    const r = await api.checkExternalProxy(p.id)
    Modal[r.ok ? 'info' : 'warning']({
      title: r.ok ? '端口可达' : '连接失败',
      width: 480,
      content: `${r.message}\n\n${r.disclaimer}`,
      okText: '知道了',
    })
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '连通性检查失败')
  } finally {
    running.value = ''
  }
}

function toggleExternalSub(p: ExternalProxy) {
  const next = !p.subscription_enabled
  running.value = next ? '恢复下发' : '停发订阅'
  api
    .setExternalProxySubscription(p.id, next)
    .then(() => message.success(next ? '已恢复下发,用户下次更新订阅即可看到' : '已从订阅下架'))
    .catch((e) => message.error(e instanceof ApiError ? e.message : '操作失败'))
    .finally(() => {
      running.value = ''
      load()
    })
}

// ---------- 排序 ----------

const sortOpen = ref(false)
const sortTargetRow = ref<SortTarget | null>(null)
function openSort(r: Row) {
  sortTargetRow.value = sortTarget(r)
  sortOpen.value = true
}

/**
 * 下发一个 Mieru 入口。
 *
 * **逐入口,不是整台机器。** 一个入口一个 mita 实例,重启一个不影响另一个
 * —— 这一点与 sing-box 那一侧正好相反,所以确认文案必须分开写。
 */
function deployMieru(r: MieruRow) {
  lbDangerConfirm({
    title: `确认下发 Mieru 入口「${r.mieru.display_name}」?`,
    okType: 'primary',
    okText: '开始下发',
    impacts: [
      `会重启 ${nodeLabel(r.node)} 上这一个 mita 实例,把**这个入口**的在线连接全部踢掉。`,
      '同机的其他 Mieru 入口与 sing-box 入口一条连接都不断 —— 它们是各自独立的进程。',
      '下发前会先同步这个实例的流量:计数器随进程消失,不先同步的话那一段永久丢失。',
    ],
    footer: '要看每一步的结果,去这台机器的「入口与转发」Tab 下发 —— 那里有完整的进度弹窗。',
    onOk: () => {
      running.value = '正在下发 Mieru 入口'
      void api
        .deployMieruInbound(r.mieru.id)
        .then((res) => {
          if (res.error) message.error(res.error)
          else message.success('Mieru 入口已下发')
        })
        .catch((e) => message.error(e instanceof ApiError ? e.message : '下发失败'))
        .finally(() => {
          running.value = ''
          load()
        })
    },
  })
}

/**
 * 下发这台机器的转发。nginx 只 reload(在途连接一条不断),realm 要 restart
 * (在途连接全断)—— 两种引擎的确认档次不同,不能共用一句。
 */
function deployRelays(r: RelayRow) {
  const n = r.node
  if (r.kind === 'realm') {
    lbDangerConfirm({
      title: `下发 ${nodeLabel(n)} 的 realm 转发?`,
      okText: '下发',
      okType: 'primary',
      impacts: [
        'realm 没有 reload:每次下发都是 restart,这台机器上经 realm 的在途连接全部断开',
        '下发后做服务状态、逐端口监听与真实拨测三步检查,不通过自动回滚',
      ],
      onOk: () => {
        void runRelayDeploy(() => api.deployRealm(n.id), 'realm 转发已下发')
      },
    })
    return
  }
  Modal.confirm({
    title: `下发 ${nodeLabel(n)} 的 nginx 转发?`,
    content: '只 reload nginx,在途连接一条不断;下发后做服务状态、逐端口监听与真实拨测三步检查。',
    okText: '下发',
    onOk: () => {
      void runRelayDeploy(() => api.deployRelays(n.id), 'nginx 转发已下发')
    },
  })
}

async function runRelayDeploy(
  fn: () => Promise<{ result: DeployResult; error?: string }>,
  ok: string,
) {
  running.value = '正在下发转发'
  try {
    const res = await fn()
    if (res.error) message.error(res.error)
    else if (res.result.status === 'SUCCESS') message.success(ok)
    else message.error(res.result.error_message || '下发未成功,详情见部署记录')
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '下发失败')
  } finally {
    running.value = ''
    load()
  }
}

// ---------------------------------------------------------------- 机器级操作

const deployOpen = ref(false)
const deployRunning = ref(false)
const deployResult = ref<DeployResult | null>(null)
const deployNode = ref<Node | null>(null)

function doDeploy(n: Node) {
  confirmDeployNode(n, () => {
    void runDeploy(n)
  })
}

async function runDeploy(n: Node) {
  deployNode.value = n
  deployResult.value = null
  deployOpen.value = true
  deployRunning.value = true
  try {
    deployResult.value = await api.deployNode(n.id)
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '部署失败')
    deployOpen.value = false
  } finally {
    deployRunning.value = false
    load()
  }
}

/** 把部署结果拼成 DeployStepList 认的形状,复用节点详情里那条时间线。 */
const deployAsRecord = computed<DeploymentRecord | null>(() =>
  deployResult.value
    ? {
        id: 0,
        node_id: deployResult.value.node_id,
        revision: deployResult.value.revision,
        config_sha256: deployResult.value.config_sha256,
        status: deployResult.value.status,
        started_at: deployResult.value.started_at,
        finished_at: deployResult.value.finished_at,
        error_message: deployResult.value.error_message ?? '',
        rollback_result: deployResult.value.rollback_result ?? '',
        steps: deployResult.value.steps ?? [],
      }
    : null,
)

function doRestart(n: Node) {
  confirmRestartNode(n, () => {
    running.value = '正在重启'
    api
      .restartNode(n.id)
      .then(() => message.success('已重启'))
      .catch((e) => message.error(e instanceof ApiError ? e.message : '重启失败'))
      .finally(() => {
        running.value = ''
        load()
      })
  })
}

function createOn(nodeID: number) {
  const n = nodes.value.find((x) => x.id === nodeID)
  if (n) openCreate(n)
}

// 列宽之和压在 1180 以内,不固定任何列:右侧固定的「操作」列在表格超宽时会盖住
// 它左边那几列(V18 那条规矩),而这一页左边恰恰是「出口去向」与「全局排序」。
// 协议标记并进「入口」格、访问等级并进「出口去向」格 —— 每格两行,列数少两列。
const columns = [
  { title: '类型', key: 'kind', width: 100 },
  { title: '入口', key: 'name', width: 230 },
  { title: '所属', key: 'owner', width: 150 },
  { title: '端口 / 地址', key: 'port', width: 130 },
  { title: '出口去向 · 等级', key: 'exit', width: 170 },
  { title: '全局排序', key: 'order', width: 90 },
  { title: '状态', key: 'state', width: 140 },
  { title: '操作', key: 'ops', width: 190 },
]
</script>

<template>
  <div class="lb-page iv">
    <div class="lb-page__head">
      <div class="lb-page__title-wrap">
        <h1 class="lb-page__title">
          <span>入口管理</span>
          <LbInfoTip :width="360">
            能出现在用户订阅里的每一条线路:自建的 sing-box 与 Mieru 入口、nginx / realm 转发,以及外部代理。
            顺序就是订阅里的顺序(与后端同一套判据)。
            <b>列表统一不等于部署方式统一</b>:sing-box 整台一次下发,Mieru 逐入口,nginx 只 reload,realm 要 restart;外部代理没有「下发」这回事,只有编辑、检查、订阅开关、排序与续费,它的源与同步仍在「外部代理」页。
          </LbInfoTip>
        </h1>
        <div v-if="!loading && !loadError" class="lb-page__summary">
          {{ summary.total }} 条线路:自建 {{ summary.self }}(分布在 {{ summary.nodes }} 台机器<template v-if="summary.relays">,含 {{ summary.relays }} 条转发</template>),外部代理 {{ summary.external }}。<template v-if="summary.chained">{{ summary.chained }} 个走链式出口,</template><template v-if="summary.disabled">{{ summary.disabled }} 个已停用,</template><span v-if="summary.pending" class="iv__pending">{{ summary.pending }} 个待部署。</span>
          <span class="iv__scheme">排序方案:{{ scheme === 'GLOBAL' ? '全局排序' : '旧方案(先机器再入口,外部代理整块在' + (externalPosition === 'BEFORE' ? '前' : '后') + ')' }}</span>
        </div>
      </div>
      <div class="lb-page__actions">
        <!-- 新增 Mieru 入口不放在这里:它的表单要先知道这台机器上已有几个
             Mieru 入口(端口段冲突检测要用),而那要先挑机器。
             走节点详情的「入口与转发」Tab —— 那里三类的按钮各占一行,
             而且离要改的东西最近。外部代理在它自己的页面上新增。 -->
        <a-dropdown :disabled="!nodeOptions.length">
          <a-button type="primary">新增 sing-box 入口</a-button>
          <template #overlay>
            <a-menu>
              <a-menu-item v-for="o in nodeOptions" :key="o.value" @click="createOn(o.value)">
                加到 {{ o.label }}
              </a-menu-item>
            </a-menu>
          </template>
        </a-dropdown>
        <a-button :loading="loading" @click="load">刷新</a-button>
      </div>
    </div>

    <a-alert v-if="partialError" type="warning" show-icon :message="partialError" />

    <section>
    <LbSectionTitle title="全部线路" :count="`${filtered.length} / ${rows.length} 条`" />
    <div class="lb-card lb-card--flush">
    <div class="lb-filter iv__filters">
      <a-input v-model:value="kw" placeholder="搜名称 / tag / 机器 / 来源 / 端口" allow-clear>
        <template #prefix><LbIcon name="search" :size="14" /></template>
      </a-input>
      <a-select v-model:value="filterType" style="width: 130px">
        <a-select-option value="ALL">全部类型</a-select-option>
        <a-select-option value="SINGBOX">sing-box</a-select-option>
        <a-select-option value="MIERU">Mieru</a-select-option>
        <a-select-option value="RELAY">转发</a-select-option>
        <a-select-option value="EXTERNAL">外部代理</a-select-option>
      </a-select>
      <a-select
        v-model:value="filterNode"
        placeholder="机器"
        allow-clear
        style="width: 170px"
        :options="nodeFilterOptions"
      />
      <a-select
        v-model:value="filterProtocol"
        placeholder="协议"
        allow-clear
        style="width: 150px"
        :options="protocolOptions"
      />
      <a-select
        v-model:value="filterTier"
        placeholder="访问等级"
        allow-clear
        style="width: 140px"
        :options="tiers.map((t) => ({ value: t.id, label: t.name }))"
      />
      <a-radio-group v-model:value="filterExit" size="small" button-style="solid">
        <a-radio-button value="ALL">全部出口</a-radio-button>
        <a-radio-button value="DIRECT">本机直连</a-radio-button>
        <a-radio-button value="CHAIN">链式</a-radio-button>
      </a-radio-group>
      <label class="lb-filter__toggle" :class="{ 'lb-filter__toggle--on': onlyPending }">
        <a-switch v-model:checked="onlyPending" size="small" />
        只看待部署
      </label>
    </div>

    <div v-if="loadError" class="lb-error-strip iv__error">{{ loadError }}</div>

    <!-- 窄屏整表换卡片:AntD 的横向滚动会把最右边的「操作」列推出屏幕。 -->
    <div v-if="narrow" class="iv__cards">
      <LbRowCard v-for="r in filtered" :key="r.key">
        <template #head>
          <span class="iv__kind">{{ kindLabel[r.kind] }}</span>
          <span class="iv__name">{{ rowName(r) }}</span>
          <LbStatusTag v-if="r.kind !== 'external'" :meta="addressFamilyMeta(rowHasIPv6(r))" />
          <LbStatusTag :meta="rowProtocolMeta(r)" />
          <LbStatusTag :meta="rowEnabled(r) ? inboundEnabledMeta.on : inboundEnabledMeta.off" />
        </template>

        <div class="iv__dim">
          <a v-if="isSelf(r)" @click="goNode(r)">{{ rowOwner(r) }}</a>
          <a v-else @click="goExternal">{{ rowOwner(r) }}</a>
          · {{ rowPortText(r) }}
          · {{ rowTier(r) }}
        </div>
        <div class="iv__dim">
          {{ exitText(r) }} ·
          {{ rowInSub(r) ? '在订阅里' : '不在订阅里' }}
          · 排序 <a class="lb-tabular" @click="openSort(r)">{{ globalText(r) }}</a>
        </div>
        <div>
          <!-- 配置状态只对 sing-box 行成立:Mieru 是另一个进程、另一份配置。 -->
          <LbStatusTag
            v-if="r.kind === 'singbox'"
            :meta="configStatusMeta[configState(r.node)]"
            :suffix="`rev ${r.node.config_revision}`"
          />
          <LbStatusTag v-else-if="r.kind === 'mieru' && !rowDeployed(r)" :meta="mieruPendingMeta" />
          <LbStatusTag v-else-if="r.kind === 'external'" :meta="expiryStatusMeta(r.proxy.expiry)" />
        </div>

        <template #foot>
          <template v-if="r.kind === 'external'">
            <a-button size="small" :disabled="!!running" @click="openEdit(r)">编辑</a-button>
            <a-button size="small" :disabled="!!running" @click="checkExternal(r.proxy)">检查</a-button>
            <a-button size="small" :disabled="!!running" @click="toggleExternalSub(r.proxy)">
              {{ r.proxy.subscription_enabled ? '停发订阅' : '恢复下发' }}
            </a-button>
            <a-button size="small" :disabled="!!running" @click="expiryTarget = r.proxy">续费</a-button>
          </template>
          <template v-else-if="isRelay(r)">
            <a-button size="small" :disabled="!!running" @click="goNode(r)">进入机器</a-button>
            <a-button size="small" :disabled="!!running" @click="deployRelays(r)">下发转发</a-button>
          </template>
          <template v-else>
            <a-button size="small" :disabled="!!running" @click="openEdit(r)">编辑</a-button>
            <a-button size="small" :disabled="!!running" @click="openChain(r)">出口</a-button>
            <a-button v-if="r.kind === 'mieru'" size="small" :disabled="!!running" @click="deployMieru(r)">下发</a-button>
            <a-button v-else size="small" :disabled="!!running" @click="doDeploy(r.node)">下发整台</a-button>
            <a-button size="small" danger :disabled="!!running" @click="remove(r)">删除</a-button>
          </template>
          <a-button size="small" :disabled="!!running" @click="openSort(r)">排序</a-button>
        </template>
      </LbRowCard>
      <LbEmptyState v-if="!filtered.length && !loading && !loadError" variant="filtered" title="没有匹配的线路" />
    </div>

    <a-table
      v-else
      :columns="columns"
      :data-source="filtered"
      :loading="loading"
      row-key="key"
      size="small"
      :scroll="{ x: 1180 }"
      :pagination="{ pageSize: 30, hideOnSinglePage: true }"
    >
      <template #emptyText>
        <LbEmptyState v-if="!loadError" variant="filtered" title="没有匹配的线路" />
        <span v-else />
      </template>
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'kind'">
          <span class="iv__kind" :class="{ 'iv__kind--external': (record as Row).kind === 'external' }">
            {{ kindLabel[(record as Row).kind] }}
          </span>
        </template>

        <template v-else-if="column.key === 'name'">
          <div class="iv__name">
            {{ rowName(record as Row) }}
            <LbStatusTag v-if="(record as Row).kind !== 'external'" :meta="addressFamilyMeta(rowHasIPv6(record as Row))" />
          </div>
          <div class="iv__sub">
            <LbStatusTag :meta="rowProtocolMeta(record as Row)" />
            <span class="iv__dim lb-mono">{{ rowSub(record as Row) }}</span>
          </div>
        </template>

        <template v-else-if="column.key === 'owner'">
          <a v-if="isSelf(record as Row)" @click="goNode(record as Row)">{{ rowOwner(record as Row) }}</a>
          <a v-else @click="goExternal">{{ rowOwner(record as Row) }}</a>
          <div v-if="isSelf(record as Row)" class="iv__dim">{{ (record as SingBoxRow).node.host }}</div>
          <div v-else class="iv__dim">外部代理页管理源与同步</div>
        </template>

        <template v-else-if="column.key === 'port'">
          <span class="lb-mono">{{ rowPortText(record as Row) }}</span>
        </template>

        <template v-else-if="column.key === 'exit'">
          <div>{{ exitText(record as Row) }}</div>
          <div class="iv__dim">等级 {{ rowTier(record as Row) }}</div>
        </template>

        <!-- 全局排序值:GLOBAL 方案下是一个数;旧方案下写明「机器几 · 入口几」,
             点一下打开调整弹窗 —— 自建入口要选清楚改的是机器还是入口。 -->
        <template v-else-if="column.key === 'order'">
          <a class="iv__order lb-tabular" title="调整排序" @click="openSort(record as Row)">{{ globalText(record as Row) }}</a>
        </template>

        <template v-else-if="column.key === 'state'">
          <LbStatusTag
            :meta="rowEnabled(record as Row) ? inboundEnabledMeta.on : inboundEnabledMeta.off"
          />
          <!-- **机器的配置状态只对 sing-box 行成立。** 它算的是 config.json
               与库里那份的差异,而 Mieru 是另一个进程、另一份配置 ——
               把它贴在 Mieru 行上会让"已同步"这三个字说一件不成立的事。 -->
          <LbStatusTag
            v-if="(record as Row).kind === 'singbox'"
            :meta="configStatusMeta[configState((record as SingBoxRow).node)]"
          />
          <LbStatusTag
            v-else-if="(record as Row).kind === 'mieru' && !rowDeployed(record as Row)"
            :meta="mieruPendingMeta"
          />
          <LbStatusTag
            v-else-if="(record as Row).kind === 'external'"
            :meta="expiryStatusMeta((record as ExternalRow).proxy.expiry)"
          />
          <div class="iv__dim">
            {{ rowInSub(record as Row) ? '在订阅里' : '不在订阅里' }}
          </div>
        </template>

        <template v-else-if="column.key === 'ops'">
          <!-- 操作按类型分派:外部代理只给适用的几样,不显示「安装 sing-box」
               「重启节点」这类对它无效的按钮;转发的编辑在节点详情里。 -->
          <a-space v-if="(record as Row).kind === 'external'" :size="4" wrap>
            <a-button size="small" :disabled="!!running" @click="openEdit(record as Row)">编辑</a-button>
            <a-button size="small" :disabled="!!running" @click="checkExternal((record as ExternalRow).proxy)">检查</a-button>
            <a-dropdown :disabled="!!running">
              <a-button size="small">更多</a-button>
              <template #overlay>
                <a-menu>
                  <a-menu-item @click="toggleExternalSub((record as ExternalRow).proxy)">
                    {{ (record as ExternalRow).proxy.subscription_enabled ? '停发订阅' : '恢复下发' }}
                  </a-menu-item>
                  <a-menu-item @click="expiryTarget = (record as ExternalRow).proxy">续费 / 修改到期时间</a-menu-item>
                  <a-menu-item @click="openSort(record as Row)">调整排序</a-menu-item>
                  <a-menu-divider />
                  <a-menu-item @click="goExternal">去「外部代理」页(源 / 同步 / 删除)</a-menu-item>
                </a-menu>
              </template>
            </a-dropdown>
          </a-space>
          <a-space v-else-if="isRelay(record as Row)" :size="4" wrap>
            <a-button size="small" :disabled="!!running" @click="goNode(record as Row)">进入机器</a-button>
            <a-button size="small" :disabled="!!running" @click="deployRelays(record as RelayRow)">下发转发</a-button>
            <a-dropdown :disabled="!!running">
              <a-button size="small">更多</a-button>
              <template #overlay>
                <a-menu>
                  <a-menu-item @click="openSort(record as Row)">调整排序</a-menu-item>
                  <a-menu-item @click="goNode(record as Row)">编辑 / 删除(节点详情)</a-menu-item>
                </a-menu>
              </template>
            </a-dropdown>
          </a-space>
          <a-space v-else :size="4" wrap>
            <a-button size="small" :disabled="!!running" @click="openEdit(record as Row)">编辑</a-button>
            <a-button size="small" :disabled="!!running" @click="openChain(record as SingBoxRow | MieruRow)">出口</a-button>
            <!-- **下发那一档按类型分。** sing-box 是整台机器一次(踢掉这台机器上
                 全部 sing-box 入口的连接),Mieru 是逐入口各下各的(只断那一个)
                 —— 两者的后果差得很远,放在同一个按钮下面只能写一句废话。 -->
            <a-button
              v-if="(record as Row).kind === 'mieru'"
              size="small"
              :disabled="!!running"
              @click="deployMieru(record as MieruRow)"
            >
              下发
            </a-button>
            <a-dropdown :disabled="!!running">
              <a-button size="small">更多</a-button>
              <template #overlay>
                <a-menu>
                  <a-menu-item @click="openSort(record as Row)">调整排序</a-menu-item>
                  <a-menu-item
                    v-if="
                      (record as Row).kind === 'singbox' &&
                      (record as SingBoxRow).inbound.protocol === 'VLESS_REALITY'
                    "
                    @click="openDest(record as SingBoxRow)"
                  >
                    实测握手目标
                  </a-menu-item>
                  <a-menu-item @click="goNode(record as Row)">进入这台机器</a-menu-item>
                  <a-menu-divider />
                  <!-- 机器级操作单独一组并写明"整台" —— 它们影响这台机器上
                       全部 sing-box 入口,而管理员是从某一行点进来的。
                       Mieru 行上也留着:同机的 sing-box 照样可能待下发。 -->
                  <a-menu-item @click="doDeploy((record as SingBoxRow).node)">
                    下发整台机器的 sing-box
                  </a-menu-item>
                  <a-menu-item @click="doRestart((record as SingBoxRow).node)">
                    重启整台机器的 sing-box
                  </a-menu-item>
                  <a-menu-divider />
                  <a-menu-item danger @click="remove(record as SingBoxRow | MieruRow)">删除这个入口</a-menu-item>
                </a-menu>
              </template>
            </a-dropdown>
          </a-space>
        </template>
      </template>
    </a-table>
    </div>
    </section>

    <template v-if="targetNode && target">
      <InboundFormModal
        v-model:open="formOpen"
        :inbound="creating ? null : target.inbound"
        :node="targetNode"
        :tiers="tiers"
        :existing-count="(targetNode.inbounds ?? []).length"
        @saved="load"
      />
      <InboundChainModal
        v-model:open="chainOpen"
        :inbound="target.inbound"
        :node="targetNode"
        @applied="load"
      />
      <InboundDestModal v-model:open="destOpen" :inbound="target.inbound" @applied="load" />
    </template>

    <!-- Mieru 有自己的一套弹窗:它们收的是 MieruInbound,而且确认文案不一样
         —— 那边下发会踢掉这台机器上全部 sing-box 入口,这边只断一个入口。 -->
    <template v-if="mieruTarget">
      <MieruInboundFormModal
        v-model:open="mieruFormOpen"
        :inbound="mieruTarget.mieru"
        :node="mieruTarget.node"
        :tiers="tiers"
        :existing-count="(mieruTarget.node.mieru_inbounds ?? []).length"
        @saved="load"
      />
      <MieruChainModal
        v-model:open="mieruChainOpen"
        :inbound="mieruTarget.mieru"
        :node="mieruTarget.node"
        @applied="load"
      />
    </template>

    <!-- 外部代理用它自己页面上的那个表单:两处操作同一份记录,不复制。 -->
    <ExternalProxyModal
      v-model:open="externalFormOpen"
      :proxy="externalTarget"
      :tiers="tiers"
      @saved="load"
    />
    <ExpiryModal
      v-if="expiryTarget"
      :open="true"
      kind="EXTERNAL_PROXY"
      :object-id="expiryTarget.id"
      :name="expiryTarget.final_display_name || expiryTarget.display_name"
      :source-name="expiryTarget.source_name"
      @update:open="(v: boolean) => { if (!v) expiryTarget = null }"
      @changed="load"
    />

    <EntrySortModal v-model:open="sortOpen" :target="sortTargetRow" :scheme="scheme" @saved="load" />

    <a-modal
      v-model:open="deployOpen"
      :title="`部署 ${deployNode ? nodeLabel(deployNode) : ''}`"
      :footer="null"
      :closable="!deployRunning"
      :mask-closable="!deployRunning"
      :keyboard="!deployRunning"
      width="640px"
    >
      <div v-if="deployRunning" class="iv__deploying">
        正在部署,15~25 秒。健康检查不通过会自动回滚 —— 这期间不要关闭页面。
      </div>
      <template v-else-if="deployAsRecord">
        <DeployStepList :record="deployAsRecord" />
        <div class="iv__deploy-foot">
          <a-button type="primary" @click="deployOpen = false">完成</a-button>
        </div>
      </template>
    </a-modal>
  </div>
</template>

<style scoped>
/* 颜色只用 tokens 里已有的值。 */
.iv__pending {
  color: var(--warn);
}
.iv__scheme {
  margin-left: 6px;
  color: var(--text3);
}
.iv__error {
  margin: 12px 20px 0;
}
.iv__name {
  font-weight: 500;
  color: var(--text);
}
.iv__kind {
  display: inline-block;
  padding: 1px 7px;
  border-radius: 6px;
  background: var(--fill);
  font-size: 11.5px;
  color: var(--text2);
  white-space: nowrap;
}
.iv__kind--external {
  background: var(--purple-bg);
  color: var(--purple);
}
.iv__order {
  font-weight: 600;
  text-decoration: underline dotted;
  text-underline-offset: 3px;
}
.iv__dim {
  font-size: 12px;
  color: var(--text3);
}
.iv__sub {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  margin-top: 3px;
}
.iv__cards {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.iv__deploying {
  padding: 24px 8px;
  font-size: 13px;
  line-height: 1.7;
  color: var(--text3);
}
.iv__deploy-foot {
  margin-top: 16px;
  text-align: right;
}
</style>
