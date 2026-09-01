<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type ChainApplyResult,
  type DeployResult,
  type AccessTier,
  type ExternalProxy,
  type NginxFacts,
  type RealmFacts,
  type Node,
  type MieruInbound,
  type NodeInbound,
  type NodeRelay,
  type NodeAddress,
  type InboundEndpointInput,
  type SingBoxChannel,
} from '@/api/client'
import {
  LbNameConfirm,
  LbRowCard,
  LbStatusTag,
  lbDangerConfirm,
  type LbStatusMeta,
} from '@/components/lb'
import InboundChainModal from './InboundChainModal.vue'
import InboundDestModal from './InboundDestModal.vue'
import InboundFormModal from './InboundFormModal.vue'
import MieruChainModal from './MieruChainModal.vue'
import NodeOpProgressModal from './NodeOpProgressModal.vue'
import { confirmDeployNode, confirmRestartNode } from './nodeOps'
import { sortByEntryOrder } from './entryOrder'
import MieruInboundFormModal from './MieruInboundFormModal.vue'
import EndpointsEditor from './EndpointsEditor.vue'
import {
  addressFamilyMeta,
  confirmRemoveInbound,
  confirmRemoveMieruInbound,
  inboundHasIPv6Entry,
  inboundProtocolMeta as protocolMeta,
  portText,
} from './inboundOps'
import { useNarrow } from '@/composables/useNarrow'
import { color } from '@/theme/tokens'

/**
 * 节点的「入口」面板:这台机器对外提供的全部入口。
 *
 * 两类入口在同一个列表里,因为对管理员来说它们回答的是同一个问题
 * ——「用户连这台机器的哪个端口、连上之后去哪」:
 *
 *   sing-box 入口   这台机器自己的入站。V8 起可以有多条,各自的协议、
 *                   端口、访问等级与出口去向互不相干。
 *   nginx 转发入口  把字节原样搬到落地,可以有多条,各占一个端口。
 *
 * 叫「入口」而不是「订阅」:面板里「订阅」已经是用户手上那条
 * /sub/{token} 链接,同一个词两个意思会在后台与门户之间来回打架。
 * 而「入口」与每一行的「出口」正好成对,一眼看得出方向。
 *
 * **两类入口的操作摩擦不同,必须让它们看起来不同**:
 *   nginx 转发   改了只 reload,在途连接一条不断 → 普通确认;
 *   sing-box     改了要重启,踢掉这台机器上【全部入口】的在线连接,
 *                改出口还连带部署另一台机器 → lbDangerConfirm,逐条列影响。
 * 合成一种确认之后,管理员会对「点这一下要不要挑时机」失去判断。
 */
const props = defineProps<{ node: Node; tiers: AccessTier[] }>()
const emit = defineEmits<{
  /** 有动作在跑时抬起,页面据此在弹窗上屏蔽遮罩点击与 ESC */
  busy: [label: string]
  /** 变更之后节点本身可能变了,让页面重新拉一次 */
  changed: []
}>()

const narrow = useNarrow()

/**
 * 可作为落地的候选。在面板里现拉而不是让页面传:
 * 它们只在配置入口时才需要,而节点详情每次打开都带着会白拉两个接口。
 *
 * 落地候选**排除中转角色的机器与这台机器自己**:前者上面没有 sing-box,
 * 转发过去只会得到一条连不上的线路;后者会让流量绕回自己 —— 出口 IP
 * 一个字节都没变,而管理员以为配了一条链路。
 */
const landingNodes = ref<Node[]>([])
const externalProxies = ref<ExternalProxy[]>([])

/**
 * 落地候选精确到【入站】而不是机器。
 *
 * 一台机器上有两个入口时,「转发到 B」是有歧义的,而歧义的表现是流量进了
 * 管理员没打算用的那个入口(协议、端口、等级都不同),没有任何一层会报错。
 * 所以下拉里每一项都写清「机器 / 入口」。
 */
const landingInbounds = computed(() =>
  landingNodes.value.flatMap((n) =>
    (n.inbounds ?? []).map((i) => ({
      value: i.id,
      label: `${n.display_name || n.name} / ${i.display_name}${
        i.deployed_protocol ? '' : '(未部署过)'
      }`,
    })),
  ),
)

const relays = ref<NodeRelay[]>([])
const loading = ref(false)
const loadError = ref('')
const running = ref('')
const nginx = ref<NginxFacts | null>(null)
const nginxError = ref('')
const realm = ref<RealmFacts | null>(null)
const realmError = ref('')
const lastChain = ref<ChainApplyResult | null>(null)

/**
 * 中转角色的机器上没有 sing-box 入站 —— 那一块整个不出现。
 *
 * 显示一个"未配置"的占位会让人以为可以去配一个,而角色一经创建不可更改:
 * 要在这台机器上跑 sing-box,只能删了重建。
 */
const isRelayHost = computed(() => props.node.role === 'RELAY')
const inbounds = computed(() => props.node.inbounds ?? [])
// Mieru 入口与 sing-box 入站是两个数组:服务端是两个进程,参数与下发方式
// 都不一样。合并只发生在下面的 rows 里,也就是【展示】这一层。
const mierus = computed(() => props.node.mieru_inbounds ?? [])
const nodeLabel = computed(() => props.node.display_name || props.node.name)

/**
 * 这台机器上装的是哪一支 sing-box。
 *
 * 显示出来而不是只在安装那一刻说一句:管理员半年后回到这个页面时,
 * 「为什么这台能选 Snell 那台不能」的答案必须就在眼前 ——
 * 否则他只会得出"面板有 bug"这个结论。
 */
const channelTag = computed(() =>
  props.node.singbox_channel === 'PREVIEW'
    ? { text: '预览版 1.14', shape: 'dot' as const, fg: '#8A5300', bg: '#FDF3E2', bd: '#F0DCB6' }
    : { text: '正式版', shape: 'dot' as const, fg: '#4A5568', bg: '#F2F4F7', bd: '#DDE1E8' },
)

async function loadTargets() {
  try {
    const [ns, ps] = await Promise.all([api.nodes(), api.externalProxies()])
    landingNodes.value = (ns.items ?? []).filter(
      (n) => n.role !== 'RELAY' && n.id !== props.node.id,
    )
    externalProxies.value = ps.items ?? []
  } catch {
    // 拉不到候选不影响看现有入口 —— 那才是打开这个 Tab 最常见的目的。
    // 下拉里会是空的,新增时立刻就能发现,而不是拿到一份错的列表。
    landingNodes.value = []
    externalProxies.value = []
  }
}

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    relays.value = (await api.nodeRelays(props.node.id)).items ?? []
  } catch (e) {
    // 列表读不到时不显示「暂无数据」——那会被读成「一条都没有」,
    // 而实际是没读到。表格保持空白,上方显式说明原因。
    relays.value = []
    loadError.value = e instanceof ApiError ? e.message : '读取转发规则失败'
  } finally {
    loading.value = false
  }
}

// ---------------------------------------------------------------- 状态标记

const readyMeta: Record<'yes' | 'no', LbStatusMeta> = {
  yes: { text: '落地就绪', shape: 'check', fg: color.success, bg: color.successBg, bd: color.successBorder },
  // 三重编码:形状 + 文案 + 颜色。打印与色觉障碍下颜色全部失效,
  // 而「落地未就绪」与「已停用」的处置方式完全不同。
  no: { text: '落地未就绪', shape: 'triangle', fg: color.warning, bg: color.warningBg, bd: color.warningBorder },
}
const enabledMeta: Record<'on' | 'off', LbStatusMeta> = {
  on: { text: '启用', shape: 'check', fg: color.success, bg: color.successBg, bd: color.successBorder },
  off: { text: '已停用', shape: 'minus', fg: color.neutral, bg: color.neutralBg, bd: color.neutralBorder },
}
/**
 * 不计流量的入口(V15)要在列表里一眼看得出来:它的流量既不扣用户额度,
 * 也不进机器周期用量 —— 那张图上少的那一截,只有这个标记能解释。
 * 用警告色:它不是坏了,但它改变了"停用即断线、超额即断线"这两条全站规矩。
 */
const unmeteredMeta: LbStatusMeta = {
  text: '不计流量', shape: 'triangle', fg: color.warning, bg: color.warningBg, bd: color.warningBorder,
}

/**
 * Mieru 入口的传输层标记,与 inboundProtocolMeta 同构。
 *
 * **显示的是【已经生效】的那一个,不是数据库里的期望值** —— 改了传输层
 * 到下发成功之间的窗口里,订阅下发的、用户实际连的还是旧的那一种。
 * 两者不一致时把箭头写出来,那正是「还没下发」这件事唯一看得见的地方。
 */
function mieruMeta(m: MieruInbound): LbStatusMeta {
  if (!m.deployed_transport) {
    return { text: '未下发', shape: 'ring', fg: color.neutral, bg: color.neutralBg, bd: color.neutralBorder }
  }
  const pending = m.deployed_transport !== m.transport
  return {
    text: pending ? `${m.deployed_transport} → ${m.transport} 待下发` : `Mieru ${m.deployed_transport}`,
    shape: pending ? 'triangle' : 'check',
    fg: pending ? color.warning : color.success,
    bg: pending ? color.warningBg : color.successBg,
    bd: pending ? color.warningBorder : color.successBorder,
  }
}

/**
 * 两类入口合成一份列表。
 *
 * 它们回答的是同一个问题 ——「用户连这台机器的哪个端口、连上之后去哪」,
 * 分成两张表之后要在两处之间来回看才拼得出这台机器对外的全貌。
 *
 * **但摩擦不同这件事必须留在界面上**:sing-box 那几行改了要重启、
 * 会踢掉这台机器上全部入口的在线连接;nginx 那几行只 reload,在途连接
 * 一条不断。所以每行都带类型标记,操作按钮也各走各的确认档次 ——
 * 合成一种确认之后,管理员会对「点这一下要不要挑时机」失去判断。
 *
 * 排序:sing-box 在前、nginx 在后,各自按 sort_order。不按端口号混排 ——
 * 那样两类会交错,类型标记就得逐行去认。
 */
