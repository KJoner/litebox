<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  PROTOCOL_LABEL,
  PROTOCOL_SHORT,
  SS_METHOD_LABEL,
  type ChainApplyResult,
  type DeployResult,
  type DestCheckResult,
  type ExternalProxy,
  type NginxFacts,
  type Node,
  type NodeInbound,
  type NodeProtocol,
  type NodeRelay,
  type NodeSSMethod,
} from '@/api/client'
import { LbStatusTag, lbDangerConfirm, type LbStatusMeta } from '@/components/lb'
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
const props = defineProps<{ node: Node }>()
const emit = defineEmits<{
  /** 有动作在跑时抬起,页面据此在弹窗上屏蔽遮罩点击与 ESC */
  busy: [label: string]
  /** 变更之后节点本身可能变了,让页面重新拉一次 */
  changed: []
}>()

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

/**
 * 协议标记读 deployed_protocol —— 节点上【已经生效】的那个。
 *
 * 读期望值的话,改协议到部署成功之间的窗口里这里会显示新协议,
 * 而订阅下发的、用户实际连的还是旧的 —— 界面与事实分叉,
 * 而分叉的那一头恰好是管理员会相信的那一头。
 */
function protocolMeta(i: NodeInbound): LbStatusMeta {
  const p = i.deployed_protocol
  if (!p) {
    return {
      text: '未部署',
      shape: 'ring',
      fg: color.neutral,
      bg: color.neutralBg,
      bd: color.neutralBorder,
    }
  }
  const pending = p !== i.protocol
  return {
    text: pending ? `${PROTOCOL_SHORT[p]} → ${PROTOCOL_SHORT[i.protocol]} 待部署` : PROTOCOL_SHORT[p],
    shape: pending ? 'triangle' : 'check',
    fg: pending ? color.warning : color.success,
    bg: pending ? color.warningBg : color.successBg,
    bd: pending ? color.warningBorder : color.successBorder,
  }
}

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

