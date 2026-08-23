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
  type Node,
  type MieruInbound,
  type NodeInbound,
  type NodeRelay,
} from '@/api/client'
import { LbRowCard, LbStatusTag, lbDangerConfirm, type LbStatusMeta } from '@/components/lb'
import InboundChainModal from './InboundChainModal.vue'
import InboundDestModal from './InboundDestModal.vue'
import InboundFormModal from './InboundFormModal.vue'
import MieruChainModal from './MieruChainModal.vue'
import MieruInboundFormModal from './MieruInboundFormModal.vue'
import {
  addressFamilyMeta,
  confirmRemoveInbound,
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
  /**
   * 部署与安装由页面执行,面板只负责把按钮放在管理员正在看的地方。
   *
   * 不在这里各写一遍:部署要走 lbDangerConfirm、开进度弹窗、失败后还要
   * 展示步骤明细,而那一整套已经在页面上了。两处各一份的话,某天改了
   * 确认文案只改到一处,而两处点下去做的是同一件事。
   */
  deploy: []
  install: []
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
const lastDeploy = ref<DeployResult | null>(null)
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

// key 的前缀不能省:三张表各自从 1 开始,不加前缀的话 i1 / m1 / r1
// 会撞成同一个 key,而 Vue 会把三行当成同一行来复用 DOM。
const rows = computed<EntryRow[]>(() => [
  ...inbounds.value.map((i) => ({ key: `i${i.id}`, kind: 'singbox' as const, inbound: i })),
  ...mierus.value.map((m) => ({ key: `m${m.id}`, kind: 'mieru' as const, mieru: m })),
  ...relays.value.map((r) => ({ key: `r${r.id}`, kind: 'nginx' as const, relay: r })),
])

const kindLabel: Record<EntryRow['kind'], string> = {
  singbox: 'sing-box',
  mieru: 'Mieru',
  nginx: 'nginx 转发',
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
  if (row.kind === 'nginx') {
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
  lbDangerConfirm({
    title: `确认删除 Mieru 入口「${m.display_name}」?`,
    okText: '确认删除',
    impacts: [
      `它会从 ${nodeLabel.value} 的订阅里立刻消失,新拉订阅的人看不到它。`,
      '但它在节点上仍然跑着,已经拿到订阅的人照常能连 —— 直到下一次下发这台机器。',
      '下发时会重启 mita,把这台机器上全部 Mieru 连接一起踢掉(不影响 sing-box 入口)。',
    ],
    footer: '这台机器上别的入口不受影响。',
    onOk: () =>
      runWithBusy(async () => {
        await api.deleteMieruInbound(m.id)
        message.success('已删除。下次下发这台机器时才会从节点上真正移除')
        emit('changed')
      }),
  })
}

const mieruChainOpen = ref(false)
const mieruChainTarget = ref<MieruInbound | null>(null)

function openMieruChain(m: MieruInbound) {
  mieruChainTarget.value = m
  mieruChainOpen.value = true
}

/**
 * 装 mita 与 mieru 客户端二进制。
 *
 * **与「安装 sing-box」是两件事,按钮也分行。** 两者装的是不同的二进制、
 * 不同的服务定义,而且这一步还要先确认机器上有 unshare ——
 * 每个 mita 实例都要一个独立的挂载命名空间,不然它们会共用
 * /var/lib/mita/metrics.pb,互相覆盖对方的流量计数,而三个实例都"正常运行"。
 *
 * 只装二进制、不起任何服务:服务定义由每个入口自己的下发写。
 */
async function installMieru() {
  running.value = '正在安装 Mieru'
  emit('busy', running.value)
  try {
    const r = await api.installMieru(props.node.id)
    message.success(`已安装 mita ${r.result.mita_version}`)
    emit('changed')
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '安装失败')
  } finally {
    running.value = ''
    emit('busy', '')
  }
}

const lastMieruDeploy = ref<DeployResult | null>(null)

/**
 * 下发一个 Mieru 入口。
 *
 * **逐入口下发,不是整台机器一起。** 一台机器上的每个 Mieru 入口都是一个
 * 独立的 mita 实例(出口是实例级的,这是上游的限制),重启一个不影响另一个
 * —— 合成"下发这台机器的 Mieru"会把本来只该断一个入口的连接扩大到全部。
 */
function deployMieru(m: MieruInbound) {
  lbDangerConfirm({
    title: `确认下发 Mieru 入口「${m.display_name}」?`,
    okType: 'primary',
    okText: '开始下发',
    impacts: [
      `会重启 ${nodeLabel.value} 上这一个 mita 实例,把**这个入口**的在线连接全部踢掉。`,
      '同机的其他 Mieru 入口与 sing-box 入口一条连接都不断 —— 它们是各自独立的进程。',
      '下发前会先同步这个实例的流量:计数器随进程消失,不先同步的话那一段永久丢失。',
    ],
    footer: '失败会自动回滚到上一份配置,并把 mita 的日志带回来。',
    onOk: () => {
      // 不返回这个 Promise:AntD 会把确认框留在屏幕上转圈等它,
      // 而下发要十几秒 —— 那期间管理员既看不到进度也点不了别的。
      void (async () => {
        running.value = '正在下发 Mieru 入口'
        emit('busy', running.value)
        lastMieruDeploy.value = null
        try {
          const r = await api.deployMieruInbound(m.id)
          lastMieruDeploy.value = r.result
          if (r.error) {
            message.error(r.error)
          } else {
            message.success('Mieru 入口已下发')
          }
          emit('changed')
        } catch (e) {
          message.error(e instanceof ApiError ? e.message : '下发失败')
        } finally {
          running.value = ''
          emit('busy', '')
        }
      })()
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
const hiddenRelayTargets = computed(
  () => externalProxies.value.length - relayableProxies.value.length,
)

const editing = ref<NodeRelay | null>(null)
const formOpen = ref(false)
const form = ref({
  display_name: '',
  listen_port: 0,
  public_port: 0,
  target_kind: 'INBOUND' as 'INBOUND' | 'EXTERNAL',
  target_inbound_id: 0,
  target_external_id: 0,
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

function openCreate() {
  editing.value = null
  form.value = {
    display_name: '',
    listen_port: 0,
    public_port: 0,
    target_kind: 'INBOUND',
    target_inbound_id: landingInbounds.value[0]?.value ?? 0,
    target_external_id: relayableProxies.value[0]?.id ?? 0,
    access_tier_id: props.tiers[0]?.id ?? 1,
    sort_order: 0,
    subscription_enabled: true,
    enabled: true,
    public_remark: '',
  }
  formOpen.value = true
}

function openEdit(r: NodeRelay) {
  editing.value = r
  form.value = {
    display_name: r.display_name,
    listen_port: r.listen_port,
    public_port: r.public_port,
    target_kind: r.target_kind,
    target_inbound_id: r.target_inbound_id,
    target_external_id: r.target_external_id,
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
    if (editing.value) {
      await api.updateRelay(editing.value.id, { ...form.value })
    } else {
      await api.createRelay(props.node.id, { ...form.value })
    }
    formOpen.value = false
    await load()
    // 说明「已排队」而不是「已生效」:标脏之后由协调器合并下发,
    // 中间有几秒的静默期。说成已生效会让管理员立刻去客户端上试。
    message.success('已保存,中转配置将在数秒内下发(只 reload,不断开在途连接)')
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
async function deployNow() {
  running.value = '正在下发中转配置'
  emit('busy', running.value)
  lastDeploy.value = null
  try {
    const r = await api.deployRelays(props.node.id)
    lastDeploy.value = r.result
    if (r.error) {
      message.error(r.error)
    } else {
      message.success('中转配置已下发')
    }
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '下发失败')
  } finally {
    running.value = ''
    emit('busy', '')
  }
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
      <a-button size="small" :disabled="!!running" @click="emit('install')">安装 sing-box</a-button>
      <a-button size="small" :disabled="!!running" @click="emit('deploy')">部署配置</a-button>
      <span class="nr__note">
        改了要重启,这台机器上<b>全部 sing-box 入口</b>的在线连接都会断开。
      </span>
    </div>
    <!-- Mieru 单独一行:服务端是 mita,与 sing-box 是两个进程、两套服务定义,
         下发也是逐入口各下各的 —— 每个入口一个 mita 实例。 -->
    <div v-if="!isRelayHost" class="nr__ops">
      <span class="nr__kind">Mieru</span>
      <a-button size="small" :disabled="!!running" @click="openCreateMieru">新增 Mieru 入口</a-button>
      <a-button size="small" :disabled="!!running" @click="installMieru">安装 Mieru</a-button>
      <span class="nr__note">
        下发在每一行上,<b>各下各的</b> —— 一个入口一个 mita 实例,重启一个不影响其他。
      </span>
    </div>
    <div v-else class="nr__ops">
      <span class="nr__kind">sing-box</span>
      <a-button size="small" :disabled="!!running" @click="emit('install')">
        安装 sing-box(仅二进制)
      </a-button>
      <!-- 中转机上不装服务:一个没有配置的 sing-box 只会反复崩溃重启,
           而 supervise-daemon 会让它一直重试。二进制要留 ——
           转发规则的健康检查要用它做真实拨测,只跑那几秒。 -->
      <span class="nr__note">中转机上不装服务,这份二进制只在转发拨测时跑几秒。</span>
    </div>
    <div class="nr__ops">
      <span class="nr__kind">nginx 转发</span>
      <a-button size="small" :disabled="!!running" @click="openCreate">新增转发入口</a-button>
      <a-button size="small" :loading="!!running" @click="checkNginx">检查 nginx</a-button>
      <a-button size="small" :loading="!!running" @click="deployNow">下发转发配置</a-button>
      <span class="nr__note">只 reload,在途连接一条不断 —— 不需要挑时机。</span>
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
    <div v-if="lastMieruDeploy" class="nr__result">
      <div>Mieru 下发:<b>{{ lastMieruDeploy.status }}</b></div>
      <div v-for="(s, i) in lastMieruDeploy.steps" :key="i" class="nr__step">
        <span class="nr__step-name">{{ s.name }}</span>
        <span class="nr__step-status">{{ s.status }}</span>
        <span class="nr__step-detail">{{ s.detail }}</span>
      </div>
    </div>
    <div v-if="lastDeploy" class="nr__result">
      <div>中转下发:<b>{{ lastDeploy.status }}</b></div>
      <div v-for="(s, i) in lastDeploy.steps" :key="i" class="nr__step">
        <span class="nr__step-name">{{ s.name }}</span>
        <span class="nr__step-status">{{ s.status }}</span>
        <span class="nr__step-detail">{{ s.detail }}</span>
      </div>
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
      :title="editing ? '编辑转发线路' : '新增转发线路'"
      :confirm-loading="!!running"
      @ok="submitForm"
    >
      <a-form layout="vertical" size="small">
        <a-form-item label="线路名称(会发给用户)">
          <a-input v-model:value="form.display_name" placeholder="例如:香港中转-东京" />
        </a-form-item>
        <a-form-item label="监听端口(nginx 在这台机器上真正监听的号码)">
          <a-input-number v-model:value="form.listen_port" :min="1" :max="65535" />
        </a-form-item>
        <a-form-item label="公网端口(留 0 表示与监听端口相同)">
          <a-input-number v-model:value="form.public_port" :min="0" :max="65535" />
          <div class="nr__hint">
            NAT 主机上两者不同:公网 443 映射到主机的 20443 时,监听端口填 20443、公网端口填
            443。填反了 nginx 会监听在转发链路另一端的号码上,而各项检查都会通过。
          </div>
        </a-form-item>
        <a-form-item v-if="!editing" label="落地去向">
          <a-radio-group v-model:value="form.target_kind" size="small" button-style="solid">
            <a-radio-button value="INBOUND">自建节点的入口</a-radio-button>
            <a-radio-button value="EXTERNAL">外部代理</a-radio-button>
          </a-radio-group>
          <div class="nr__hint">落地种类建好之后不能改 —— 换种类等于换成另一条线路。</div>
        </a-form-item>
        <a-form-item label="落地">
          <a-select
            v-if="form.target_kind === 'INBOUND'"
            v-model:value="form.target_inbound_id"
            :options="landingInbounds"
          />
          <a-select
            v-else
            v-model:value="form.target_external_id"
            :disabled="!relayableProxies.length"
            :options="relayableProxies.map((p) => ({ value: p.id, label: p.display_name }))"
          />
          <div class="nr__hint">
            落地是一个<b>入口</b>而不是一台机器 —— 一台机器上有两个入口时,
            「转发到它」是有歧义的,而流量进错入口不会有任何报错。
            <template v-if="form.target_kind === 'EXTERNAL' && hiddenRelayTargets">
              <br />
              另有 {{ hiddenRelayTargets }} 条外部代理走 QUIC(Hysteria2 / TUIC),
              是纯 UDP,而 nginx 透传只搬 TCP 字节 —— 没有列出来。
              给它们配转发的话,规则下发得下去,但用户永远连不上,面板还全绿。
            </template>
          </div>
        </a-form-item>
        <a-form-item label="访问等级">
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
          <a-checkbox v-model:checked="form.enabled">启用(关掉后 nginx 上不再监听)</a-checkbox>
          <a-checkbox v-model:checked="form.subscription_enabled">下发到订阅</a-checkbox>
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