type EntryRow =
  | { key: string; kind: 'singbox'; inbound: NodeInbound }
  | { key: string; kind: 'mieru'; mieru: MieruInbound }
  | { key: string; kind: 'nginx'; relay: NodeRelay }
  // realm 与 nginx 共用 NodeRelay(同一张表),但在列表里是两种行:
  // 它们的下发摩擦不同档,而「去向」那一列的判据也要分得出是谁在搬字节。
  | { key: string; kind: 'realm'; relay: NodeRelay }

// key 的前缀不能省:三张表各自从 1 开始,不加前缀的话 i1 / m1 / r1
// 会撞成同一个 key,而 Vue 会把三行当成同一行来复用 DOM。
//
// **三类一起按 sort_order 排**(V14.1)。分三段拼起来的话,那个数字
// 只在同一类之内有意义 —— 管理员把 Mieru 入口排到 0、VLESS 入口排到 1,
// 列表里 VLESS 那条仍然在前面,而他改了、保存了、什么都没发生。
// 判据与后端订阅那一侧(subscription.EntryOrder)完全一致:两边分叉的话,
// 这里看到的顺序与用户客户端里的对不上。
//
// 这一屏只有一台机器,所以机器那两个键取常数 —— 排序结果只由
// sort_order 与兜底键决定。
const rows = computed<EntryRow[]>(() =>
  sortByEntryOrder<EntryRow>(
    [
      ...inbounds.value.map((i) => ({ key: `i${i.id}`, kind: 'singbox' as const, inbound: i })),
      ...mierus.value.map((m) => ({ key: `m${m.id}`, kind: 'mieru' as const, mieru: m })),
      ...relays.value.map((r) =>
        r.engine === 'REALM'
          ? { key: `r${r.id}`, kind: 'realm' as const, relay: r }
          : { key: `r${r.id}`, kind: 'nginx' as const, relay: r },
      ),
    ],
    (row) => ({
      nodeSort: 0,
      nodeId: 0,
      sort: rowSortOrder(row),
      kind: row.kind,
      id: rowID(row),
    }),
  ),
)

function rowSortOrder(row: EntryRow): number {
  if (row.kind === 'singbox') return row.inbound.sort_order
  if (row.kind === 'mieru') return row.mieru.sort_order
  return row.relay.sort_order
}

function rowID(row: EntryRow): number {
  if (row.kind === 'singbox') return row.inbound.id
  if (row.kind === 'mieru') return row.mieru.id
  return row.relay.id
}

const kindLabel: Record<EntryRow['kind'], string> = {
  singbox: 'sing-box',
  mieru: 'Mieru',
  nginx: 'nginx 转发',
  realm: 'realm 转发',
}

function kindText(row: EntryRow): string {
  return kindLabel[row.kind]
}

/**
 * 一行的取值都走这几个小函数,而不是在模板里写三元。
 *
 * 模板里逐处判断 kind 的话,加一列就要在两个分支各写一遍(表格与窄屏卡片),
 * 而漏掉其中一处的表现是「桌面上对、手机上空着」—— 那种差异不会有人主动去找。
 */
function rowName(row: EntryRow): string {
  if (row.kind === 'singbox') return row.inbound.display_name
  if (row.kind === 'mieru') return row.mieru.display_name
  return row.relay.display_name
}

/** 端口段渲染成 30000-30010,单端口只渲染一个号码 —— 与后端同一套写法。 */
function rangeText(start: number, end: number): string {
  if (!start) return ''
  return start === end ? String(start) : `${start}-${end}`
}

function rowPort(row: EntryRow): string {
  if (row.kind === 'singbox') {
    return portText(row.inbound.listen_port, row.inbound.public_port)
  }
  if (row.kind === 'mieru') {
    const listen = rangeText(row.mieru.listen_port_start, row.mieru.listen_port_end)
    const pub = rangeText(row.mieru.public_port_start, row.mieru.public_port_end)
    // 与 portText 同一条规矩:两者相同时只写一个,不然那一列全是重复的号码。
    return pub && pub !== listen ? `${listen} → 公网 ${pub}` : listen
  }
  return portText(row.relay.listen_port, row.relay.public_port)
}

function rowTier(row: EntryRow): string {
  if (row.kind === 'singbox') return row.inbound.access_tier_name
  if (row.kind === 'mieru') return row.mieru.access_tier_name
  return row.relay.access_tier_name
}

function rowEnabled(row: EntryRow): boolean {
  if (row.kind === 'singbox') return row.inbound.enabled
  if (row.kind === 'mieru') return row.mieru.enabled
  return row.relay.enabled
}

function rowSubText(row: EntryRow): string {
  const on =
    row.kind === 'singbox'
      ? row.inbound.subscription_enabled
      : row.kind === 'mieru'
        ? row.mieru.subscription_enabled
        : row.relay.subscription_enabled
  return on ? '在订阅里' : '已从订阅下架'
}

/**
 * 「去向」这一列:这个入口收到的流量最终从哪里出去。
 *
 * 两类入口的答案形状不同,但问题是同一个 —— 合在一列里,管理员扫一眼
 * 就能看出这台机器上哪些入口是本机出网、哪些绕到了别处。
 */
function destinationText(row: EntryRow): string {
  if (row.kind === 'nginx' || row.kind === 'realm') {
    if (row.relay.target_kind === 'ADDRESS') {
      // 指定地址不进订阅:面板不知道它背后的协议,造不出条目。这一列要说出来,
      // 否则管理员会对着一条"配好了却不在订阅里"的线路找半天。
      return `转发到 ${row.relay.target_name}(指定地址,不进订阅)`
    }
    return `透传到 ${row.relay.target_name || '(落地已删除)'}`
  }
  if (row.kind === 'mieru') {
    // Mieru 的出口要经本机 sing-box 转一跳(mita 的出口代理只认 SOCKS5),
    // 所以这里写"经 sing-box → 落地"而不是只写落地的名字 ——
    // 只写落地会让人以为 mita 直接拨到了那台机器上,而排查时那一跳
    // 恰恰是最容易被忘掉的一环。
    const t = mieruChainName(row.mieru)
    return t ? `经本机 sing-box → ${t}` : '本机直连'
  }
  const chain = chainTargetName(row.inbound)
  return chain ? `经 ${chain}` : '本机直连'
}

/**
 * 一行在订阅里占几个地址族。
 *
 * 中转那一类没有逐条开关 —— 只要机器填了 IPv6,它的转发条目就一起展开
 * (subscription.PhysicalRelay.Expand)。两类的规则不同,但答案是同一个
 * 问题,所以合在一列里给。
 */
function familyOf(row: EntryRow): LbStatusMeta {
  const dual =
    row.kind === 'singbox'
      ? inboundHasIPv6Entry(row.inbound, props.node)
      : row.kind === 'mieru'
        ? !!props.node.ipv6_address && row.mieru.ipv6_enabled
        : !!props.node.ipv6_address
  return addressFamilyMeta(dual)
}

/**
 * 一个入口在订阅里展开成的条目(IPv4 + 可选 IPv6)。
 *
 * IPv6 那条的名字取后端算好的 ipv6_entry_name,**不在这里拼 -IPV6 后缀** ——
 * 回落只有 subscription.IPv6EntryName 一处实现,这里再拼一遍就是第二处,
 * 而分叉的表现是面板上写的名字与用户客户端里那条对不上,两边都不报错。
 */
function subEntriesOf(i: NodeInbound) {
  const port = i.public_port || i.listen_port
  // IPv4 那条取【订阅地址】而不是管理地址:这一块显示的是"用户会拿到什么",
  // 拿管理地址来拼的话,一台两者不同的机器上,面板显示的地址与用户客户端里
  // 那一条对不上 —— 而两边都不报错,而排查"用户连不上"的人正是照着这里的
  // 地址去测的。用后端算好的 subscription_host,不在这里自己写回落 ——
  // 与上面 ipv6_entry_name 不在这里拼后缀是同一条道理。
  const out = [{ name: i.display_name, addr: `${props.node.subscription_host}:${port}` }]
  if (inboundHasIPv6Entry(i, props.node)) {
    out.push({
      name: i.ipv6_entry_name,
      addr: `[${props.node.ipv6_address}]:${i.ipv6_public_port || port}`,
    })
  }
  return out
}

function chainTargetName(i: NodeInbound) {
  if (i.chain_target_kind === 'INBOUND') {
    const found = landingInbounds.value.find((c) => c.value === i.chain_target_inbound_id)
    return found ? found.label : `入口 #${i.chain_target_inbound_id}`
  }
  if (i.chain_target_kind === 'EXTERNAL') {
    const p = externalProxies.value.find((x) => x.id === i.chain_target_external_id)
    return p ? p.display_name : `外部代理 #${i.chain_target_external_id}`
  }
  return ''
}

/**
 * Mieru 入口的落地名字。
 *
 * 与 chainTargetName 同一套查表,但**故意不合并成一个泛型函数** ——
 * 两边的字段虽然同名,合并要么改两个类型、要么加一层 any,
 * 而这几行的重复远比那个便宜。
 */
function mieruChainName(m: MieruInbound) {
  if (m.chain_target_kind === 'INBOUND') {
    const found = landingInbounds.value.find((c) => c.value === m.chain_target_inbound_id)
    return found ? found.label : `入口 #${m.chain_target_inbound_id}`
  }
  if (m.chain_target_kind === 'EXTERNAL') {
    const p = externalProxies.value.find((x) => x.id === m.chain_target_external_id)
    return p ? p.display_name : `外部代理 #${m.chain_target_external_id}`
  }
  return ''
}