/** 一个入口在订阅里展开成的条目(IPv4 + 可选 IPv6)。 */
function subEntriesOf(i: NodeInbound) {
  const port = i.public_port || i.listen_port
  const out = [{ name: i.display_name, addr: `${props.node.host}:${port}` }]
  if (props.node.ipv6_address) {
    out.push({
      name: `${i.display_name}-IPV6`,
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

// ---------------------------------------------------------------- sing-box 入口

const inboundOpen = ref(false)
const editingInbound = ref<NodeInbound | null>(null)
const inboundForm = ref({
  display_name: '',
  protocol: 'VLESS_REALITY' as NodeProtocol,
  ss_method: '' as NodeSSMethod | '',
  listen_port: 0,
  public_port: 0,
  ipv6_public_port: 0,
  tcp_fast_open: false,
  access_tier_id: 0,
  sort_order: 0,
  subscription_enabled: true,
  enabled: true,
  public_remark: '',
})

function openCreateInbound() {
  editingInbound.value = null
  inboundForm.value = {
    display_name: '',
    protocol: 'VLESS_REALITY',
    ss_method: '',
    listen_port: 0,
    public_port: 0,
    ipv6_public_port: 0,
    tcp_fast_open: false,
    access_tier_id: props.node.access_tier_id,
    sort_order: inbounds.value.length,
    subscription_enabled: true,
    enabled: true,
    public_remark: '',
  }
  inboundOpen.value = true
}

function openEditInbound(i: NodeInbound) {
  editingInbound.value = i
  inboundForm.value = {
    display_name: i.display_name,
    protocol: i.protocol,
    ss_method: i.ss_method,
    listen_port: i.listen_port,
    public_port: i.public_port,
    ipv6_public_port: i.ipv6_public_port,
    tcp_fast_open: i.tcp_fast_open,
    access_tier_id: i.access_tier_id,
    sort_order: i.sort_order,
    subscription_enabled: i.subscription_enabled,
    enabled: i.enabled,
    public_remark: i.public_remark,
  }
  inboundOpen.value = true
}

/**
 * 切到 VLESS 之前必须先实测过握手目标。
 *
 * 在这里就说出来,而不是等后端拒绝 —— 后端那句话出现在保存失败的红字里,
 * 而管理员此刻正盯着的是协议那个下拉框。
 */
const protocolSwitchBlocked = computed(() => {
  const cur = editingInbound.value
  if (!cur || inboundForm.value.protocol !== 'VLESS_REALITY') return ''
  if (cur.protocol === 'VLESS_REALITY') return ''
  if (!cur.handshake_checked_at) {
    return '这个入口还没有实测过握手目标。REALITY 要求目标返回的每个 TLS 记录不超过 8192 字节,超限时握手会静默失败:客户端连不上,而节点上一切正常。请先用下面的「实测握手目标」跑一次。'
  }
  return ''
})

async function submitInbound() {
  if (!inboundForm.value.display_name.trim()) {
    message.warning('请填写入口名称')
    return
  }
  if (!inboundForm.value.listen_port) {
    message.warning('请填写主机监听端口')
    return
  }
  running.value = editingInbound.value ? '正在保存入口' : '正在新增入口'
  emit('busy', running.value)
  try {
    if (editingInbound.value) {
      await api.updateInbound(editingInbound.value.id, { ...inboundForm.value })
    } else {
      await api.createInbound(props.node.id, { ...inboundForm.value })
    }
    inboundOpen.value = false
    emit('changed')
    // 说「下次部署后生效」而不是「已保存」:自动部署会重启 sing-box,
    // 把这台机器上全部入口的在线连接一起踢掉,而他动的只是其中一个。
    message.success('已保存。入口的变更要重新部署这台机器才会生效')
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '保存失败')
  } finally {
    running.value = ''
    emit('busy', '')
  }
}

/**
 * 删除一个 sing-box 入口。
 *
 * 不可逆的只有四个操作(删用户、删节点、卸载服务、重置主机密钥),
 * 这一个可以重建,所以用 lbDangerConfirm 而不是打字确认 ——
 * 但它的影响面比删一条 nginx 转发大得多,必须逐条列出来。
 */
function removeInbound(i: NodeInbound) {
  lbDangerConfirm({
    title: `删除入口「${i.display_name}」?`,
    impacts: [
      `${nodeLabel.value} 上监听 ${i.listen_port} 的 sing-box 入站会被撤掉`,
      '这一条会从所有用户的订阅里消失;只用这个入口的人会连不上',
      '下次部署这台机器时才真正生效 —— 在那之前它照常在跑',
      '部署会重启 sing-box,这台机器上【全部入口】的在线连接都会断开',
      '入口级的流量计数器不会复用,重建之后历史曲线接不上',
    ],
    okText: '删除',
    onOk: async () => {
      running.value = '正在删除入口'
      emit('busy', running.value)
      try {
        await api.deleteInbound(i.id)
        emit('changed')
        message.success('已删除。下次部署这台机器时才真正从节点上撤掉')
      } catch (e) {
        message.error(e instanceof ApiError ? e.message : '删除失败')
      } finally {
        running.value = ''
        emit('busy', '')
      }
    },
  })
}

// ---------------------------------------------------------------- 握手目标

const destOpen = ref(false)
const destTarget = ref<NodeInbound | null>(null)
const destInput = ref('')
const destResult = ref<DestCheckResult | null>(null)
const destError = ref('')

function openDest(i: NodeInbound) {
  destTarget.value = i
  destInput.value = i.reality_dest
  destResult.value = null
  destError.value = ''
  destOpen.value = true
}

async function runDest() {
  const target = destTarget.value
  if (!target) return
  running.value = '正在实测握手目标'
  emit('busy', running.value)
  destResult.value = null
  destError.value = ''
  try {
    const r = await api.applyInboundDest(target.id, destInput.value.trim())
    destResult.value = r.result
    if (r.error) {
      destError.value = r.error
    } else {
      message.success('握手目标已实测通过并写入这个入口')
      emit('changed')
    }
  } catch (e) {
    destError.value = e instanceof ApiError ? e.message : '实测失败'
  } finally {
    running.value = ''
    emit('busy', '')
  }
}

// ---------------------------------------------------------------- 出口(链式)

const chainOpen = ref(false)
const chainTarget = ref<NodeInbound | null>(null)
const chainForm = ref({
  target_kind: 'INBOUND' as 'INBOUND' | 'EXTERNAL',
  target_inbound_id: 0,
  target_external_id: 0,
})

/**
 * 当前选择的落地种类下有没有候选。
 *
 * 没有候选时下拉里一个选项都没有,而 v-model 还绑着 0 —— antd 会把它
 * 原样渲染成一个 "0"。那既不是地址也不是名字,看起来像个 bug,
 * 而真实情况是「这个面板上还没有第二台机器可以当落地」。
 */
const chainCandidates = computed(() =>
  chainForm.value.target_kind === 'INBOUND' ? landingInbounds.value : externalProxies.value,
)
const chainReason = computed(() => {
  if (chainCandidates.value.length > 0) return ''
  return chainForm.value.target_kind === 'INBOUND'
    ? '还没有别的落地入口可选 —— 中转角色的机器与这台机器自己都不能当落地。'
    : '还没有可用的外部代理。链式落地本版本只支持 Shadowsocks 的外部代理。'
})

function openChain(i: NodeInbound) {
  chainTarget.value = i
  chainForm.value = {
    target_kind: i.chain_target_kind === 'EXTERNAL' ? 'EXTERNAL' : 'INBOUND',
    target_inbound_id: i.chain_target_inbound_id || landingInbounds.value[0]?.value || 0,
    target_external_id: i.chain_target_external_id || externalProxies.value[0]?.id || 0,
  }
  chainOpen.value = true
}

function confirmChain() {
  const inbound = chainTarget.value
  if (!inbound) return
  const name =
    chainForm.value.target_kind === 'INBOUND'
      ? landingInbounds.value.find((x) => x.value === chainForm.value.target_inbound_id)?.label
      : externalProxies.value.find((x) => x.id === chainForm.value.target_external_id)?.display_name
  if (!name) {
    message.warning('请选择落地')
    return
  }
  chainOpen.value = false
  lbDangerConfirm({
    title: `把入口「${inbound.display_name}」的出口改到「${name}」?`,
    impacts: [
      `${nodeLabel.value} 上的 sing-box 会重启,这台机器上【全部入口】的在线连接都会断开`,
      ...(chainForm.value.target_kind === 'INBOUND'
        ? [`落地那台机器也会重新部署一次,它上面的在线连接同样会断开`]
        : []),
      '两次部署有先后:先落地后本机,顺序反了本机会连不上落地',
      '订阅内容不变 —— 用户不会察觉这个入口后面多了一跳',
      '拨测只验证链路可用,不验证出口真的落在那台机器上',
    ],
    okText: '改出口',
    onOk: () => {
      // 不返回这个 Promise:AntD 只要拿到 Promise 就把确认框留在屏幕上
      // 转圈等它 resolve,而两台机器的部署要几十秒。
      void runApplyChain(inbound)
    },
  })
}

async function runApplyChain(inbound: NodeInbound) {
  running.value = '正在切换出口(两台机器)'
  emit('busy', running.value)
  lastChain.value = null
  try {
    const r = await api.applyChain(inbound.id, { ...chainForm.value })
    lastChain.value = r.result
    if (r.error) {
      message.error(r.error)
    } else {
      message.success('出口已切换')
    }
    emit('changed')
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '切换失败')
  } finally {
    running.value = ''
    emit('busy', '')
  }
}

function clearChain(i: NodeInbound) {
  lbDangerConfirm({
    title: `把入口「${i.display_name}」的出口改回本机直连?`,
    impacts: [
      '这台机器上的 sing-box 会重启,全部入口的在线连接都会断开',
      '落地那台也会重新部署一次,以撤掉这条链路的凭据',
      '之后这个入口的流量从这台机器自己的 IP 出去',
    ],
    okText: '改回直连',
    onOk: () => {
      void runClearChain(i)
    },
  })
}

async function runClearChain(i: NodeInbound) {
  running.value = '正在改回直连(两台机器)'
  emit('busy', running.value)
  lastChain.value = null
  try {
    const r = await api.clearChain(i.id)
    lastChain.value = r.result
    if (r.error) {
      message.error(r.error)
    } else {
      message.success('已改回本机直连')
    }
    emit('changed')
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '操作失败')
  } finally {
    running.value = ''
    emit('busy', '')
  }
}

// ---------------------------------------------------------------- nginx 转发

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
    target_external_id: externalProxies.value[0]?.id ?? 0,
    access_tier_id: props.node.access_tier_id,
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
      <span class="nr__spacer" />
      <a-button size="small" :loading="!!running" @click="checkNginx">检查 nginx</a-button>
      <a-button size="small" :loading="!!running" @click="deployNow">下发转发配置</a-button>
      <a-dropdown placement="bottomRight" :disabled="!!running">
        <a-button size="small" type="primary">新增入口 ▾</a-button>
        <template #overlay>
          <a-menu>
            <a-menu-item :disabled="isRelayHost" @click="openCreateInbound">
              <span :title="isRelayHost ? '中转角色的机器上不跑 sing-box,角色一经创建不可更改' : ''">
                sing-box 入口
              </span>
            </a-menu-item>
            <a-menu-item @click="openCreate">Nginx 转发入口</a-menu-item>
          </a-menu>
        </template>
      </a-dropdown>
    </div>

    <!-- sing-box 入口。中转角色的机器上没有,这一整块不出现 ——
         显示一个"未配置"的占位会让人以为可以去配一个,而角色不可更改。 -->
    <template v-if="!isRelayHost">
      <div class="nr__section">
        <span class="nr__kind">sing-box</span>
        <span class="nr__note">
          这台机器自己的入站。可以有多条,各自的协议、端口、访问等级与出口互不相干。
        </span>
      </div>
      <div v-if="!inbounds.length" class="nr__warn">
        这台机器上一个入口都没有 —— sing-box 会正常运行,但谁都连不上。
        点右上角「新增入口 → sing-box 入口」加一条。
      </div>
      <div v-for="i in inbounds" :key="i.id" class="nr__sb">
        <div class="nr__sb-head">
          <span class="nr__sb-name">{{ i.display_name }}</span>
          <LbStatusTag :meta="protocolMeta(i)" />
          <LbStatusTag v-if="!i.enabled" :meta="enabledMeta.off" />
          <span class="nr__tag lb-mono">{{ i.tag }}</span>
          <span class="nr__spacer" />
          <a-button size="small" type="link" :disabled="!!running" @click="openEditInbound(i)">
            编辑
          </a-button>
          <a-button
            v-if="i.protocol === 'VLESS_REALITY'"
            size="small"
            type="link"
            :disabled="!!running"
            @click="openDest(i)"
          >
            握手目标
          </a-button>
          <a-button size="small" type="link" :disabled="!!running" @click="openChain(i)">
            出口
          </a-button>
          <a-button size="small" type="link" danger :disabled="!!running" @click="removeInbound(i)">
            删除
          </a-button>
        </div>
        <div class="nr__sb-body lb-mono">
          <!-- 两个端口相同时只写一个号码:写成「公网 443 → 主机 443」是把同一个
               数字说两遍,而那正好会让人以为这台机器上配了端口转发。 -->
          <template v-if="(i.public_port || i.listen_port) === i.listen_port">
            端口 {{ i.listen_port }}
          </template>
          <template v-else>公网 {{ i.public_port }} → 主机 {{ i.listen_port }}</template>
          <template v-if="node.ipv6_address">
            · IPv6 [{{ node.ipv6_address }}]:{{ i.ipv6_public_port || i.public_port || i.listen_port }}
          </template>
          <template v-if="i.protocol === 'SHADOWSOCKS'"> · {{ i.ss_method }}</template>
          <template v-else> · {{ i.reality_dest || '未设握手目标' }}</template>
          <template v-if="i.tcp_fast_open"> · TFO</template>
        </div>
        <div class="nr__sb-sub">
          <div>
            出口:
            <template v-if="i.chain_target_kind">
              经 <b>{{ chainTargetName(i) }}</b>,链路凭据
              <span class="lb-mono">{{ i.chain_code || '—' }}</span>
              <a-button size="small" type="link" danger :disabled="!!running" @click="clearChain(i)">
                改回直连
              </a-button>
            </template>
            <template v-else>本机直连</template>
          </div>
          <div>
            访问等级 {{ i.access_tier_name }} ·
            {{ i.subscription_enabled ? '在订阅里' : '已从订阅下架' }} · 排序 {{ i.sort_order }}
          </div>
          <div>
            订阅里展开成:
            <span v-for="e in subEntriesOf(i)" :key="e.name" class="nr__sb-entry">
              {{ e.name }} <span class="lb-mono">{{ e.addr }}</span>
            </span>
          </div>
          <!-- IPv6 不是第二条节点记录,而是同一条记录在订阅生成时的逻辑展开 ——
               两条指向同一个入站,改 IPv6 保存即生效,不需要重新部署。 -->
          <div v-if="node.ipv6_address">
            两条指向<b>同一个入站</b>,UUID、REALITY 公钥、short ID 完全相同。
          </div>
        </div>
        <div v-if="!i.deployed_protocol" class="nr__warn">
          这个入口还没有上过节点,<b>不会出现在任何人的订阅里</b>。部署这台机器之后才生效。
        </div>
      </div>
      <div v-if="!node.subscription_enabled && inbounds.length" class="nr__warn">
        这台机器的「下发到用户订阅」是关的,它上面全部入口都不会进入新生成的订阅。
        节点仍在运行,已导入旧订阅的客户端还能用。
      </div>
      <p class="nr__note">
        入口的增删改<b>不会自动部署</b>:那会重启 sing-box,把这台机器上全部入口的在线连接
        一起踢掉。改完之后自己挑时机点上面的「部署」。
        <br />
        <b>流量拆不到入口。</b>同一个用户在这台机器上的流量是所有入口的合计 ——
        计数器里没有入站这一维,不是暂时没做。
      </p>
    </template>

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

    <!-- nginx 透传入口。与上面那块分开:两者的操作摩擦不同 ——
         这些改了只 reload,在途连接一条不断。 -->
    <div class="nr__section">
      <span class="nr__kind">nginx 转发</span>
      <span class="nr__note">把字节原样搬到落地。可以有多条,各占一个端口。</span>
    </div>
    <div v-if="loadError" class="nr__warn">{{ loadError }}</div>
    <a-table
      v-if="!loadError"
      :data-source="relays"
      :loading="loading"
      :pagination="false"
      row-key="id"
      size="small"
    >
      <a-table-column key="name" title="线路" data-index="display_name" />
      <a-table-column key="port" title="端口">
        <template #default="{ record }">
          <span class="lb-mono">{{ record.listen_port }}</span>
          <span v-if="record.public_port && record.public_port !== record.listen_port">
            → 公网 <span class="lb-mono">{{ record.public_port }}</span>
          </span>
        </template>
      </a-table-column>
      <a-table-column key="target" title="落地">
        <template #default="{ record }">
          <div>{{ record.target_name || '(已删除)' }}</div>
          <LbStatusTag :meta="readyMeta[record.target_ready ? 'yes' : 'no']" />
        </template>
      </a-table-column>
      <a-table-column key="tier" title="等级" data-index="access_tier_name" />
      <a-table-column key="state" title="状态">
        <template #default="{ record }">
          <LbStatusTag :meta="enabledMeta[record.enabled ? 'on' : 'off']" />
          <span class="nr__sub">{{
            record.subscription_enabled ? '在订阅里' : '已从订阅下架'
          }}</span>
        </template>
      </a-table-column>
      <a-table-column key="sort" title="排序" data-index="sort_order" />
      <a-table-column key="ops" title="操作">
        <template #default="{ record }">
          <a-button size="small" type="link" @click="openEdit(record)">编辑</a-button>
          <a-button size="small" type="link" danger @click="removeRelay(record)">删除</a-button>
        </template>
      </a-table-column>
      <template #emptyText>
        <!-- 只在读取成功且确实是零条时才说「还没有」。读取失败时上面那条
             loadError 会先出现,表格根本不渲染 —— 否则「读不到」会被读成
             「一条都没有」,而两者要做的事完全不同。 -->
        <span class="nr__note">
          还没有转发入口。点右上角「新增入口 → Nginx 转发入口」加一条。
        </span>
      </template>
    </a-table>

    <p class="nr__note">
      转发入口的增删改只会 <b>reload nginx</b>,在途连接一条不断 ——
      所以这里没有需要挑时机的操作。落地未就绪的线路不会出现在任何人的订阅里。
    </p>

    <!-- 两次部署的结果都要显示:失败时管理员必须知道卡在哪台机器上 -->
    <div v-if="lastChain" class="nr__result">
      <div>停在:<b>{{ lastChain.stage }}</b></div>
      <div v-if="lastChain.target_deploy">落地部署:{{ lastChain.target_deploy.status }}</div>
      <div v-if="lastChain.host_deploy">本机部署:{{ lastChain.host_deploy.status }}</div>
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
    <a-modal
      v-model:open="inboundOpen"
      :title="editingInbound ? '编辑 sing-box 入口' : '新增 sing-box 入口'"
      :confirm-loading="!!running"
      :ok-button-props="{ disabled: !!protocolSwitchBlocked }"
      @ok="submitInbound"
    >
      <a-form layout="vertical" size="small">
        <a-form-item label="入口名称(会发给用户)">
          <a-input v-model:value="inboundForm.display_name" placeholder="例如:洛杉矶 01-SS" />
        </a-form-item>
        <a-form-item label="落地协议">
          <a-radio-group v-model:value="inboundForm.protocol" size="small" button-style="solid">
            <a-radio-button value="VLESS_REALITY">{{ PROTOCOL_LABEL.VLESS_REALITY }}</a-radio-button>
            <a-radio-button value="SHADOWSOCKS">{{ PROTOCOL_LABEL.SHADOWSOCKS }}</a-radio-button>
          </a-radio-group>
          <div v-if="protocolSwitchBlocked" class="nr__warn">{{ protocolSwitchBlocked }}</div>
        </a-form-item>
        <a-form-item v-if="inboundForm.protocol === 'SHADOWSOCKS'" label="加密方法">
          <a-select
            v-model:value="inboundForm.ss_method"
            :options="
              Object.entries(SS_METHOD_LABEL).map(([v, l]) => ({ value: v, label: l }))
            "
          />
        </a-form-item>
        <a-form-item label="主机监听端口(sing-box 真正 bind 的号码)">
          <a-input-number v-model:value="inboundForm.listen_port" :min="1" :max="65535" />
        </a-form-item>
        <a-form-item label="公网端口(留 0 表示与监听端口相同)">
          <a-input-number v-model:value="inboundForm.public_port" :min="0" :max="65535" />
          <div class="nr__hint">
            NAT 主机上两者不同:公网 443 映射到主机的 20443 时,监听端口填 20443、公网端口填
            443。填反了 sing-box 会监听在转发链路另一端的号码上,而各项检查都会通过。
          </div>
        </a-form-item>
        <a-form-item v-if="node.ipv6_address" label="IPv6 公网端口(留 0 表示跟随 IPv4 公网端口)">
          <a-input-number v-model:value="inboundForm.ipv6_public_port" :min="0" :max="65535" />
        </a-form-item>
        <a-form-item>
          <a-checkbox v-model:checked="inboundForm.tcp_fast_open">
            TCP Fast Open(同时管两端:入站与订阅里下发的出站)
          </a-checkbox>
          <div class="nr__hint">
            默认关。成败取决于用户到这台机器这一段路径上的中间设备,而那条路面板看不到。
          </div>
        </a-form-item>
        <a-form-item label="排序">
          <a-input-number v-model:value="inboundForm.sort_order" />
        </a-form-item>
        <a-form-item label="对用户公开的备注">
          <a-input v-model:value="inboundForm.public_remark" />
        </a-form-item>
        <a-form-item>
          <a-checkbox v-model:checked="inboundForm.enabled">
            启用(关掉后下次部署时从节点上撤掉)
          </a-checkbox>
          <a-checkbox v-model:checked="inboundForm.subscription_enabled">下发到订阅</a-checkbox>
        </a-form-item>
      </a-form>
    </a-modal>

    <!-- ---------------- 握手目标实测 ---------------- -->
    <a-modal
      v-model:open="destOpen"
      title="实测握手目标"
      :confirm-loading="!!running"
      ok-text="实测并写入"
      @ok="runDest"
    >
      <p class="nr__hint">
        检测从<b>这台机器的出口</b>发起 —— CDN 按地域下发不同证书链,在别处测出来的结果
        对这台机器不成立。通过之后才写入这个入口;不通过时拒绝保存。
      </p>
      <a-form layout="vertical" size="small">
        <a-form-item label="握手目标域名">
          <a-input v-model:value="destInput" placeholder="例如:www.fastly.com" />
        </a-form-item>
      </a-form>
      <div v-if="destError" class="nr__warn">{{ destError }}</div>
      <div v-if="destResult" class="nr__result">
        <div>
          {{ destResult.server }}:{{ destResult.port }} ·
          最大 TLS 记录 {{ destResult.max_record_size }} 字节
        </div>
        <div v-for="(p, idx) in destResult.problems" :key="idx" class="nr__warn">{{ p }}</div>
      </div>
    </a-modal>

    <!-- ---------------- 出口(链式) ---------------- -->
    <a-modal
      v-model:open="chainOpen"
      :title="`入口「${chainTarget?.display_name ?? ''}」的出口`"
      ok-text="下一步"
      @ok="confirmChain"
    >
      <p class="nr__hint">
        链式出口只改这一个入口的去向,这台机器上别的入口不受影响。
        订阅内容不变 —— 用户拿到的还是这台机器的地址与协议,不知道后面还有一跳。
      </p>
      <a-form layout="vertical" size="small">
        <a-form-item label="落地种类">
          <a-radio-group v-model:value="chainForm.target_kind" size="small" button-style="solid">
            <a-radio-button value="INBOUND">自建节点的入口</a-radio-button>
            <a-radio-button value="EXTERNAL">外部代理</a-radio-button>
          </a-radio-group>
        </a-form-item>
        <a-form-item label="落地">
          <a-select
            v-if="chainForm.target_kind === 'INBOUND'"
            v-model:value="chainForm.target_inbound_id"
            placeholder="选择落地入口"
            :disabled="!landingInbounds.length"
            :options="landingInbounds"
          />
          <a-select
            v-else
            v-model:value="chainForm.target_external_id"
            placeholder="选择外部代理"
            :disabled="!externalProxies.length"
            :options="externalProxies.map((p) => ({ value: p.id, label: p.display_name }))"
          />
        </a-form-item>
      </a-form>
      <!-- 按钮变灰时必须说出为什么。只变灰的话,管理员会以为是权限问题
           或者页面坏了,而真实原因是「面板上还没有第二台机器」。 -->
      <p v-if="chainReason" class="nr__warn">{{ chainReason }}</p>
    </a-modal>

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
            :options="externalProxies.map((p) => ({ value: p.id, label: p.display_name }))"
          />
          <div class="nr__hint">
            落地是一个<b>入口</b>而不是一台机器 —— 一台机器上有两个入口时,
            「转发到它」是有歧义的,而流量进错入口不会有任何报错。
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
  border: 1px solid var(--lb-border);
  border-radius: 8px;
  margin-bottom: 12px;
}
.nr__head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
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
  color: var(--lb-text-secondary);
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
  border: 1px solid var(--lb-border);
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
  color: var(--lb-text-secondary);
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
  color: var(--lb-text-secondary);
}
.nr__note,
.nr__hint {
  font-size: 12px;
  color: var(--lb-text-secondary);
  margin: 0;
  line-height: 1.6;
}
.nr__warn {
  font-size: 12px;
  color: var(--lb-danger);
}
.nr__nginx {
  font-size: 12px;
  line-height: 1.6;
}
.nr__sub {
  font-size: 12px;
  color: var(--lb-text-secondary);
  margin-left: 6px;
}
.nr__result {
  font-size: 12px;
  line-height: 1.7;
  border-top: 1px dashed var(--lb-border);
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
  color: var(--lb-text-secondary);
  word-break: break-all;
}
</style>