// ---------------------------------------------------------------- sing-box 入口
//
// 表单、握手目标、出口去向三个弹窗都抽到了独立组件里 —— 节点详情的这个
// 面板与跨节点的「入口管理」页都要开它们,而它们的确认文案里写的是
// **两台机器**上会发生什么。各写一份的话,少列一条的后果是管理员按一个
// 不完整的清单去判断这一下要不要挑时机。见 InboundFormModal / InboundChainModal /
// InboundDestModal 与共用的 inboundOps.ts。

const inboundOpen = ref(false)
const editingInbound = ref<NodeInbound | null>(null)

function openCreateInbound() {
  editingInbound.value = null
  inboundOpen.value = true
}

function openEditInbound(i: NodeInbound) {
  editingInbound.value = i
  inboundOpen.value = true
}

function removeInbound(i: NodeInbound) {
  confirmRemoveInbound(i, nodeLabel.value, runWithBusy, () => emit('changed'))
}

// ---------------------------------------------------------------- Mieru 入口
//
// 与 sing-box 那一组分开:它们改动之后的后果不一样(重启的是两个不同的进程),
// 所以按钮分行、确认档次各走各的。合成一种之后,管理员会对
// 「点这一下要不要挑时机」失去判断。

const mieruOpen = ref(false)
const editingMieru = ref<MieruInbound | null>(null)

function openCreateMieru() {
  editingMieru.value = null
  mieruOpen.value = true
}

function openEditMieru(m: MieruInbound) {
  editingMieru.value = m
  mieruOpen.value = true
}

/**
 * 删除一个 Mieru 入口。
 *
 * **不自动下发**,所以确认文案里必须写明"它还在跑" —— 不写的话,
 * 管理员会以为点完删除权限就收回了,而实际上要等下一次下发。
 * 用 lbDangerConfirm 而不是打字确认:它可以撤回(重新建一个同样的入口),
 * 而且这一下不打断任何人。
 */
function removeMieru(m: MieruInbound) {
  confirmRemoveMieruInbound(m, nodeLabel.value, runWithBusy, () => emit('changed'))
}

// ---------------------------------------------------------------- 去节点上做事
//
// 装、卸、下发、重启都要连 SSH,少则两三秒、多则二十几秒,而结果恰恰是
// 最需要读的:失败时要看卡在哪一步,成功时要看它到底做了什么。
// 所以它们**一律走进度弹窗**,不用吐司 —— 三秒吐司交付不了一份步骤表。
//
// 全部收在这个面板里而不是散到页面上:一台机器上有三类服务,
// 它们的装卸下发是九个动作,分散之后"这一下影响谁"就要在两个文件之间
// 来回看才拼得出来。

// 卸载是那四个不可逆动作之一,走打字确认(LbNameConfirm)。
// **不给它降档成 lbDangerConfirm**:给可撤回的操作也加打字摩擦,
// 管理员很快会变成无脑复制粘贴,真正不可逆的那几个反而失去警示作用 ——
// 反过来把不可逆的降档,是把唯一一道刹车拿掉。
type UninstallKind = 'singbox' | 'mieru' | 'nginx' | 'realm'
const uninstallKind = ref<UninstallKind | null>(null)

const uninstallMeta = computed(() => {
  if (!uninstallKind.value) return null
  return {
    singbox: {
      title: `卸载 ${nodeLabel.value} 上的 sing-box`,
      okText: '卸载 sing-box',
      impacts: [
        '停止并删除 sing-box 服务、二进制、配置与备份。',
        `这台机器上全部 ${inbounds.value.length} 个 sing-box 入口的用户立刻断线。`,
        'Mieru 入口与 nginx 转发不受影响 —— 它们是另外两个服务。',
        '入口记录都留在面板里,重新「安装 sing-box」+「下发配置」就能回来。',
      ],
    },
    mieru: {
      title: `卸载 ${nodeLabel.value} 上的 Mieru`,
      okText: '卸载 Mieru',
      impacts: [
        `停止并删除全部 ${mierus.value.length} 个 mita 实例、二进制与实例目录。`,
        '这台机器上全部 Mieru 入口的用户立刻断线。',
        'sing-box 与 nginx 不受影响。',
        '入口记录都留在面板里(端口段、等级、链路凭据都不删),' +
          '重新「安装 Mieru」再逐个「下发」就能回来。',
        '但它们会先从订阅里消失 —— 节点上已经没有它们了,' +
          '继续下发会让用户拿到一条永远连不上的线路。',
      ],
    },
    nginx: {
      title: `卸载 ${nodeLabel.value} 上的 nginx 转发`,
      okText: '卸载 nginx',
      impacts: [
        '停止并删除面板托管的那个 nginx 实例(litebox-nginx)与它的配置。',
        `这台机器上全部 ${relays.value.length} 条转发线路立刻中断。`,
        '**节点自带的 nginx 一个字不动** —— 面板装的是一个独立实例。',
        'sing-box 与 Mieru 入口不受影响。',
        '转发规则都留在面板里,重新「下发转发配置」就能回来。',
      ],
    },
    realm: {
      title: `卸载 ${nodeLabel.value} 上的 realm 转发`,
      okText: '卸载 realm',
      impacts: [
        '停止并删除面板托管的 realm 服务(litebox-realm)、二进制、配置与备份。',
        `这台机器上全部 ${relays.value.filter((r) => r.engine === 'REALM').length} 条 realm 线路立刻中断。`,
        'sing-box、Mieru 入口与 nginx 转发不受影响。',
        '转发规则都留在面板里,重新「安装 realm」+「下发 realm 配置」就能回来。',
      ],
    },
  }[uninstallKind.value]
})

function doUninstall() {
  const kind = uninstallKind.value
  uninstallKind.value = null
  if (kind === 'singbox') {
    void runOp('卸载 sing-box', '正在停止服务并清理文件', async () => {
      const r = await api.uninstallSingBox(props.node.id)
      opSteps.value = r.result.steps
      opError.value = r.error ?? ''
    })
  } else if (kind === 'mieru') {
    void runOp('卸载 Mieru', '正在停止全部实例并清理文件', async () => {
      const r = await api.uninstallMieru(props.node.id)
      opSteps.value = r.result.steps
      opError.value = r.error ?? ''
    })
  } else if (kind === 'nginx') {
    void runOp('卸载 nginx 转发', '正在停止实例并清理配置', async () => {
      const r = await api.uninstallNginx(props.node.id)
      opSteps.value = r.result.steps
      opError.value = r.error ?? ''
      await checkNginx()
    })
  } else if (kind === 'realm') {
    void runOp('卸载 realm 转发', '正在停止服务并清理文件', async () => {
      const r = await api.uninstallRealm(props.node.id)
      opSteps.value = r.result.steps
      opError.value = r.error ?? ''
      await checkRealm()
    })
  }
}

const opOpen = ref(false)
const opTitle = ref('')
const opRunning = ref('')
const opSteps = ref<string[]>([])
const opDeploy = ref<DeployResult | null>(null)
const opError = ref('')
const opNote = ref('')
// 装完之后「已安装」的判据变了、下发完之后 deployed_* 变了,
// 所以要重拉 —— 但不能在结果还在屏幕上时拉,见 runOp 的 finally。
const opNeedsReload = ref(false)

/**
 * 跑一个节点操作,把过程与结果放进弹窗。
 *
 * fn 抛错与 fn 返回 error 字段是两回事:前者是没跑起来(网络、鉴权),
 * 后者是跑了但失败了(而且通常带着已经做完的步骤)。两种都要显示,
 * 但后者必须连步骤一起 —— "停了服务但没删定义"与"什么都没做"
 * 要人做的事完全不同。
 */
async function runOp(title: string, running: string, fn: () => Promise<void>) {
  opTitle.value = title
  opRunning.value = running
  opSteps.value = []
  opDeploy.value = null
  opError.value = ''
  opNote.value = ''
  opOpen.value = true
  // busy 同时抬起来:外面那个抽屉据此屏蔽遮罩点击与 ESC。
  emit('busy', running)
  try {
    await fn()
  } catch (e) {
    // status 0 = 请求根本没拿到响应(连接中断,或反向代理在操作完成前超时)。
    // 节点操作在后端与这次请求是解绑的,断开不会中止它 —— 它很可能已经在
    // 后台跑完了。所以不能只说"操作失败",那会让管理员去重试一件已经做完的事
    // (再重启一次服务、再踢一次在线连接)。
    if (e instanceof ApiError && e.status === 0) {
      opError.value =
        '与面板的连接中断了 —— 网络波动,或反向代理在操作完成前超时' +
        '(反代的 proxy_read_timeout 要 ≥ 面板的 10 分钟长操作上限)。\n' +
        '这类节点操作在后端与本次请求解绑,断开【不会】中止它,它很可能已经跑完了。\n' +
        '关掉本窗口刷新页面,看节点状态与「部署记录」的最新一条再决定要不要重试。'
    } else {
      opError.value = e instanceof ApiError ? e.message : '操作失败'
    }
  } finally {
    opRunning.value = ''
    emit('busy', '')
    // **重拉要等到弹窗关掉。** 页面的 reload 会把 loading 抬起来,
    // 而那一屏在 loading 期间整个换成骨架 —— 这个面板会被卸载,
    // 连同刚刚跑完的那份结果一起消失。表现是"点了安装,转了几秒,
    // 窗口自己没了",而操作其实是成功的。
    opNeedsReload.value = true
  }
}

/** 弹窗关掉时才让外面重拉,理由见 runOp 的 finally。 */
function closeOp(v: boolean) {
  opOpen.value = v
  if (!v && opNeedsReload.value) {
    opNeedsReload.value = false
    emit('changed')
  }
}

// ---------- sing-box ----------

/**
 * 装哪一支。
 *
 * **通道由这个动作写入,不是节点表单里的一栏** —— 它描述的是"机器上那个
 * 文件是哪一版",做成可编辑的设置就会多出一个「想要预览版、装的还是
 * 正式版」的状态,而那个状态下 Snell 入口保存得进去、部署到一半失败并回滚。
 */
function installSingBox(channel: SingBoxChannel) {
  const preview = channel === 'PREVIEW'
  const label = preview ? '预览版' : '正式版'
  void runOp(`安装 sing-box(${label})`, '正在上传二进制并写入服务定义', async () => {
    const r = await api.installNode(props.node.id, channel)
    opSteps.value = [
      `二进制已就位:${r.binary_path}(${label})`,
      `服务定义:${r.service_name}(${r.init_system})`,
    ]
    // 改了别人机器上的 sshd 就必须说出来,而且要说清改了什么、
    // 用的是 reload 还是 restart。悄悄改完再报一句"安装完成"是不能接受的。
    if (r.tcp_forwarding?.changed) {
      opSteps.value.push('已顺带打开节点的 SSH TCP 转发:' + r.tcp_forwarding.detail)
      opSteps.value.push(
        '面板读流量、实测 REALITY 握手目标、部署时拨测都要经这条通道,' +
          '原先它被 sshd 挡着。只加了一行 AllowTcpForwarding yes,' +
          '用 reload 而不是 restart,没有断开任何已有连接。',
      )
    }
    opNote.value = preview
      ? '二进制换了,但**服务还在跑旧的那一个**——要点一次「下发配置」' +
        '重启 sing-box 才真的切过去。之后新增入口时就能选 Snell 了。'
      : '接下来点「下发配置」把这台机器的入口配置推上去。'
  })
}

/**
 * 切到预览版之前先把代价说清楚。
 *
 * 装正式版不弹这个:那是默认的、也是绝大多数机器该待的地方。
 * 而"换一个预览版的二进制上去"是管理员要为这台机器单独做的决定。
 */
function confirmInstallPreview() {
  lbDangerConfirm({
    title: `在 ${nodeLabel.value} 上装预览版 sing-box?`,
    okType: 'primary',
    okText: '装预览版',
    impacts: [
      '预览版是上游的 **rc / beta**,不是打了 tag 的正式版。',
      '**Snell 入口只能建在装了预览版的机器上** —— 这是装它的唯一理由。',
      '同一份配置下实测常驻内存 **30.4MB**(正式版 22.4MB),128MB 的机器要留意。',
      'VLESS 与 Shadowsocks 入口**照常工作,配置一个字节都不用改** ——' +
        '实测正式版渲染出来的配置在预览版上跑起来零告警。',
      '装完之后要再点一次「下发配置」重启 sing-box 才真的换过去,' +
        '那一下会断开这台机器上**全部入口**的在线连接。',
    ],
    footer:
      '想换回正式版:先把这台机器上的 Snell 入口删掉或改成别的协议,再点「安装(正式版)」。' +
      '留着 Snell 入口装回去会被拦下 —— 那台机器的整份配置会渲染不出来。',
    onOk: () => {
      void installSingBox('PREVIEW')
    },
  })
}

function uninstallSingBox() {
  uninstallKind.value = 'singbox'
}

function deploySingBox() {
  confirmDeployNode(props.node, () =>
    runOp('下发 sing-box 配置', '正在下发并做健康检查(约 15~25 秒)', async () => {
      opDeploy.value = await api.deployNode(props.node.id)
      if (opDeploy.value.status !== 'SUCCESS') {
        opError.value = opDeploy.value.error_message || '下发未成功'
      }
    }),
  )
}

function restartSingBox() {
  confirmRestartNode(props.node, () =>
    runOp('重启 sing-box', '正在重启服务', async () => {
      await api.restartNode(props.node.id)
      opSteps.value = ['服务已重启']
      // 重启不同步流量,这一点必须说 —— 计数器随进程消失,
      // 未同步窗口内的那一段永久丢失。常规变更该走「下发配置」。
      opNote.value =
        '重启**不会先同步流量**:计数器随进程消失,未同步的那一段永久丢失。' +
        '常规变更请用「下发配置」,它的第一步就是强制同步。'
    }),
  )
}

// ---------- Mieru ----------

function installMieru() {
  void runOp('安装 Mieru', '正在确认 unshare 并上传 mita/mieru', async () => {
    const r = await api.installMieru(props.node.id)
    opSteps.value = [
      `mita:${r.result.mita_path}`,
      `mieru 客户端:${r.result.client_path}(部署时的真实拨测要用它)`,
      `已在节点上跑通:mita ${r.result.mita_version}`,
    ]
    opNote.value = '接下来在每个 Mieru 入口那一行点「下发」—— 一个入口一个 mita 实例。'
  })
}

function uninstallMieru() {
  uninstallKind.value = 'mieru'
}

// ---------- nginx ----------

function installNginx() {
  void runOp('安装 nginx', '正在安装 nginx 与 stream 模块', async () => {
    const r = await api.installNginx(props.node.id)
    opSteps.value = r.result.steps
    opError.value = r.error ?? ''
    if (!r.error) {
      opNote.value = '接下来点「下发转发配置」。它只 reload,在途连接一条不断。'
    }
    await checkNginx()
  })
}

function uninstallNginx() {
  uninstallKind.value = 'nginx'
}

// ---------- realm(V15) ----------
//
// 与 nginx 那一组平行,但**下发与重启都是 restart**:realm 没有 reload,
// 这台机器上全部 realm 线路的在途连接都会断 —— 所以它们走 lbDangerConfirm,
// 而 nginx 的下发停在普通确认档。

/** 与 checkNginx 同理:手工触发的只读探测,不在打开面板时自动跑。 */
async function checkRealm() {
  running.value = '正在探测 realm'
  emit('busy', running.value)
  realmError.value = ''
  try {
    realm.value = await api.nodeRealm(props.node.id)
  } catch (e) {
    realm.value = null
    realmError.value = e instanceof ApiError ? e.message : '探测失败'
  } finally {
    running.value = ''
    emit('busy', '')
  }
}

function installRealm() {
  void runOp('安装 realm', '正在上传 realm 二进制', async () => {
    const r = await api.installRealm(props.node.id)
    opSteps.value = r.result.steps
    opError.value = r.error ?? ''
    if (!r.error) {
      opNote.value = '接下来点「下发 realm 配置」。注意它是 restart,会断开这台机器上全部 realm 线路的在途连接。'
    }
    await checkRealm()
  })
}

function uninstallRealm() {
  uninstallKind.value = 'realm'
}

const realmRuleCount = computed(() => relays.value.filter((r) => r.engine === 'REALM').length)

function deployRealmNow() {
  lbDangerConfirm({
    title: `下发 ${nodeLabel.value} 上的 realm 配置?`,
    impacts: [
      'realm 没有 reload:这一步会 restart 它,' +
        `这台机器上全部 ${realmRuleCount.value} 条 realm 线路的在途连接都会断开。`,
      '下发后做三步健康检查(服务状态、端口监听、经转发的真实拨测),失败自动回滚到上一版。',
      'nginx 转发、sing-box 与 Mieru 入口不受影响。',
    ],
    okText: '下发并重启 realm',
    onOk: () => {
      void runOp('下发 realm 配置', '正在下发并做健康检查', async () => {
        const r = await api.deployRealm(props.node.id)
        opDeploy.value = r.result
        opError.value = r.error ?? ''
      })
    },
  })
}

function restartRealm() {
  lbDangerConfirm({
    title: `重启 ${nodeLabel.value} 上的 realm?`,
    impacts: [
      `这台机器上全部 ${realmRuleCount.value} 条 realm 线路的在途连接都会断开。`,
      '不重新渲染配置:节点上跑的仍是上一次下发的那份。改了规则要用「下发 realm 配置」。',
    ],
    okText: '重启 realm',
    onOk: () => {
      void runOp('重启 realm', '正在重启并确认状态', async () => {
        const r = await api.restartRealm(props.node.id)
        opSteps.value = r.result.steps
        opError.value = r.error ?? ''
        await checkRealm()
      })
    },
  })
}

function stopRealm() {
  lbDangerConfirm({
    title: `停止 ${nodeLabel.value} 上的 realm?`,
    impacts: [
      `这台机器上全部 ${realmRuleCount.value} 条 realm 线路立刻中断,直到再次「下发」或「重启」。`,
      '规则、二进制与配置都留着 —— 巡检看到它没在跑会把它拉起来,想长期停掉请把规则停用或卸载 realm。',
    ],
    okText: '停止 realm',
    onOk: () => {
      void runOp('停止 realm', '正在停止服务', async () => {
        const r = await api.stopRealm(props.node.id)
        opSteps.value = r.result.steps
        opError.value = r.error ?? ''
        await checkRealm()
      })
    },
  })
}

const mieruChainOpen = ref(false)
const mieruChainTarget = ref<MieruInbound | null>(null)

function openMieruChain(m: MieruInbound) {
  mieruChainTarget.value = m
  mieruChainOpen.value = true
}

/**
 * 下发一个 Mieru 入口。
 *
 * **逐入口下发,不是整台机器一起。** 一台机器上的每个 Mieru 入口都是一个
 * 独立的 mita 实例(出口是实例级的,这是上游的限制),重启一个不影响另一个
 * —— 合成"下发这台机器的 Mieru"会把本来只该断一个入口的连接扩大到全部。
 */
function deployMieru(m: MieruInbound) {
  // 带出口的入口会**顺带准备本机 sing-box 那一跳**,而那一下可能重启
  // sing-box —— 会踢掉这台机器上全部 sing-box 入口的在线连接。
  // 自动做是对的(那一步没有任何判断余地),但绝不能不说:
  // 管理员是冲着"只动这一个 Mieru 入口"来点这个按钮的。
  const chained = !!m.chain_target_kind
  lbDangerConfirm({
    title: `确认下发 Mieru 入口「${m.display_name}」?`,
    okType: 'primary',
    okText: '开始下发',
    impacts: [
      ...(chained
        ? [
            '这个入口配了出口,而出口要经**本机 sing-box 的一个回环 socks 入站**' +
              '转一跳(mita 的出口代理只认 SOCKS5)。面板会先把那一跳准备好:',
            '· 这台机器上没有 sing-box 的话,**自动安装**;',
            '· 它的配置没同步的话,**自动下发并重启 sing-box** ——' +
              '这台机器上其他 sing-box 入口的在线连接会一起断开;',
            '· 然后确认那个回环端口真的在监听。',
            '**落地那一台不会被自动部署** —— 那是另一台机器,重启它会断掉' +
              '它上面全部用户。它没就绪时这次下发会被拦下并告诉你去部署哪一台。',
          ]
        : []),
      `会重启 ${nodeLabel.value} 上这一个 mita 实例,把**这个入口**的在线连接全部踢掉。`,
      '同机的其他 Mieru 入口一条连接都不断 —— 它们是各自独立的进程。',
      '下发前会先同步这个实例的流量:计数器随进程消失,不先同步的话那一段永久丢失。',
    ],
    footer: chained
      ? '两跳会分开验:先单独验「本机 sing-box → 落地」那一段(不经 mita),' +
        '再验完整链路 —— 失败时看得出是哪一半断的。'
      : '失败会自动回滚到上一份配置,并把 mita 的日志带回来。',
    // 不返回 Promise:AntD 会把确认框留在屏幕上转圈等它,而下发要十几秒
    // —— 那期间进度弹窗已经开着,两个 Modal 同层叠着,后开的反被压住。
    onOk: () => {
      void runOp(
        `下发 Mieru 入口「${m.display_name}」`,
        chained ? '正在准备本机 sing-box 那一跳,然后下发' : '正在下发并做健康检查',
        async () => {
          const r = await api.deployMieruInbound(m.id)
          opDeploy.value = r.result
          opError.value = r.error ?? ''
        },
      )
    },
  })
}

/** 把一次异步操作包上 busy 标记 —— 面板里每个动作都要抬起它。 */
function runWithBusy(fn: () => Promise<void>) {
  running.value = '正在处理'
  emit('busy', running.value)
  void fn().finally(() => {
    running.value = ''
    emit('busy', '')
  })
}

// ---------------------------------------------------------------- 握手目标

const destOpen = ref(false)
const destTarget = ref<NodeInbound | null>(null)

function openDest(i: NodeInbound) {
  destTarget.value = i
  destOpen.value = true
}

// ---------------------------------------------------------------- 出口(链式)

const chainOpen = ref(false)
const chainTarget = ref<NodeInbound | null>(null)

function openChain(i: NodeInbound) {
  chainTarget.value = i
  chainOpen.value = true
}

// ---------------------------------------------------------------- nginx 转发

/**
 * nginx 透传的落地候选与链式的不是同一组。
 *
 * nginx stream 只渲染 TCP 的 server 块,而 Hysteria2 / TUIC 是纯 UDP ——
 * 它们照常进订阅给用户直连,只是不能当透传落地。两个谓词在后端由
 * DialableByNode / RelayableByNginx 分别回答,前端也必须分开用:
 * 混成一个的话,不能拨的会出现在转发的下拉里,而部署要到十几秒后才失败。
 */
const relayableProxies = computed(() => externalProxies.value.filter((p) => p.relayable))
/** realm 同时搬 UDP,Hysteria2 / TUIC 也能当落地 —— 但拨测测不了它们,部署记录里会记 SKIPPED。 */
const realmTargetProxies = computed(() => externalProxies.value)
const targetProxies = computed(() =>
  form.value.engine === 'REALM' ? realmTargetProxies.value : relayableProxies.value,
)
const hiddenRelayTargets = computed(
  () => externalProxies.value.length - relayableProxies.value.length,
)

const editing = ref<NodeRelay | null>(null)
const formOpen = ref(false)
const form = ref({
  engine: 'NGINX' as 'NGINX' | 'REALM',
  display_name: '',
  listen_port: 0,
  public_port: 0,
  target_kind: 'INBOUND' as 'INBOUND' | 'EXTERNAL' | 'ADDRESS',
  target_inbound_id: 0,
  target_external_id: 0,
  target_host: '',
  target_port: 0,
  access_tier_id: 0,
  sort_order: 0,
  subscription_enabled: true,
  enabled: true,
  public_remark: '',
})

/**
 * nginx 现状是**手工触发**的只读探测。
 *
 * 不在打开面板时自动跑:那要连一次 SSH,而管理员多半只是来看一眼规则列表。
 * 与「仪表盘节点卡片不主动触发 SSH 采集」是同一条道理。
 */
async function checkNginx() {
  running.value = '正在探测 nginx'
  emit('busy', running.value)
  nginxError.value = ''
  try {
    nginx.value = await api.nodeNginx(props.node.id)
  } catch (e) {
    nginx.value = null
    nginxError.value = e instanceof ApiError ? e.message : '探测失败'
  } finally {
    running.value = ''
    emit('busy', '')
  }
}

// V16:中转规则的订阅地址条目(单端口)。nodeAddresses 是这台机器的地址池。
const nodeAddresses = ref<NodeAddress[]>([])
const relayEndpoints = ref<InboundEndpointInput[]>([])

async function loadNodeAddresses() {
  try {
    nodeAddresses.value = (await api.nodeAddresses(props.node.id)).items
  } catch {
    nodeAddresses.value = []
  }
}

function openCreate(engine: 'NGINX' | 'REALM' = 'NGINX') {
  editing.value = null
  relayEndpoints.value = []
  void loadNodeAddresses()
  form.value = {
    engine,
    display_name: '',
    listen_port: 0,
    public_port: 0,
    target_kind: 'INBOUND',
    target_inbound_id: landingInbounds.value[0]?.value ?? 0,
    target_external_id: relayableProxies.value[0]?.id ?? 0,
    target_host: '',
    target_port: 0,
    access_tier_id: props.tiers[0]?.id ?? 1,
    sort_order: 0,
    subscription_enabled: true,
    enabled: true,
    public_remark: '',
  }
  formOpen.value = true
}

async function openEdit(r: NodeRelay) {
  editing.value = r
  relayEndpoints.value = []
  void loadNodeAddresses()
  // 「指定地址」不进订阅、没有地址条目。其余引擎按种类拉一次。
  if (r.target_kind !== 'ADDRESS') {
    try {
      relayEndpoints.value = (await api.inboundEndpoints(r.engine, r.id)).items.map((e) => ({
        address_id: e.address_id,
        public_port: e.public_port,
        public_port_end: e.public_port_end,
        display_name: e.display_name,
      }))
    } catch {
      relayEndpoints.value = []
    }
  }
  form.value = {
    engine: r.engine,
    display_name: r.display_name,
    listen_port: r.listen_port,
    public_port: r.public_port,
    target_kind: r.target_kind,
    target_inbound_id: r.target_inbound_id,
    target_external_id: r.target_external_id,
    target_host: r.target_host,
    target_port: r.target_port,
    access_tier_id: r.access_tier_id,
    sort_order: r.sort_order,
    subscription_enabled: r.subscription_enabled,
    enabled: r.enabled,
    public_remark: r.public_remark,
  }
  formOpen.value = true
}

async function submitForm() {
  if (!form.value.display_name.trim()) {
    message.warning('请填写线路名称')
    return
  }
  running.value = editing.value ? '正在保存' : '正在新增'
  emit('busy', running.value)
  try {
    let relayID: number
    let engine: 'NGINX' | 'REALM'
    if (editing.value) {
      await api.updateRelay(editing.value.id, { ...form.value })
      relayID = editing.value.id
      engine = editing.value.engine
    } else {
      relayID = (await api.createRelay(props.node.id, { ...form.value })).relay.id
      engine = form.value.engine
    }
    // 「指定地址」不进订阅,没有地址条目要存。其余按引擎(NGINX / REALM)存。
    if (form.value.target_kind !== 'ADDRESS') {
      await api.saveInboundEndpoints(engine, relayID, relayEndpoints.value)
    }
    formOpen.value = false
    await load()
    // 说明「已排队」而不是「已生效」:标脏之后由协调器合并下发,
    // 中间有几秒的静默期。说成已生效会让管理员立刻去客户端上试。
    message.success(
      form.value.engine === 'REALM'
        ? '已保存,realm 配置将在数秒内下发(restart,这台机器上全部 realm 线路的在途连接会断开)'
        : '已保存,中转配置将在数秒内下发(只 reload,不断开在途连接)',
    )
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '保存失败')
  } finally {
    running.value = ''
    emit('busy', '')
  }
}

/**
 * 删除一条规则。
 *
 * 可逆(重建即可)但影响面大 —— 用户手上那条线路会从订阅里消失,
 * 而等级、排序、备注要重配。所以用 lbDangerConfirm 逐条列影响,
 * 不用需要打字的那一档:打字摩擦留给真正不可逆的四个操作。
 */
function removeRelay(r: NodeRelay) {
  lbDangerConfirm({
    title: `删除转发线路「${r.display_name}」?`,
    impacts: [
      `${nodeLabel.value} 上监听 ${r.listen_port} 的转发会被撤掉`,
      '这条线路会从所有用户的订阅里消失,已连上的会话会断开',
      '重建时线路名称、访问等级、排序与备注都要重新填',
    ],
    okText: '删除',
    onOk: async () => {
      running.value = '正在删除'
      emit('busy', running.value)
      try {
        await api.deleteRelay(r.id)
        await load()
        message.success('已删除,中转配置将在数秒内下发')
      } catch (e) {
        message.error(e instanceof ApiError ? e.message : '删除失败')
      } finally {
        running.value = ''
        emit('busy', '')
      }
    },
  })
}

/** 立刻下发。只 reload nginx,在途连接不断 —— 普通确认即可。 */
function deployNow() {
  void runOp('下发 nginx 转发配置', '正在下发并做健康检查', async () => {
    const r = await api.deployRelays(props.node.id)
    opDeploy.value = r.result
    opError.value = r.error ?? ''
    if (!r.error) {
      opNote.value = '只 reload,在途连接一条不断。'
    }
  })
}

onMounted(async () => {
  load()
  await loadTargets()
})
</script>

<template>
  <section class="nr">
    <div class="nr__head">
      <span class="nr__title">入口</span>
      <span class="nr__note">这台机器对外提供的入口。用户连的是这里的端口。</span>
      <span v-if="running" class="nr__running">{{ running }}…</span>
    </div>

    <!-- 按钮按服务分行,一行一类。
         混在一排的话,「部署配置」(重启 sing-box、踢掉全部 sing-box 入口的
         在线连接)、「下发」(只重启一个 mita 实例)与「下发转发配置」
         (只 reload,一条在途连接都不断)会长得一样重,
         而这三件事该不该挑时机完全不同。 -->
    <div v-if="!isRelayHost" class="nr__ops">
      <span class="nr__kind">sing-box</span>
      <a-button size="small" type="primary" :disabled="!!running" @click="openCreateInbound">
        新增入口
      </a-button>
      <a-dropdown-button
        size="small"
        :disabled="!!running"
        @click="installSingBox('STABLE')"
      >
        安装
        <template #overlay>
          <a-menu>
            <a-menu-item @click="installSingBox('STABLE')">安装正式版</a-menu-item>
            <a-menu-item @click="confirmInstallPreview">
              安装预览版(1.14,Snell 入口需要)
            </a-menu-item>
          </a-menu>
        </template>
      </a-dropdown-button>
      <a-button size="small" :disabled="!!running" @click="deploySingBox">下发配置</a-button>
      <a-button size="small" :disabled="!!running" @click="restartSingBox">重启</a-button>
      <a-button size="small" danger :disabled="!!running" @click="uninstallSingBox">卸载</a-button>
      <span class="nr__note">
        <LbStatusTag :meta="channelTag" />
        下发与重启都会重启服务,这台机器上<b>全部 sing-box 入口</b>的在线连接都会断开。
        「下发配置」会先强制同步流量,「重启」不会。
      </span>
    </div>
    <!-- Mieru 单独一行:服务端是 mita,与 sing-box 是两个进程、两套服务定义,
         下发也是逐入口各下各的 —— 每个入口一个 mita 实例。 -->
    <div v-if="!isRelayHost" class="nr__ops">
      <span class="nr__kind">Mieru</span>
      <a-button size="small" :disabled="!!running" @click="openCreateMieru">新增 Mieru 入口</a-button>
      <a-button size="small" :disabled="!!running" @click="installMieru">安装</a-button>
      <a-button size="small" danger :disabled="!!running" @click="uninstallMieru">卸载</a-button>
      <span class="nr__note">
        <b>下发在每一行上,各下各的</b> —— 一个入口一个 mita 实例,重启一个不影响其他。
        「卸载」摘掉的是这台机器上全部实例。
      </span>
    </div>
    <div v-else class="nr__ops">
      <span class="nr__kind">sing-box</span>
      <!-- 中转机上只放二进制,通道对它没有意义:那台机器上没有配置,
           这份二进制只在转发拨测时跑几秒。所以不给它那个下拉。 -->
      <a-button size="small" :disabled="!!running" @click="installSingBox('STABLE')">
        安装(仅二进制)
      </a-button>
      <a-button size="small" danger :disabled="!!running" @click="uninstallSingBox">卸载</a-button>
      <!-- 中转机上不装服务:一个没有配置的 sing-box 只会反复崩溃重启,
           而 supervise-daemon 会让它一直重试。二进制要留 ——
           转发规则的健康检查要用它做真实拨测,只跑那几秒。 -->
      <span class="nr__note">中转机上不装服务,这份二进制只在转发拨测时跑几秒。</span>
    </div>
    <div class="nr__ops">
      <span class="nr__kind">nginx 转发</span>
      <a-button size="small" :disabled="!!running" @click="openCreate('NGINX')">新增转发入口</a-button>
      <a-button size="small" :disabled="!!running" @click="installNginx">安装</a-button>
      <a-button size="small" :loading="!!running" @click="checkNginx">检查</a-button>
      <a-button size="small" :loading="!!running" @click="deployNow">下发转发配置</a-button>
      <a-button size="small" danger :disabled="!!running" @click="uninstallNginx">卸载</a-button>
      <span class="nr__note">
        下发只 reload,在途连接一条不断 —— 不需要挑时机。「卸载」会中断全部转发线路。
      </span>
    </div>
    <!-- realm(V15)单独一行:它是面板下发的单个二进制、独立的服务,
         而且**没有 reload** —— 下发与重启都会断开在途连接,与 nginx 那一行
         的摩擦不同档,合在一起会让管理员对「这一下要不要挑时机」失去判断。 -->
    <div class="nr__ops">
      <span class="nr__kind">realm 转发</span>
      <a-button size="small" :disabled="!!running" @click="openCreate('REALM')">新增 realm 转发</a-button>
      <a-button size="small" :disabled="!!running" @click="installRealm">安装</a-button>
      <a-button size="small" :loading="!!running" @click="checkRealm">检查</a-button>
      <a-button size="small" :disabled="!!running" @click="deployRealmNow">下发 realm 配置</a-button>
      <a-button size="small" :disabled="!!running" @click="restartRealm">重启</a-button>
      <a-button size="small" :disabled="!!running" @click="stopRealm">停止</a-button>
      <a-button size="small" danger :disabled="!!running" @click="uninstallRealm">卸载</a-button>
      <span class="nr__note">
        realm 没有 reload:<b>下发与重启都会断开这台机器上全部 realm 线路的在途连接</b>,要挑时机。
        与 nginx 的区别:二进制由面板下发(不依赖发行版的包)、同时搬 UDP。
      </span>
    </div>

    <!-- nginx 现状。缺 stream 模块在两个发行版上都是默认情况,
         而报错只说 unknown directive "stream",不提缺哪个包。 -->
    <div v-if="nginxError" class="nr__warn">探测 nginx 失败:{{ nginxError }}</div>
    <div v-else-if="nginx" class="nr__nginx">
      <template v-if="!nginx.installed">
        这台机器上还没有 nginx。下发时会自动安装
        <b class="lb-mono">nginx</b> 与 stream 模块。
      </template>
      <template v-else-if="!nginx.stream_available">
        <b>nginx 缺少 stream 模块,当前无法转发。</b>
        下发时会自动安装
        <b class="lb-mono">{{ nginx.missing_package || '对应的 stream 模块包' }}</b
        >。装了 nginx 却没有 stream 是这两个发行版的默认情况,而 nginx 只会报
        <span class="lb-mono">unknown directive "stream"</span>,不会提缺哪个包。
      </template>
      <template v-else>
        {{ nginx.version }} · stream
        {{ nginx.stream_built_in ? '已编译进二进制' : '动态模块' }}
        <span v-if="!nginx.stream_built_in" class="lb-mono">
          {{ nginx.stream_module_path }}
        </span>
      </template>
    </div>

    <div v-if="realmError" class="nr__warn">探测 realm 失败:{{ realmError }}</div>
    <div v-else-if="realm" class="nr__nginx">
      <template v-if="!realm.installed">
        这台机器上还没有 realm。点「安装」由面板上传二进制(约 6MB),
        下发时不会自动装 —— 传一个二进制该由你显式决定。
      </template>
      <template v-else>
        {{ realm.version }} ·
        {{ realm.config_present ? '已下发过配置' : '还没下发过配置' }} ·
        {{ realm.running ? '在跑' : '没在跑' }}
        <span class="lb-mono">{{ realm.state }}</span>
      </template>
    </div>

    <div v-if="loadError" class="nr__warn">{{ loadError }}</div>
    <!-- 判据要把 Mieru 一起算上:只有 Mieru 入口的机器上,用户是连得上的,
         而原来那句「谁都连不上」在那种机器上是错的 —— 一句错误的告警
         会让管理员去修一台本来就好好的机器。 -->
    <div v-else-if="!isRelayHost && !inbounds.length && !mierus.length" class="nr__warn">
      这台机器上一个入口都没有 —— 服务会正常运行,但谁都连不上。
    </div>
    <div v-else-if="!isRelayHost && !inbounds.length" class="nr__hint">
      这台机器上只有 Mieru 入口,没有 sing-box 入口 —— sing-box 会正常运行但不接受连接,
      用户走的是 mita。
    </div>

    <!-- <768 整表换卡片:横向滚动会把「操作」列推到屏幕外,手机上找不到它。 -->
    <div v-if="!loadError && narrow" class="nr__cards">
      <LbRowCard v-for="row in rows" :key="row.key">
        <template #head>
          <span class="nr__kind">{{ kindText(row) }}</span>
          <span class="nr__sb-name">{{ rowName(row) }}</span>
          <LbStatusTag :meta="familyOf(row)" />
          <LbStatusTag v-if="row.kind === 'singbox' && row.inbound.unmetered" :meta="unmeteredMeta" />
          <LbStatusTag v-if="row.kind === 'singbox'" :meta="protocolMeta(row.inbound)" />
          <LbStatusTag v-else-if="row.kind === 'mieru'" :meta="mieruMeta(row.mieru)" />
          <LbStatusTag v-else :meta="readyMeta[row.relay.target_ready ? 'yes' : 'no']" />
        </template>
        <div class="nr__sb-body lb-mono">{{ rowPort(row) }}</div>
        <div class="nr__sb-sub">
          <div>去向:{{ destinationText(row) }}</div>
          <div>{{ rowTier(row) }} · {{ rowSubText(row) }}</div>
        </div>
        <template #foot>
          <template v-if="row.kind === 'singbox'">
            <a-button size="small" type="link" @click="openEditInbound(row.inbound)">编辑</a-button>
            <a-button
              v-if="row.inbound.protocol === 'VLESS_REALITY'"
              size="small"
              type="link"
              @click="openDest(row.inbound)"
            >
              握手目标
            </a-button>
            <a-button size="small" type="link" @click="openChain(row.inbound)">出口</a-button>
            <a-button size="small" type="link" danger @click="removeInbound(row.inbound)">
              删除
            </a-button>
          </template>
          <template v-else-if="row.kind === 'mieru'">
            <a-button size="small" type="link" @click="openEditMieru(row.mieru)">编辑</a-button>
            <a-button size="small" type="link" @click="openMieruChain(row.mieru)">出口</a-button>
            <a-button size="small" type="link" @click="deployMieru(row.mieru)">下发</a-button>
            <a-button size="small" type="link" danger @click="removeMieru(row.mieru)">删除</a-button>
          </template>
          <template v-else>
            <a-button size="small" type="link" @click="openEdit(row.relay)">编辑</a-button>
            <a-button size="small" type="link" danger @click="removeRelay(row.relay)">删除</a-button>
          </template>
        </template>
      </LbRowCard>
    </div>

    <a-table
      v-else-if="!loadError"
      :data-source="rows"
      :loading="loading"
      :pagination="false"
      row-key="key"
      size="small"
    >
      <a-table-column key="kind" title="类型">
        <template #default="{ record }">
          <span class="nr__kind">{{ kindText(record) }}</span>
        </template>
      </a-table-column>
      <a-table-column key="name" title="入口">
        <template #default="{ record }">
          <div class="nr__sb-name">
            {{ rowName(record) }}
            <LbStatusTag :meta="familyOf(record)" />
            <LbStatusTag
              v-if="record.kind === 'singbox' && record.inbound.unmetered"
              :meta="unmeteredMeta"
            />
          </div>
          <span v-if="record.kind === 'singbox'" class="nr__tag lb-mono">
            {{ record.inbound.tag }}
          </span>
        </template>
      </a-table-column>
      <a-table-column key="port" title="端口">
        <template #default="{ record }">
          <span class="lb-mono">{{ rowPort(record) }}</span>
        </template>
      </a-table-column>
      <a-table-column key="dest" title="去向">
        <template #default="{ record }">
          <!-- sing-box 那几行显示的是【已经生效】的协议,不是数据库里的期望值:
               改协议到部署成功之间的窗口里,订阅下发的、用户实际连的还是旧的。 -->
          <LbStatusTag v-if="record.kind === 'singbox'" :meta="protocolMeta(record.inbound)" />
          <!-- Mieru 那几行同理显示【已经生效】的传输层:改了它到下发成功之间,
               用户实际连的还是旧的那一种。 -->
          <LbStatusTag v-else-if="record.kind === 'mieru'" :meta="mieruMeta(record.mieru)" />
          <LbStatusTag v-else :meta="readyMeta[record.relay.target_ready ? 'yes' : 'no']" />
          <div class="nr__sub">{{ destinationText(record) }}</div>
        </template>
      </a-table-column>
      <a-table-column key="tier" title="等级">
        <template #default="{ record }">{{ rowTier(record) }}</template>
      </a-table-column>
      <a-table-column key="state" title="状态">
        <template #default="{ record }">
          <LbStatusTag :meta="enabledMeta[rowEnabled(record) ? 'on' : 'off']" />
          <div class="nr__sub">{{ rowSubText(record) }}</div>
          <div
            v-if="record.kind === 'singbox' && !record.inbound.deployed_protocol"
            class="nr__warn"
          >
            还没上过节点
          </div>
          <!-- Mieru 的判据是 deployed_transport,与 sing-box 看
               deployed_protocol 同一条道理:一台下发过很多次的机器上,
               刚加的这个入口仍然还不存在,而它不该出现在任何人的订阅里。 -->
          <div
            v-else-if="record.kind === 'mieru' && !record.mieru.deployed_transport"
            class="nr__warn"
          >
            还没上过节点
          </div>
        </template>
      </a-table-column>
      <a-table-column key="ops" title="操作">
        <template #default="{ record }">
          <template v-if="record.kind === 'singbox'">
            <a-button size="small" type="link" @click="openEditInbound(record.inbound)">
              编辑
            </a-button>
            <a-button
              v-if="record.inbound.protocol === 'VLESS_REALITY'"
              size="small"
              type="link"
              @click="openDest(record.inbound)"
            >
              握手目标
            </a-button>
            <a-button size="small" type="link" @click="openChain(record.inbound)">出口</a-button>
            <a-button size="small" type="link" danger @click="removeInbound(record.inbound)">
              删除
            </a-button>
          </template>
          <template v-else-if="record.kind === 'mieru'">
            <a-button size="small" type="link" @click="openEditMieru(record.mieru)">
              编辑
            </a-button>
            <!-- 没有「握手目标」:Mieru 不用 REALITY。
                 「出口」有,但那一跳要经本机 sing-box —— mita 只拨得出 SOCKS5。 -->
            <a-button size="small" type="link" @click="openMieruChain(record.mieru)">
              出口
            </a-button>
            <!-- 「下发」逐入口一个,不是整台机器一起:一个入口一个 mita 实例。 -->
            <a-button size="small" type="link" @click="deployMieru(record.mieru)">
              下发
            </a-button>
            <a-button size="small" type="link" danger @click="removeMieru(record.mieru)">
              删除
            </a-button>
          </template>
          <template v-else>
            <a-button size="small" type="link" @click="openEdit(record.relay)">编辑</a-button>
            <a-button size="small" type="link" danger @click="removeRelay(record.relay)">
              删除
            </a-button>
          </template>
        </template>
      </a-table-column>
      <template #emptyText>
        <!-- 只在读取成功且确实是零条时才说「还没有」。读取失败时上面那条
             loadError 会先出现,表格根本不渲染 —— 否则「读不到」会被读成
             「一条都没有」,而两者要做的事完全不同。 -->
        <span class="nr__note">还没有任何入口。用上面的「新增入口」加一条。</span>
      </template>
    </a-table>

    <!-- 订阅展开:一个 sing-box 入口在客户端里会变成一到两条,这里逐条写出来。 -->
    <div v-if="!isRelayHost && inbounds.length" class="nr__sb-sub">
      <div v-for="i in inbounds" :key="i.id">
        <b>{{ i.display_name }}</b> 在订阅里展开成:
        <span v-for="e in subEntriesOf(i)" :key="e.name" class="nr__sb-entry">
          {{ e.name }} <span class="lb-mono">{{ e.addr }}</span>
        </span>
      </div>
      <!-- IPv6 不是第二条节点记录,而是同一条记录在订阅生成时的逻辑展开 ——
           两条指向同一个入站,改 IPv6 保存即生效,不需要重新部署。 -->
      <div v-if="node.ipv6_address">
        IPv6 与 IPv4 两条指向<b>同一个入站</b>,UUID、REALITY 公钥、short ID 完全相同。
        名字与公网端口可以逐入口单独设,其余一律共用 —— 流量也记在一起。
      </div>
    </div>

    <div v-if="!node.subscription_enabled" class="nr__warn">
      这台机器的「下发到用户订阅」是关的,它上面全部入口都不会进入新生成的订阅。
      节点仍在运行,已导入旧订阅的客户端还能用。
    </div>

    <!-- 中转机上没有「部署配置」那个按钮,也不产生任何流量数字 ——
         这两句话照写会指向一个不存在的按钮和一份不存在的统计。 -->
    <p v-if="!isRelayHost" class="nr__note">
      sing-box 入口的增删改<b>不会自动部署</b>:那会重启服务,把这台机器上全部
      sing-box 入口的在线连接一起踢掉。改完之后自己挑时机点上面的「部署配置」。
      Mieru 入口同理,但它的下发在<b>每一行上</b> —— 一个入口一个 mita 实例,
      重启一个不影响其他入口,也不影响 sing-box。
      转发入口只 reload nginx,不需要挑时机。
      <br />
      <b>流量拆不到入口。</b>同一个用户在这台机器上的流量是所有入口的合计 ——
      计数器里没有入站这一维,不是暂时没做。
    </p>
    <p v-else class="nr__note">
      转发入口的增删改只会 <b>reload nginx</b>,在途连接一条不断 —— 不需要挑时机。
      落地未就绪的线路不会出现在任何人的订阅里。
      <br />
      中转主机上跑的是 nginx,它不接流量统计接口,<b>面板不计这台机器的流量</b>。
    </p>

    <!-- 两次部署的结果都要显示:失败时管理员必须知道卡在哪台机器上 -->
    <div v-if="lastChain" class="nr__result">
      <div>停在:<b>{{ lastChain.stage }}</b></div>
      <div v-if="lastChain.target_deploy">落地部署:{{ lastChain.target_deploy.status }}</div>
      <div v-if="lastChain.host_deploy">本机部署:{{ lastChain.host_deploy.status }}</div>
    </div>
    <!-- ---------------- sing-box 入口表单 ---------------- -->
    <InboundFormModal
      v-model:open="inboundOpen"
      :inbound="editingInbound"
      :node="node"
      :tiers="tiers"
      :existing-count="inbounds.length"
      @saved="emit('changed')"
      @busy="(l: string) => emit('busy', l)"
    />

    <!-- ---------------- Mieru 入口表单 ---------------- -->
    <MieruInboundFormModal
      v-model:open="mieruOpen"
      :inbound="editingMieru"
      :node="node"
      :tiers="tiers"
      :existing-count="mierus.length"
      @saved="emit('changed')"
      @busy="(l: string) => emit('busy', l)"
    />

    <!-- 去节点上做事的进度与结果。跑的时候关不掉 —— 这些操作要十几秒,
         随手一点关掉的话结果几秒后才回来、已经没有地方呈现。 -->
    <NodeOpProgressModal
      :open="opOpen"
      @update:open="closeOp"
      :title="opTitle"
      :running="opRunning"
      :steps="opSteps"
      :deploy="opDeploy"
      :error="opError"
      :note="opNote"
    />

    <!-- 卸载是不可逆的那一档,走打字确认。要求输入的是**内部名称** ——
         内部名称唯一,展示名称可以重复。 -->
    <LbNameConfirm
      v-if="uninstallMeta"
      :open="uninstallKind !== null"
      :title="uninstallMeta.title"
      :name="node.name"
      :ok-text="uninstallMeta.okText"
      :impacts="uninstallMeta.impacts"
      prompt="输入内部名称以确认"
      @update:open="(v: boolean) => (uninstallKind = v ? uninstallKind : null)"
      @confirm="doUninstall"
    />

    <MieruChainModal
      v-model:open="mieruChainOpen"
      :inbound="mieruChainTarget"
      :node="node"
      @applied="emit('changed')"
      @busy="(l: string) => emit('busy', l)"
    />

    <InboundDestModal
      v-model:open="destOpen"
      :inbound="destTarget"
      @applied="emit('changed')"
      @busy="(l: string) => emit('busy', l)"
    />

    <InboundChainModal
      v-model:open="chainOpen"
      :inbound="chainTarget"
      :node="node"
      @applied="
        (r: ChainApplyResult | null) => {
          lastChain = r
          emit('changed')
        }
      "
      @busy="(l: string) => emit('busy', l)"
    />

    <!-- ---------------- nginx 转发表单 ---------------- -->
    <a-modal
      v-model:open="formOpen"
      :title="
        (editing ? '编辑' : '新增') + (form.engine === 'REALM' ? ' realm ' : ' nginx ') + '转发线路'
      "
      :confirm-loading="!!running"
      @ok="submitForm"
    >
      <a-form layout="vertical" size="small">
        <a-form-item label="线路名称(会发给用户)">
          <a-input v-model:value="form.display_name" placeholder="例如:香港中转-东京" />
        </a-form-item>
        <a-form-item :label="`监听端口(${form.engine === 'REALM' ? 'realm' : 'nginx'} 在这台机器上真正监听的号码)`">
          <a-input-number v-model:value="form.listen_port" :min="1" :max="65535" />
        </a-form-item>
        <!-- 「指定地址」不进订阅,没有订阅地址条目,只有监听端口。 -->
        <a-form-item
          v-if="form.target_kind !== 'ADDRESS'"
          label="订阅地址(用户连哪几个地址、各自的公网端口与名字)"
        >
          <EndpointsEditor
            v-model="relayEndpoints"
            :addresses="nodeAddresses"
            :host="node.host"
            :is-mieru="false"
            :name-hint="form.display_name"
          />
          <div class="nr__hint">
            这条中转在订阅里下发的每一条地址(管理 IP / 额外 IPv4 / IPv6)。端口留空表示跟随监听端口;
            NAT 主机上两者不同 —— 公网 443 映射到主机 20443 时,监听端口填 20443、这里填 443。
            名字留空表示跟随线路名(IPv6 条目加 -IPV6)。
          </div>
        </a-form-item>
        <a-form-item v-if="!editing" label="转发引擎">
          <a-radio-group v-model:value="form.engine" size="small" button-style="solid">
            <a-radio-button value="NGINX">nginx</a-radio-button>
            <a-radio-button value="REALM">realm</a-radio-button>
          </a-radio-group>
          <div class="nr__hint">
            引擎建好之后不能改。nginx 改规则 reload、在途连接不断;realm 改规则 restart、在途连接全断,
            但它由面板下发二进制、同时搬 UDP(Hysteria2 / TUIC 也能当落地)。
          </div>
        </a-form-item>
        <a-form-item v-if="!editing" label="落地去向">
          <a-radio-group v-model:value="form.target_kind" size="small" button-style="solid">
            <a-radio-button value="INBOUND">自建节点的入口</a-radio-button>
            <a-radio-button value="EXTERNAL">外部代理</a-radio-button>
            <a-radio-button value="ADDRESS">指定地址</a-radio-button>
          </a-radio-group>
          <div class="nr__hint">落地种类建好之后不能改 —— 换种类等于换成另一条线路。</div>
        </a-form-item>
        <a-form-item v-if="form.target_kind === 'ADDRESS'" label="落地地址(IP 或域名)与端口">
          <a-input v-model:value="form.target_host" placeholder="例如:203.0.113.9 或 landing.example.com" style="width: 60%" />
          <a-input-number v-model:value="form.target_port" :min="1" :max="65535" style="margin-left: 8px" />
          <div class="nr__hint">
            <b>这种线路不进订阅、不进门户,也不做拨测</b> —— 面板不知道那个地址背后跑的是什么协议、凭据是什么,
            造不出条目。它的用途是把这台机器当纯端口转发器,用户拿到的地址由你另行分发。
            域名原样写进配置、由转发程序自己解析,面板不解析它。
          </div>
        </a-form-item>
        <a-form-item v-else label="落地">
          <a-select
            v-if="form.target_kind === 'INBOUND'"
            v-model:value="form.target_inbound_id"
            :options="landingInbounds"
          />
          <a-select
            v-else
            v-model:value="form.target_external_id"
            :disabled="!targetProxies.length"
            :options="targetProxies.map((p) => ({ value: p.id, label: p.display_name }))"
          />
          <div class="nr__hint">
            落地是一个<b>入口</b>而不是一台机器 —— 一台机器上有两个入口时,
            「转发到它」是有歧义的,而流量进错入口不会有任何报错。
            <template v-if="form.target_kind === 'EXTERNAL' && form.engine === 'NGINX' && hiddenRelayTargets">
              <br />
              另有 {{ hiddenRelayTargets }} 条外部代理走 QUIC(Hysteria2 / TUIC),
              是纯 UDP,而 nginx 透传只搬 TCP 字节 —— 没有列出来。
              给它们配转发的话,规则下发得下去,但用户永远连不上,面板还全绿。
            </template>
          </div>
        </a-form-item>
        <a-form-item v-if="form.target_kind !== 'ADDRESS'" label="访问等级">
          <a-select
            v-model:value="form.access_tier_id"
            :options="tiers.map((t) => ({ value: t.id, label: t.name }))"
          />
          <div class="nr__hint">
            等级不高于它的用户才看得到这条线路。它与落地那个入口的等级是
            <b>两道独立的闸门</b> —— 用户必须同时过得了两边,否则会拿到一条
            订阅里看得见、连上去握手被拒的条目。
          </div>
        </a-form-item>
        <a-form-item label="排序">
          <a-input-number v-model:value="form.sort_order" />
        </a-form-item>
        <a-form-item label="对用户公开的备注">
          <a-input v-model:value="form.public_remark" />
        </a-form-item>
        <a-form-item>
          <a-checkbox v-model:checked="form.enabled">启用(关掉后不再监听这个端口)</a-checkbox>
          <a-checkbox v-if="form.target_kind !== 'ADDRESS'" v-model:checked="form.subscription_enabled">
            下发到订阅
          </a-checkbox>
        </a-form-item>
      </a-form>
    </a-modal>
  </section>
</template>

<style scoped>
.nr {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 12px;
  border: 1px solid #E3E6EA;
  border-radius: 8px;
  margin-bottom: 12px;
}
.nr__head,
.nr__ops {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

/* 一类操作一行。两类挤在一排的话,「部署配置」与「下发转发配置」
   会长得一样重,而前者重启服务、后者只 reload。 */
.nr__ops {
  padding: 6px 8px;
  border: 1px solid #E3E6EA;
  border-radius: 6px;
  background: #fafbfc;
}

.nr__cards {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.nr__title {
  font-weight: 600;
}

/* 入口种类标记。中性底色 —— 它描述的是"这是哪一类入口",不是状态,
   上色会跟旁边真正表达状态的 LbStatusTag 抢注意力。取值来自 tokens.ts。 */
.nr__kind {
  display: inline-block;
  padding: 0 6px;
  border-radius: 3px;
  background: #f1f3f5;
  color: #576070;
  font-size: 11px;
  letter-spacing: 0.02em;
}

/* 入站 tag。它是 sing-box 配置与流量计数器里的标识,排查时要用,
   但对日常操作没有意义 —— 所以弱化,不与名字抢注意力。 */
.nr__tag {
  font-size: 11px;
  color: #6B7480;
}

.nr__section {
  display: flex;
  align-items: baseline;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 4px;
}

/* sing-box 入口块。用一层浅底把它与下面的转发列表分开 ——
   两者的操作摩擦不同(这一块改了要重启,下面改了只 reload),
   长得一样会让人对"点这一下要不要挑时机"失去判断。 */
.nr__sb {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 10px 12px;
  border: 1px solid #E3E6EA;
  border-radius: 6px;
  background: #fafbfc;
}
.nr__sb-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.nr__sb-name {
  font-weight: 600;
}
.nr__sb-body {
  font-size: 12px;
}
.nr__sb-sub {
  font-size: 12px;
  color: #6B7480;
  line-height: 1.7;
}
.nr__sb-entry {
  margin-right: 10px;
}
.nr__spacer {
  flex: 1;
}
.nr__running {
  font-size: 12px;
  color: #6B7480;
}
.nr__note,
.nr__hint {
  font-size: 12px;
  color: #6B7480;
  margin: 0;
  line-height: 1.6;
}
.nr__warn {
  font-size: 12px;
  color: #B4291D;
}
.nr__nginx {
  font-size: 12px;
  line-height: 1.6;
}
.nr__sub {
  font-size: 12px;
  color: #6B7480;
  margin-left: 6px;
}
.nr__result {
  font-size: 12px;
  line-height: 1.7;
  border-top: 1px dashed #E3E6EA;
  padding-top: 8px;
}
.nr__step {
  display: flex;
  gap: 8px;
}
.nr__step-name {
  min-width: 180px;
}
.nr__step-detail {
  color: #6B7480;
  word-break: break-all;
}
</style>
