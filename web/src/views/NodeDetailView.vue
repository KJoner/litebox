<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { message, Modal } from 'ant-design-vue'
import {
  api,
  ApiError,
  PROTOCOL_LABEL,
  type AccessTier,
  type ConfigDiff,
  type DailyPoint,
  type DeployResult,
  type DeploymentRecord,
  type DestCheckResult,
  type Node,
  type NodeCycleUsage,
  type NodeMetrics,
  type NodeProtocol,
  type ProbeResult,
  type TuneReport,
} from '@/api/client'
import { formatBytes, formatDuration, shortHash } from '@/utils/format'
import DeployStepList from '@/components/DeployStepList.vue'
import MetricsChart from '@/components/MetricsChart.vue'
import NodeTuningPanel from '@/components/node/NodeTuningPanel.vue'
import NodeEntriesPanel from '@/components/node/NodeEntriesPanel.vue'
import NodeFormModal from '@/components/node/NodeFormModal.vue'
import {
  LbEmptyState,
  LbNameConfirm,
  LbQuotaBar,
  LbSparkline,
  LbStatusTag,
  LbTimeText,
  configStatusMeta,
  lbDangerConfirm,
  type LbPoint,
} from '@/components/lb'
import { configState, needsDeploy, nodeBadges } from '@/components/lb/derive'
import { useNarrow } from '@/composables/useNarrow'
import { color, threshold, usageColor } from '@/theme/tokens'

/**
 * 节点详情页。
 *
 * 原来是一个 720 宽的抽屉,现在铺满整页 —— 抽屉里塞不下一个可增删改的入口列表,
 * 而「这台机器对外提供哪些入口」正是节点详情要回答的第一个问题。
 * 整页还带来一件抽屉给不了的东西:地址可以分享,`/nodes/3/entries`
 * 直接打开那台机器的入口列表。
 *
 * 按钮仍然分三层,层次比抽屉时期更要紧 —— 页面宽了之后按钮更容易挨在一起:
 *   只读检查(6 个)  常驻工具条,结果一律弹窗呈现。点错了最坏结果是白等几秒。
 *   部署            唯一的主按钮 —— 它是这个页面存在的理由。
 *   会改变节点的     全部进「⋯」,不可逆的四项在分隔线之下。
 *
 * **只读检查的结果全部走弹窗**,不再在工具条下方占一块。那一块会把 Tab 整个
 * 推下去,而检查结果是看完就走的东西 —— 让它挤占常驻内容的位置,
 * 等于每做一次检查都要重新找一遍自己刚才在看哪个 Tab。
 */
const route = useRoute()
const router = useRouter()

const tiers = ref<AccessTier[]>([])
/** 路由参数是字符串;非法值按「没有这个节点」处理,由 loadError 呈现。 */
const nodeId = computed(() => {
  const raw = Number(route.params.id)
  return Number.isFinite(raw) && raw > 0 ? raw : null
})

/**
 * 抽屉宽度。窄屏必须占满,不能固定 720 ——
 * 390 宽的屏幕上,固定宽度会让内容左半边整个滑出可视区,
 * 而抽屉里横向滚动条并不明显,看起来就是"字被切掉了一半"。
 */
const narrow = useNarrow()

const node = ref<Node | null>(null)
const loading = ref(false)
const loadError = ref<{ message: string; status?: number; at: string } | null>(null)
/**
 * 当前 Tab 与地址栏同步。
 *
 * 整页之后地址是可以分享的,而「打开某台机器的入口列表」正是最常被转述的
 * 一件事 —— 只记住 /nodes/3 的话,对方点开落在概览上,还得再找一次。
 * 非法的 tab 名回落到概览,不显示一个空白页。
 */
const TABS = ['overview', 'metrics', 'deploys', 'traffic', 'entries'] as const
const tab = ref<string>(
  TABS.includes(String(route.params.tab) as (typeof TABS)[number])
    ? String(route.params.tab)
    : 'overview',
)

function syncTabToRoute(key: unknown) {
  // replace 而不是 push:切 Tab 不该在浏览器历史里堆一层,
  // 否则连点五个 Tab 之后要按五次后退才回得到列表。
  router.replace({ name: 'node-detail', params: { id: String(nodeId.value), tab: String(key) } })
}

/** 编辑表单由本页托管 —— 抽屉时期它挂在列表页上,而现在列表页不再知道谁被打开了。 */
const editOpen = ref(false)
const tierLoadError = ref(false)

const deployments = ref<DeploymentRecord[]>([])
const daily = ref<DailyPoint[]>([])
const cycle = ref<NodeCycleUsage | null>(null)
const trafficError = ref(false)
/**
 * 这台机器是否被面板计流量。中转主机为 false。
 *
 * 与 trafficError 分开:一个是「读不到」,一个是「本来就不计」——
 * 前者要重试,后者重试一万次也不会有数字。混成一种的表现是
 * 中转机的详情页上永远挂着一个红叉和一个没用的重试按钮。
 */
const metered = ref(true)
const notMeteredReason = ref('')

/** 工具条上正在跑的动作名。同一时刻只允许一个。 */
const running = ref('')

/**
 * 节点地址填的是域名而不是 IP 字面量。
 *
 * 判据与后端一致:能被解析成 IP 的就是字面量,否则就是域名。域名节点的
 * "现在指到哪儿"只有在刚连过之后才知道 —— 所以解析结果只出现在
 * 「测试 SSH」与「探测」的结果里,不放进常驻的信息卡片:
 * 那会让每次打开详情页都去连一次节点。
 */
const hostIsDomain = computed(() => {
  const host = node.value?.host ?? ''
  if (!host) return false
  // IPv4 字面量:四段数字。IPv6 字面量:含冒号。其余按域名看待。
  return !/^\d{1,3}(\.\d{1,3}){3}$/.test(host) && !host.includes(':')
})

/** 当前(期望)协议是 Shadowsocks —— 决定这一屏显示 REALITY 还是加密方法。 */
const isSS = computed(() => node.value?.protocol === 'SHADOWSOCKS')
/**
 * 中转角色:这台机器上不跑 sing-box。
 *
 * 它的协议、三个端口、REALITY 参数在库里都保持着建表时的默认值 ——
 * 渲染出来就是一份从来没有生效过的配置,而那看起来像是配好了。
 */
const isRelay = computed(() => node.value?.role === 'RELAY')

/**
 * 期望协议与节点上生效的协议不一致 —— 也就是「改了协议还没部署」。
 *
 * 从未部署过(deployed_protocol 为空)不算不一致:那台机器上还什么都没有,
 * 报「不一致」会让管理员以为是自己改坏了什么。
 */
const protocolMismatch = computed(
  () =>
    !!node.value &&
    !!node.value.deployed_protocol &&
    node.value.deployed_protocol !== node.value.protocol,
)

async function load(id: number) {
  loading.value = true
  loadError.value = null
  try {
    node.value = await api.node(id)
  } catch (err) {
    loadError.value = {
      message: err instanceof ApiError ? err.message : '加载节点详情失败',
      status: err instanceof ApiError ? err.status : undefined,
      at: new Date().toLocaleTimeString(),
    }
    node.value = null
    loading.value = false
    return
  }
  loading.value = false

  api
    .nodeDeployments(id, 50)
    .then((r) => (deployments.value = r.items))
    .catch(() => (deployments.value = []))
  trafficError.value = false
  metered.value = true
  notMeteredReason.value = ''
  api
    .nodeTraffic(id, 30)
    .then((r) => {
      daily.value = r.daily
      cycle.value = r.cycle
      metered.value = r.metered !== false
      notMeteredReason.value = r.reason ?? ''
    })
    .catch(() => {
      daily.value = []
      cycle.value = null
      trafficError.value = true
    })
  loadMetrics(id)
}

function reload() {
  if (nodeId.value !== null) load(nodeId.value)
}

// ---------- 只读检查 ----------
//
// 结果贴在工具条下方的面板里,不用吐司:探测会写回架构、版本、构建标签,
// 用一条三秒吐司交付「已更新 3 项」等于让管理员自己去 descriptions 里找哪里变了。

const sshResult = ref<{ ok: boolean; text: string; ip?: string } | null>(null)
const probe = ref<ProbeResult | null>(null)
const diff = ref<ConfigDiff | null>(null)
const destResults = ref<DestCheckResult[]>([])
const tuning = ref<TuneReport | null>(null)
/**
 * 当前弹窗里显示哪一种结果。同一时刻只可能有一种 ——
 * 分成四个开关只是把同一份状态抄四遍,而四份状态迟早会有一份忘了清。
 */
const panel = ref<'' | 'ssh' | 'probe' | 'diff' | 'dest' | 'tune'>('')

/** 弹窗标题跟着内容走。缺省值只是为了让关闭动画期间标题不闪成空。 */
const panelTitle = computed(() => {
  switch (panel.value) {
    case 'ssh':
      return 'SSH 连通性'
    case 'probe':
      return '节点探测'
    case 'diff':
      return '配置比对'
    case 'dest':
      return '扫描握手目标'
    case 'tune':
      return 'TCP 调优'
    default:
      return '检查结果'
  }
})

async function readonlyAction(
  label: string,
  key: typeof panel.value,
  fn: () => Promise<void>,
) {
  running.value = label
  panel.value = key
  try {
    await fn()
  } catch (err) {
    if (key === 'ssh') {
      sshResult.value = { ok: false, text: err instanceof ApiError ? err.message : `${label}失败` }
    } else {
      panel.value = ''
      message.error(err instanceof ApiError ? err.message : `${label}失败`)
    }
  } finally {
    running.value = ''
  }
}

const doTestSSH = () =>
  readonlyAction('测试 SSH', 'ssh', async () => {
    const r = await api.testNodeSSH(nodeId.value!)
    sshResult.value = { ok: true, text: r.uname, ip: r.resolved_ip }
  })

const doProbe = () =>
  readonlyAction('探测', 'probe', async () => {
    probe.value = await api.probeNode(nodeId.value!)
    reload()
    // 探测会写回节点档案,重新读一次让上面的字段跟着变。
    node.value = await api.node(nodeId.value!)
  })

const doDiff = () =>
  readonlyAction('比对配置', 'diff', async () => {
    diff.value = await api.nodeConfigDiff(nodeId.value!)
  })

const doScanDests = () =>
  readonlyAction('扫描握手目标', 'dest', async () => {
    destResults.value = (await api.scanNodeDests(nodeId.value!)).items
  })

/**
 * TCP 调优。按钮本身是只读的:它只采集这台机器的事实、算出方案、逐项与
 * 当前值对比。要不要写下去在面板里另有一个带影响范围的确认 ——
 * 一个直接下发的「一键优化」在 128MB 小鸡与 4GB 机器上写的是完全不同的值,
 * 而两次点击看起来一模一样。
 */
const doTuning = () =>
  readonlyAction('TCP 调优检查', 'tune', async () => {
    tuning.value = await api.tuningPreview(nodeId.value!)
  })

const doSyncTraffic = () =>
  readonlyAction('同步流量', '', async () => {
    const r = await api.syncNodeTraffic(nodeId.value!)
    // 这个只更新数字,结果就在页面上 —— 吐司 + 就地刷新即可,不用开面板。
    message.success(`流量已同步 · 新增 ${formatBytes(r.bytes_added)}`)
    panel.value = ''
    reload()
  })

const doCollectMetrics = () =>
  readonlyAction('采集资源', '', async () => {
    const m = await api.collectNodeMetrics(nodeId.value!)
    message.success(`已采集 · 内存 ${memPercent(m).toFixed(0)}%`)
    panel.value = ''
    await loadMetrics(nodeId.value!)
  })

/**
 * SSH 通道能力。unknown 与 no 不能混:前者是没测出来(比如 sshd 只监听在
 * 别的地址上),后者是明确被拒。把 unknown 显示成"未允许"会让管理员去改一个
 * 其实没问题的配置。
 */
function forwardingText(state: string): string {
  if (state === 'yes') return '已允许'
  if (state === 'no') return '被禁止 —— 流量统计与握手实测不可用'
  return '未能确认'
}
function forwardingTone(state: string): string {
  if (state === 'yes') return color.success
  if (state === 'no') return color.danger
  return color.warning
}

/**
 * TLS 记录超过 8192 的目标不能用于 REALITY,握手超时的同理。
 * 禁用按钮必须说明为什么禁用,否则管理员只会反复点。
 */
function destBlockReason(d: DestCheckResult): string {
  // `d.problems?.[0]` 而不是 `d.problems[0]`:后者在 problems 为 null 时抛错,
  // 而这个函数跑在渲染期 —— 抛出去就是整个抽屉变白、遮罩留在屏幕上。
  if (!d.usable) return d.problems?.[0] ?? '实测未通过'
  if (d.max_record_size > 8192) return `TLS 记录 ${d.max_record_size} > 8192,不能用于 REALITY`
  if (!d.tls13) return '不支持 TLS 1.3'
  return ''
}

async function applyDest(dest: string) {
  running.value = '应用握手目标'
  try {
    // 「应用」不是纯保存:它会再实测一次目标、通过后才写库。
    await api.checkNodeDest(nodeId.value!, dest, true)
    message.success(`已应用 ${dest},需要部署才在节点上生效`)
    destResults.value = []
    panel.value = ''
    reload()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '应用失败')
  } finally {
    running.value = ''
  }
}

// ---------- 部署 ----------

const deployOpen = ref(false)
const deployRunning = ref(false)
const deployResult = ref<DeployResult | null>(null)

function confirmDeploy() {
  const n = node.value
  if (!n) return
  lbDangerConfirm({
    title: `部署 rev ${n.config_revision + 1} 到 ${n.display_name || n.name}?`,
    okText: '部署',
    okType: 'primary',
    impacts: [
      '会重启 sing-box,断开这台机器上全部在线连接',
      '部署前会强制同步一次流量,未落库的计数不会丢',
      `健康检查不通过时自动回滚到 rev ${n.config_revision}`,
    ],
    footer: '部署是可逆的(有自动回滚),所以不要求输入节点名称。',
    // 故意不 return doDeploy() 的 Promise。Modal.confirm 只要拿到 Promise
    // 就会把自己留在屏幕上转圈等它 resolve —— 而部署要 15~25 秒,
    // 这期间进度弹窗已经打开,两个 Modal 同层叠在一起,后开的反而被压住。
    // 这里让确认框先落幕,进度弹窗独占屏幕:同一时刻只有一个部署相关的窗口。
    onOk: () => {
      void doDeploy()
    },
  })
}

async function doDeploy() {
  deployResult.value = null
  deployOpen.value = true
  deployRunning.value = true
  running.value = '部署'
  try {
    deployResult.value = await api.deployNode(nodeId.value!)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '部署失败')
    deployOpen.value = false
  } finally {
    deployRunning.value = false
    running.value = ''
    reload()
  }
}

/** 把部署结果拼成 DeployStepList 认的形状,复用同一个时间线。 */
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
        steps: deployResult.value.steps,
      }
    : null,
)

// ---------- 会改变节点的动作 ----------

async function run(label: string, fn: () => Promise<unknown>, done: string) {
  running.value = label
  try {
    await fn()
    message.success(done)
    reload()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : `${label}失败`)
  } finally {
    running.value = ''
  }
}

const doInstall = () =>
  run('安装 sing-box', async () => {
    const r = await api.installNode(nodeId.value!)
    message.success(`sing-box 与 ${r.init_system} 服务定义已就绪`)
    // 改了节点的 sshd 配置就必须说出来,而且要说清改了哪个文件、备份在哪。
    // 悄悄改别人机器上的 sshd_config 再报一句"安装完成",是不能接受的。
    if (r.tcp_forwarding?.changed) {
      Modal.info({
        title: '已顺带打开节点的 SSH TCP 转发',
        width: 560,
        content: `${r.tcp_forwarding.detail}。\n\n面板读流量、从节点出口实测 REALITY 握手目标、部署时拨测 VLESS 都要经这条通道,原先它被 sshd 挡着。改动只加了一行 AllowTcpForwarding yes,用的是 reload 而不是 restart,没有断开任何已有连接。`,
        okText: '知道了',
      })
    }
  }, '安装完成,接下来执行「部署」')

function confirmRestart() {
  const n = node.value!
  lbDangerConfirm({
    title: `重启 ${n.display_name || n.name} 的服务?`,
    okText: '重启',
    impacts: [
      '断开这台机器上全部在线连接',
      `配置不变,重启后仍是 rev ${n.config_revision}`,
      '这是运维用的直接重启,不会先同步流量 —— 常规的用户变更请用「部署」',
    ],
    onOk: () => run('重启', () => api.restartNode(nodeId.value!), '已重启'),
  })
}

// 三个不可逆的动作各自一个输入名称确认。要求输入的是**内部名称** ——
// 内部名称唯一,展示名称可以重复。
type NameConfirmKind = 'resetKey' | 'uninstall' | 'delete'
const nameConfirm = ref<NameConfirmKind | null>(null)
const nameConfirmLoading = ref(false)

const nameConfirmMeta = computed(() => {
  const n = node.value
  if (!n || !nameConfirm.value) return null
  return {
    resetKey: {
      title: `重置 ${n.display_name || n.name} 的主机密钥`,
      okText: '重置',
      impacts: [
        '节点的 SSH 主机指纹会变,面板下次连接需重新信任',
        '若你还用其他工具 SSH 这台机器,它们会全部报「主机密钥已变更」',
        '无法撤销',
      ],
    },
    uninstall: {
      title: `卸载 ${n.display_name || n.name} 的服务`,
      okText: '卸载',
      impacts: [
        '停止并移除节点上的 sing-box、配置与服务单元',
        '该节点上的用户立即全部断连',
        '节点记录保留在面板里,历史流量与部署记录不删',
        '想恢复必须重新执行「安装 sing-box」+「部署」',
      ],
    },
    delete: {
      title: `删除节点 ${n.display_name || n.name}`,
      okText: '删除',
      impacts: [
        '面板将不再管理该节点',
        '节点上的 sing-box 与配置不会被自动清除,需手动处理',
        '它会从所有用户的订阅里消失',
        '历史流量记录保留,节点记录无法恢复',
      ],
    },
  }[nameConfirm.value]
})

async function doNameConfirm() {
  const kind = nameConfirm.value
  const id = nodeId.value
  if (!kind || id === null) return
  nameConfirmLoading.value = true
  try {
    if (kind === 'resetKey') {
      await api.resetNodeHostKey(id)
      message.success('主机密钥已重置,下次连接会重新固定')
    } else if (kind === 'uninstall') {
      await api.uninstallNode(id)
      message.success('节点上的服务与配置已移除')
    } else {
      await api.deleteNode(id)
      message.success('节点已删除')
    }
    nameConfirm.value = null
    // 节点删掉之后这个页面已经没有对应的东西了,回列表 ——
    // 留在原地会让下一次 reload 取回 404,页面显示「节点不存在」,
    // 而那看起来像出了错,其实是刚刚那一下的正常结果。
    if (kind === 'delete') router.push({ name: 'nodes' })
    else reload()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '操作失败')
  } finally {
    nameConfirmLoading.value = false
  }
}

// ---------- 重新引导 ----------

const bootstrapOpen = ref(false)
const bootstrapPassword = ref('')

async function doBootstrap() {
  const id = nodeId.value!
  const password = bootstrapPassword.value
  bootstrapOpen.value = false
  // 口令用完立刻抹掉,不留在组件状态里,也不进日志与审计详情。
  bootstrapPassword.value = ''

  running.value = '重新引导'
  try {
    const r = await api.bootstrapNode(id, password)
    message.success(r.already_present ? '节点上已有面板公钥,连接正常' : '面板公钥已装入并验证通过')
    reload()
  } catch (err) {
    Modal.error({
      title: '引导失败',
      width: 560,
      content: err instanceof ApiError ? err.message : '引导失败',
      okText: '知道了',
    })
  } finally {
    running.value = ''
  }
}

// ---------- 资源 ----------

const metricsHistory = ref<NodeMetrics[]>([])
const metricsHours = ref(6)
const latest = computed(() =>
  metricsHistory.value.length ? metricsHistory.value[metricsHistory.value.length - 1] : null,
)

async function loadMetrics(id: number) {
  try {
    metricsHistory.value = (await api.nodeMetricsHistory(id, metricsHours.value)).items
  } catch {
    // 采集是可选能力,配置里能关掉。取不到就当没有,不打扰其他信息。
    metricsHistory.value = []
  }
}

watch(metricsHours, () => {
  if (nodeId.value !== null) loadMetrics(nodeId.value)
})

function memPercent(m: NodeMetrics): number {
  return m.mem_total_kb > 0 ? (m.mem_used_kb / m.mem_total_kb) * 100 : 0
}
function diskPercent(m: NodeMetrics): number {
  return m.disk_total_kb > 0 ? (m.disk_used_kb / m.disk_total_kb) * 100 : 0
}

const metricLabels = computed(() => metricsHistory.value.map((m) => m.collected_at))
const cpuSeries = computed(() => [
  { name: 'CPU', color: color.brand, values: metricsHistory.value.map((m) => m.cpu_percent) },
])
const memSeries = computed(() => [
  { name: '内存', color: color.maintenance, values: metricsHistory.value.map(memPercent) },
])
// 上下行画在同一张图上:分成两张就看不出「下行涨的时候上行有没有跟着涨」,
// 而那正是判断链路是否正常的第一眼依据。
const netSeries = computed(() => [
  { name: '下行', color: color.brand, values: metricsHistory.value.map((m) => m.net_rx_bps) },
  { name: '上行', color: color.warning, values: metricsHistory.value.map((m) => m.net_tx_bps) },
])
const formatPercent = (v: number) => `${v.toFixed(0)}%`
const formatRate = (v: number) => `${formatBytes(v)}/s`

// ---------- 派生展示 ----------

const runMeta = computed(() => (node.value ? nodeBadges(node.value, latest.value?.collected_at) : []))

/** 缺的日子传 null,不补 0。 */
const dailyPoints = computed<LbPoint[]>(() => {
  const byDay = new Map(daily.value.map((d) => [d.day, d.total]))
  const out: LbPoint[] = []
  for (let i = 29; i >= 0; i--) {
    const key = new Date(Date.now() - i * 86400000).toISOString().slice(0, 10)
    out.push({ at: key, value: byDay.has(key) ? (byDay.get(key) as number) : null })
  }
  return out
})

/** 最近一次部署失败时的顶部横幅。失败原因要提到第一屏,不能埋在 Tab 里。 */
const failureBanner = computed(() => {
  const d = deployments.value[0]
  if (!d || (d.status !== 'FAILED' && d.status !== 'ROLLED_BACK')) return null
  const failed = d.steps?.find((s) => s.status === 'FAILED')
  const idx = d.steps?.findIndex((s) => s.status === 'FAILED') ?? -1
  return {
    title:
      idx >= 0
        ? `部署 rev ${d.revision} 失败于步骤 ${idx + 1}「${failed?.name}」`
        : `部署 rev ${d.revision} 失败`,
    body: d.error_message || failed?.detail || '',
    rollback: d.rollback_result,
  }
})

const deployColumns = [
  { title: 'rev', key: 'rev', width: 60 },
  { title: '结果', key: 'status', width: 120 },
  { title: '时间', key: 'time', width: 140 },
  { title: '耗时', key: 'cost', width: 80 },
]

function durationOf(d: DeploymentRecord): string {
  if (!d.finished_at) return '—'
  const ms = new Date(d.finished_at).getTime() - new Date(d.started_at).getTime()
  return ms >= 0 ? formatDuration(ms) : '—'
}

/**
 * 换节点时重置全部面板状态。
 *
 * 这个 watch 必须放在**所有 ref 声明之后** —— immediate 的 watcher 在 setup
 * 期间就会跑一次,而 `const` 声明的 ref 在执行到那一行之前处于暂时性死区,
 * 提前引用会直接抛 ReferenceError,抽屉第一次挂载就初始化失败。
 */
watch(
  () => nodeId.value,
  (id) => {
    probe.value = null
    diff.value = null
    destResults.value = []
    tuning.value = null
    sshResult.value = null
    panel.value = ''
    metricsHistory.value = []
    deployments.value = []
    cycle.value = null
    // **不重置 tab** —— 换的是节点,而地址里带着的 tab 是访问者的意图。
    // 直接打开 /nodes/3/entries 却落在概览上,等于把分享出去的地址废掉一半。
    if (id !== null) load(id)
    else node.value = null
  },
  { immediate: true },
)

/**
 * 等级清单只有编辑表单要用,进页面时拉一次。
 *
 * 拉不到不挡住整页 —— 那时看详情、做检查、管入口全都不受影响,
 * 只有编辑表单打不开。把它做成整页的加载条件,会让一个次要接口
 * 的抖动变成「节点详情打不开」。
 */
api
  .accessTiers()
  .then((r) => (tiers.value = r.items ?? []))
  .catch(() => {
    tiers.value = []
    tierLoadError.value = true
  })

/** 订阅展开。IPv6 不是第二条节点记录,而是同一条记录在订阅生成时的逻辑展开。 */
const subEntries = computed(() => {
  const n = node.value
  if (!n) return []
  const out = [{ name: n.display_name || n.name, addr: `${n.host}:${n.proxy_port}` }]
  if (n.ipv6_address) {
    // 端口为 0 表示跟随 IPv4 —— 这里的回落必须和后端 Expand 里的一致。
    const port = n.ipv6_proxy_port || n.proxy_port
    out.push({
      name: `${n.display_name || n.name}-IPV6`,
      addr: `[${n.ipv6_address}]:${port}`,
    })
  }
  return out
})
</script>

<template>
  <div class="nd">
    <!-- 返回链接不是装饰:整页之后浏览器的后退键会离开整个面板,
         而管理员多半只是想回到列表。
         不做成完整面包屑 —— 顶栏已经有一条「分组 / 节点详情」,
         再来一条只是把同一句话说两遍。 -->
    <RouterLink class="nd__back" :to="{ name: 'nodes' }">← 返回自建节点</RouterLink>

    <div class="nd__bar">
      <div class="nd__head">
        <div class="nd__title">
          <span class="nd__name">{{ node?.display_name || node?.name || '节点详情' }}</span>
          <template v-if="node">
            <LbStatusTag kind="node" :status="node.status" />
            <LbStatusTag
              :meta="configStatusMeta[configState(node)]"
              :suffix="`rev ${node.config_revision}`"
            />
            <a-tag>{{ node.access_tier_name }}</a-tag>
          </template>
        </div>
        <div v-if="node" class="nd__sub lb-mono">
          {{ node.name }} · {{ node.host }}
          <!-- 中转角色没有自己的入站,那三个端口在库里是 0,写出来只会让人
               以为配漏了。客户端连的端口在「入口」里,一条规则一个。 -->
          <template v-if="node.role !== 'RELAY'">
            · 公网 {{ node.proxy_port }} → 主机 {{ node.listen_port }}
          </template>
          · SSH {{ node.ssh_port }}
          <!-- 带端口显示 IPv6 必须加方括号:2a02:…::1:9443 分不清哪一段是端口。 -->
          <template v-if="node.ipv6_address">
            · IPv6 [{{ node.ipv6_address }}]:{{ node.ipv6_proxy_port || node.proxy_port }}
          </template>
        </div>
      </div>

      <a-space v-if="node">
        <!-- 库里的配置已经在节点上生效时不做成主按钮:那一下点下去只会白白
             重启一次 sing-box、断掉全部在线连接,换回一模一样的配置。 -->
        <!-- 中转机上没有 sing-box 配置可部署 —— 那一下点下去只会得到一句
             「中转角色的节点没有 sing-box 配置」。它要下发的是 nginx 转发,
             而那在「入口」里,连摩擦档次都不同(只 reload,不断连接)。 -->
        <a-button v-if="isRelay" size="small" @click="tab = 'entries'">转发配置</a-button>
        <a-button
          v-else
          :type="needsDeploy(node) ? 'primary' : 'default'"
          size="small"
          :loading="running === '部署'"
          @click="confirmDeploy"
        >
          部署
        </a-button>
        <a-dropdown placement="bottomRight">
          <a-button size="small" :aria-label="`${node.name} 的更多操作`" title="更多操作">⋯</a-button>
          <template #overlay>
            <a-menu>
              <a-menu-item-group title="安装与配置">
                <a-menu-item @click="editOpen = true">编辑节点</a-menu-item>
                <a-menu-item @click="doInstall">安装 sing-box</a-menu-item>
                <a-menu-item
                  @click="
                    () => {
                      bootstrapPassword = ''
                      bootstrapOpen = true
                    }
                  "
                >
                  重新引导(装公钥)
                </a-menu-item>
              </a-menu-item-group>
              <a-menu-divider />
              <a-menu-item-group title="危险操作">
                <a-menu-item danger @click="confirmRestart">重启服务</a-menu-item>
                <a-menu-item danger @click="nameConfirm = 'resetKey'">重置主机密钥</a-menu-item>
                <a-menu-item danger @click="nameConfirm = 'uninstall'">卸载服务</a-menu-item>
                <a-menu-item danger @click="nameConfirm = 'delete'">删除节点</a-menu-item>
              </a-menu-item-group>
            </a-menu>
          </template>
        </a-dropdown>
      </a-space>
    </div>

    <LbEmptyState
      v-if="loadError"
      variant="error"
      :title="loadError.status === 404 ? '节点不存在或已被删除' : loadError.message"
      description="它可能刚被删掉,或者这个地址是从别处复制来的。"
      :http-status="loadError.status"
      :occurred-at="loadError.at"
      @retry="reload"
    />

    <div v-else-if="loading || !node" class="nd__skel">
      <a-skeleton active :paragraph="{ rows: 3 }" />
      <a-skeleton active :paragraph="{ rows: 4 }" />
    </div>

    <template v-else>
      <!-- 失败原因提到第一屏。埋在某个 Tab 里等于没有。 -->
      <div v-if="failureBanner" class="nd__fail">
        <div class="nd__fail-title">{{ failureBanner.title }}</div>
        <div v-if="failureBanner.body" class="nd__fail-body">{{ failureBanner.body }}</div>
        <div v-if="failureBanner.rollback" class="nd__fail-rb">
          {{ failureBanner.rollback }} —— 节点当前运行的是回滚后的配置,用户未受影响。
        </div>
        <div class="nd__fail-acts">
          <a @click="tab = 'deploys'">查看完整步骤</a>
          <a @click="doDiff">配置比对</a>
        </div>
      </div>

      <div v-if="runMeta.length" class="nd__badges">
        <LbStatusTag v-for="(b, i) in runMeta" :key="i" :meta="b" />
        <span v-if="node.maintenance_message" class="nd__maint">
          {{ node.maintenance_message }}
        </span>
      </div>

      <!-- 只读检查常驻工具条。这一排都不改动节点状态。 -->
      <div class="nd__tools">
        <span class="nd__tools-label">只读检查</span>
        <a-button size="small" :loading="running === '测试 SSH'" @click="doTestSSH">测试 SSH</a-button>
        <a-button size="small" :loading="running === '探测'" @click="doProbe">探测</a-button>
        <!-- 比对配置、扫描握手目标、同步流量在中转机上都没有对应的东西:
             它上面没有 sing-box 配置、不用 REALITY、也没有计数器。
             留着只会让人点一下换回一句报错。 -->
        <a-button
          v-if="!isRelay"
          size="small"
          :loading="running === '比对配置'"
          @click="doDiff"
        >
          比对配置
        </a-button>
        <!-- Shadowsocks 节点上这一项仍然可点。它是切回 VLESS 的前置步骤 ——
             切协议要求握手目标已经实测通过,而实测只能从这里做。
             按钮上写清楚为什么它在一个不用 REALITY 的节点上出现。 -->
        <a-button
          v-if="!isRelay"
          size="small"
          :loading="running === '扫描握手目标'"
          :title="isSS ? '当前协议不用 REALITY。实测通过后才能把这个节点切回 VLESS' : ''"
          @click="doScanDests"
        >
          扫描握手目标<template v-if="isSS">(切回 VLESS 用)</template>
        </a-button>
        <a-button
          v-if="!isRelay"
          size="small"
          :loading="running === '同步流量'"
          @click="doSyncTraffic"
        >
          同步流量
        </a-button>
        <a-button size="small" :loading="running === '采集资源'" @click="doCollectMetrics">
          采集资源
        </a-button>
        <!-- 「转发」按钮去掉了:这台机器的入口(sing-box 与 nginx 转发)
             统一在下面的「入口」Tab 里管,那是一个要增删改的列表,
             不是一次看完就走的检查 —— 放进这一排会让两类东西长得一样。 -->
        <!-- 这一下只是算方案并与当前值对比,不写节点。要不要应用在面板里另点。 -->
        <a-button
          size="small"
          :loading="running === 'TCP 调优检查'"
          title="按这台机器的内存现算一份内核参数方案,先看后应用"
          @click="doTuning"
        >
          TCP 调优
        </a-button>
        <!-- 正在跑什么必须写出来。只有按钮上一个小转圈的话,管理员会以为没点上
             而反复点,也不明白为什么这时候点别处关不掉抽屉。 -->
        <span v-if="running" class="nd__tools-running">
          {{ running }}中…&nbsp;结果会弹窗显示
        </span>
        <span v-else class="nd__tools-note">都不改动节点状态</span>
      </div>

      <!-- 只读动作的结果一律弹窗呈现。
           探测会写回三四个字段,一条吐司交付不了;而铺在工具条下方会把 Tab
           整个推下去 —— 结果是看完就走的东西,不该占常驻内容的位置。
           一个弹窗装全部四种:同一时刻只可能有一种结果,分成四个弹窗
           只是把同一份开关状态抄四遍。
           检查还在跑时不接受遮罩点击与 ESC:那几秒里随手一点就关掉了,
           而结果几秒后才回来,看起来就是「点了探测,页面什么也没发生」。
           右上角的 × 照常可用 —— 那是明确的关闭意图,不是误触。 -->
      <a-modal
        :open="panel !== ''"
        :title="panelTitle"
        :width="narrow ? '100%' : 780"
        :footer="null"
        :mask-closable="running === ''"
        :keyboard="running === ''"
        @cancel="panel = ''"
      >
      <section v-if="panel === 'ssh' && sshResult" class="nd__panel">
        <div class="nd__panel-head">
          <LbStatusTag
            :meta="
              sshResult.ok
                ? { text: 'SSH 连接正常', shape: 'check', fg: '#1B7A4B', bg: '#E9F5EE', bd: '#C3E3D0' }
                : { text: 'SSH 连接失败', shape: 'cross', fg: '#B4291D', bg: '#FDECEA', bd: '#F3CFC9' }
            "
          />
        </div>
        <pre class="nd__pre lb-mono">{{ sshResult.text }}</pre>
        <!-- 域名节点上这一行是关键信息:面板每次操作前重新解析,
             而"连上了"与"连的是哪台机器"是两个问题。 -->
        <div v-if="sshResult.ip && hostIsDomain" class="nd__panel-note">
          <code class="lb-mono">{{ node.host }}</code> 当前解析到
          <code class="lb-mono">{{ sshResult.ip }}</code> —— 这次就是连的它。
        </div>
        <div v-if="!sshResult.ok" class="nd__panel-note">
          部署按钮此时仍然可点 —— 由后端拒绝并给出同样的原因,前端不自作主张禁用。
        </div>
      </section>

      <section v-else-if="panel === 'probe' && probe" class="nd__panel">
        <div class="nd__panel-head">
          <span class="nd__panel-title">探测完成 · 节点档案已更新</span>
        </div>
        <div class="nd__kv">
          <div v-if="hostIsDomain">
            <span>域名解析到</span>
            <b class="lb-mono">{{ probe.resolved_ip || '—' }}</b>
          </div>
          <div><span>架构</span><b class="lb-mono">{{ probe.arch }}</b></div>
          <div><span>系统</span><b class="lb-mono">{{ probe.os_name }}</b></div>
          <div><span>内存</span><b class="lb-mono">{{ probe.mem_total_mb }} MB</b></div>
          <div>
            <span>服务管理</span>
            <b class="lb-mono">{{ probe.init_system || '未检测到' }} {{ probe.init_version }}</b>
          </div>
          <div>
            <span>sing-box</span>
            <b class="lb-mono">{{ probe.singbox_version || '未安装' }}</b>
          </div>
          <div>
            <span>统计接口</span>
            <b :style="{ color: probe.has_v2ray_api ? color.success : color.danger }">
              {{ probe.has_v2ray_api ? 'with_v2ray_api 已启用' : '缺少 with_v2ray_api —— 流量统计不可用' }}
            </b>
          </div>
          <!-- 单列一行:它关着的时候,流量同步、握手目标实测、部署健康检查
               三样一起失败,而 sshd 只回一句 administratively prohibited。 -->
          <div>
            <span>SSH 通道(TCP 转发)</span>
            <b :style="{ color: forwardingTone(probe.tcp_forwarding) }">
              {{ forwardingText(probe.tcp_forwarding) }}
            </b>
          </div>
          <!-- 这些数组一律经 ?? [] 兜一层。后端已经保证不会再发 null(见
               ProbeResult 的初始化),但一个 null 数组的代价太不成比例:
               它在渲染期抛错,抽屉内容整个消失、遮罩却留在屏幕上,
               管理员只能对着一片灰色乱点。兜底之后最坏也只是少显示一行。 -->
          <div class="nd__kv-wide">
            <span>构建标签</span>
            <b class="lb-mono lb-ellipsis" :title="(probe.build_tags ?? []).join(',')">
              {{ (probe.build_tags ?? []).join(',') || '—' }}
            </b>
          </div>
        </div>
        <!-- problems 与 warnings 分开:前者是「这台机器跑不了 sing-box」,
             会把节点判成离线;后者是「能跑,但面板某些功能用不了」。
             混在一起会让管理员在代理完全正常时以为节点挂了。 -->
        <div v-if="probe.problems?.length" class="nd__panel-warn">
          <div v-for="(p, i) in probe.problems" :key="i">· {{ p }}</div>
        </div>
        <div v-if="probe.warnings?.length" class="nd__panel-warn">
          <div v-for="(w, i) in probe.warnings" :key="i">· {{ w }}</div>
        </div>
      </section>

      <section v-else-if="panel === 'diff' && diff" class="nd__panel">
        <div class="nd__panel-head">
          <span class="nd__panel-title">
            配置比对 · 节点上的
            <code class="lb-mono">{{ shortHash(diff.remote_sha256) || '(读不到)' }}</code>
            ↔ 库中 rev {{ diff.revision }}
          </span>
        </div>
        <div v-if="diff.in_sync" class="nd__panel-ok">
          节点上跑的配置与库里当前应有的完全一致,没有需要下发的改动。
        </div>
        <template v-else>
          <div class="nd__diff-sum">{{ diff.diff.summary }}</div>
          <div class="nd__diff">
            <div v-for="(u, i) in diff.diff.users.added ?? []" :key="'a' + i" class="nd__diff-add">
              + 新增用户 {{ u }}
            </div>
            <div v-for="(u, i) in diff.diff.users.removed ?? []" :key="'r' + i" class="nd__diff-del">
              − 移除用户 {{ u }}
            </div>
            <div v-for="(u, i) in diff.diff.users.uuid_reset ?? []" :key="'u' + i" class="nd__diff-add">
              ± 重置 UUID {{ u }}
            </div>
            <div v-for="(a, i) in diff.diff.node_attr ?? []" :key="'n' + i" class="nd__diff-add">
              ± 节点属性 {{ a }}
            </div>
          </div>
          <a-button type="primary" size="small" @click="confirmDeploy">
            部署 rev {{ node.config_revision + 1 }}
          </a-button>
        </template>
      </section>

      <section v-else-if="panel === 'dest' && destResults.length" class="nd__panel">
        <div class="nd__panel-head">
          <span class="nd__panel-title">扫描握手目标 · 从节点本机实测 {{ destResults.length }} 个候选</span>
        </div>
        <div class="nd__dest">
          <div class="nd__dest-row nd__dest-row--head">
            <span>目标</span><span>握手</span><span>TLS 记录</span><span />
          </div>
          <div v-for="d in destResults" :key="d.server" class="nd__dest-row">
            <span class="lb-mono lb-ellipsis">{{ d.server }}:{{ d.port }}</span>
            <span class="lb-mono">{{ d.usable ? 'OK' : '超时' }}</span>
            <span class="lb-mono">{{ d.max_record_size || '—' }}</span>
            <span>
              <LbStatusTag
                v-if="node.reality_dest === d.server"
                :meta="{ text: '当前使用', shape: 'check', fg: '#1B7A4B', bg: '#E9F5EE', bd: '#C3E3D0' }"
              />
              <a-tooltip v-else-if="destBlockReason(d)" :title="destBlockReason(d)">
                <a-button size="small" disabled>应用</a-button>
              </a-tooltip>
              <a-button
                v-else
                size="small"
                :loading="running === '应用握手目标'"
                @click="applyDest(d.server)"
              >
                应用
              </a-button>
            </span>
          </div>
        </div>
        <div class="nd__panel-note">
          「应用」会再实测一次目标、通过后才写库,成功后需要部署才在节点上生效。
          这也是编辑表单里不让改握手目标的原因。
        </div>
      </section>

      <NodeTuningPanel
        v-else-if="panel === 'tune' && tuning"
        :node-id="node.id"
        :node-name="node.display_name || node.name"
        :report="tuning"
        @update:report="(r) => (tuning = r)"
        @busy="(label) => (running = label)"
        @close="panel = ''"
      />
      </a-modal>

      <a-tabs v-model:activeKey="tab" size="small" @change="syncTabToRoute">
        <a-tab-pane key="overview" tab="概览">
          <div class="nd__grid">
            <section class="nd__card">
              <div class="nd__card-head">连接与端口</div>
              <div class="nd__card-body">
                <div class="nd__kv">
                  <div>
                    <span>SSH{{ hostIsDomain ? '(域名,每次操作前重新解析)' : '' }}</span>
                    <b class="lb-mono">{{ node.ssh_user }}@{{ node.host }}:{{ node.ssh_port }}</b>
                  </div>
                  <!-- 中转机上没有 sing-box 入站,这三个端口在库里就是 0。
                       显示「公网 0 → 主机 0」会让排查的人以为服务没起来,
                       而真实情况是这台机器上根本没有这个概念 ——
                       客户端连的端口在「入口」里,一条转发规则一个。 -->
                  <template v-if="!isRelay">
                    <div>
                      <span>代理端口</span>
                      <b class="lb-mono">公网 {{ node.proxy_port }} → 主机 {{ node.listen_port }}</b>
                    </div>
                    <div><span>API 端口</span><b class="lb-mono">{{ node.api_port }} 仅回环</b></div>
                  </template>
                  <div v-else>
                    <span>入口</span>
                    <b><a @click="tab = 'entries'">见「入口」</a></b>
                  </div>
                  <div>
                    <span>IPv6</span>
                    <b class="lb-mono">
                      <template v-if="node.ipv6_address">
                        [{{ node.ipv6_address }}]:{{ node.ipv6_proxy_port || node.proxy_port }}
                        <template v-if="!node.ipv6_proxy_port">(端口跟随 IPv4)</template>
                      </template>
                      <template v-else>未配置(订阅中只有 IPv4 条目)</template>
                    </b>
                  </div>
                  <div><span>架构</span><b class="lb-mono">{{ node.arch || '未探测' }}</b></div>
                  <div><span>sing-box</span><b class="lb-mono">{{ node.singbox_version || '未安装' }}</b></div>
                  <!-- 内存与它推出来的 UDP 超时挨在一起。分开的话,一台机器
                       探测完突然变成「待部署」而管理员看不出是什么改了。
                       超时值取后端给的,不在前端按内存自己推。 -->
                  <div>
                    <span>内存</span>
                    <b class="lb-mono">{{ node.mem_total_mb ? node.mem_total_mb + ' MB' : '未探测' }}</b>
                  </div>
                  <div v-if="!isRelay">
                    <span>UDP 会话超时</span>
                    <b class="lb-mono">
                      {{ node.udp_timeout || 'sing-box 默认 5m' }}
                      <template v-if="node.udp_timeout">(按内存压短)</template>
                    </b>
                  </div>
                  <div class="nd__kv-wide">
                    <span>构建标签</span>
                    <b class="lb-mono lb-ellipsis" :title="node.singbox_build_tags">
                      {{ node.singbox_build_tags || '—' }}
                    </b>
                  </div>
                </div>
              </div>
              <div v-if="node.listen_port !== node.proxy_port" class="nd__card-foot">
                公网端口与主机端口不同 —— 需自行配置端口转发,面板只让 sing-box 监听主机端口。
              </div>
            </section>

            <!-- 落地协议、握手目标、TFO 全是 sing-box 入站的属性。
                 中转机上这些列保持着建表时的默认值,渲染出来就是一份
                 从来没有生效过的配置 —— 看起来像是配好了。 -->
            <section v-if="!isRelay" class="nd__card">
              <div class="nd__card-head">
                落地协议与配置版本
                <a v-if="!isSS" @click="doScanDests">扫描握手目标</a>
              </div>
              <div class="nd__card-body">
                <div class="nd__kv">
                  <!-- 「期望」与「节点上生效」分两行,不合成一行。
                       合起来只能显示其中一个:显示期望值会让管理员以为切换已经
                       完成(而节点上还是旧协议),显示生效值又看不出他刚才改过。 -->
                  <div>
                    <span>期望协议</span>
                    <b>{{ PROTOCOL_LABEL[node.protocol] }}</b>
                  </div>
                  <div>
                    <span>节点上生效</span>
                    <b :style="{ color: protocolMismatch ? color.warning : undefined }">
                      {{
                        node.deployed_protocol
                          ? PROTOCOL_LABEL[node.deployed_protocol]
                          : '从未部署'
                      }}
                    </b>
                  </div>
                  <template v-if="isSS">
                    <div class="nd__kv-wide">
                      <span>加密方法</span>
                      <b class="lb-mono">{{ node.ss_method || '—' }}</b>
                    </div>
                  </template>
                  <template v-else>
                    <div><span>握手目标</span><b class="lb-mono">{{ node.reality_dest || '未设置' }}<template v-if="node.reality_dest">:{{ node.reality_dest_port }}</template></b></div>
                    <div>
                      <span>最大 TLS 记录</span>
                      <b
                        class="lb-mono"
                        :style="{
                          color: node.handshake_max_record_size > 8192 ? color.danger : undefined,
                        }"
                      >
                        {{ node.handshake_max_record_size || '未实测' }} / 8192
                      </b>
                    </div>
                  </template>
                  <!-- 与协议一样分两行。TFO 必须两端一致才有意义,而"改了没部署"
                       的那段时间里订阅下发的是旧值 —— 只显示一个的话,
                       管理员看到「已开启」会以为客户端那边也已经在用了。 -->
                  <div>
                    <span>TCP Fast Open</span>
                    <b>{{ node.tcp_fast_open ? '已开启' : '未开启' }}</b>
                  </div>
                  <div>
                    <span>节点上生效</span>
                    <b
                      :style="{
                        color:
                          node.tcp_fast_open !== node.deployed_tcp_fast_open
                            ? color.warning
                            : undefined,
                      }"
                    >
                      {{ node.deployed_tcp_fast_open ? '已开启' : '未开启' }}
                    </b>
                  </div>
                  <div><span>配置版本</span><b class="lb-mono">rev {{ node.config_revision }}</b></div>
                  <div>
                    <span>已部署配置</span>
                    <!-- 判空要看原值,不能靠 shortHash 的返回值:它对空串返回的是
                         「—」而不是空,`|| '从未部署'` 永远走不到。而「—」在本项目里
                         的含义是「读取失败」,与「这台机器还没部署过」正好是两回事。 -->
                    <b class="lb-mono" :title="node.deployed_config_sha256">
                      {{ node.deployed_config_sha256 ? shortHash(node.deployed_config_sha256) : '从未部署' }}
                    </b>
                  </div>
                  <div v-if="!isSS" class="nd__kv-wide">
                    <span>上次实测</span>
                    <b><LbTimeText :value="node.handshake_checked_at" empty="从未实测" /></b>
                  </div>
                </div>
              </div>
              <div v-if="protocolMismatch" class="nd__card-foot">
                协议已改但还没部署。<strong>节点上仍在运行
                {{ PROTOCOL_LABEL[node.deployed_protocol as NodeProtocol] }},
                订阅里下发的也是它</strong> —— 现在的用户不会断线。
                部署之后才会切换到 {{ PROTOCOL_LABEL[node.protocol] }},届时所有人都要重新拉一次订阅。
              </div>
            </section>

            <section class="nd__card">
              <div class="nd__card-head">
                本周期流量
                <span v-if="cycle" class="nd__card-note">
                  {{
                    cycle.reset_cycle === 'MONTHLY'
                      ? `每月 ${cycle.reset_day} 日 00:00 UTC 重置`
                      : '不重置,统计创建以来的累计流量'
                  }}
                </span>
              </div>
              <div class="nd__card-body">
                <!-- 「不计」与「读不到」必须分开。后者带重试按钮,
                     而前者重试一万次也不会有数字。 -->
                <div v-if="!metered" class="nd__card-note">
                  {{ notMeteredReason || '中转主机,面板不计流量' }}
                </div>
                <LbEmptyState
                  v-else-if="trafficError"
                  variant="error"
                  title="流量数据暂时读不到"
                  @retry="reload"
                />
                <template v-else-if="cycle">
                  <LbQuotaBar
                    :used-bytes="cycle.used_bytes"
                    :quota-bytes="cycle.quota_bytes"
                    :warning-level="cycle.warning_level"
                    size="md"
                  />
                  <div class="nd__kv">
                    <!-- 两个口径都摆出来。只给折算值的话,管理员对不上 sing-box 的
                         数字;只给原值的话,又对不上 VPS 商的账单。 -->
                    <div>
                      <span>代理转发(sing-box 计数)</span>
                      <b class="lb-mono">{{ formatBytes(cycle.proxy_bytes) }}</b>
                    </div>
                    <div>
                      <span>主机口径{{ cycle.billing_factor > 1 ? `(双向 ×${cycle.billing_factor})` : '(出站 ×1)' }}</span>
                      <b class="lb-mono">{{ formatBytes(cycle.used_bytes) }}</b>
                    </div>
                    <div><span>上行</span><b class="lb-mono">{{ formatBytes(cycle.uplink_bytes) }}</b></div>
                    <div><span>下行</span><b class="lb-mono">{{ formatBytes(cycle.downlink_bytes) }}</b></div>
                    <div>
                      <span>剩余</span>
                      <!-- 不限量时后端给 null,不能当 0 用 —— 那会画成「剩余 0」。 -->
                      <b class="lb-mono">
                        {{ cycle.remaining_bytes === null ? '不限量' : formatBytes(cycle.remaining_bytes) }}
                      </b>
                    </div>
                    <div>
                      <span>周期起点</span>
                      <b><LbTimeText :value="cycle.period_start" mode="cycle" /></b>
                    </div>
                    <div class="nd__kv-wide">
                      <span>下次重置</span>
                      <b>
                        <LbTimeText v-if="cycle.next_reset_at" :value="cycle.next_reset_at" mode="cycle" />
                        <template v-else>不重置</template>
                      </b>
                    </div>
                  </div>
                  <div class="nd__card-foot">
                    额度只做统计与预警,不会停止 sing-box、不禁用节点,也不改订阅开关。
                    <template v-if="cycle.billing_factor > 1">
                      <br />
                      这台机器按<strong>进出合计</strong>计费:一次用户下载在网卡上要走两趟
                      (从源站收一份、再发给客户端一份),所以主机口径约是代理转发量的两倍。
                      额度填的就是 VPS 商给的数字。
                    </template>
                    <template v-else>
                      <br />
                      这台机器按<strong>出站</strong>计费,与 sing-box 的计数 1:1。
                      若你的 VPS 是进出合计计费,到编辑里把「计费口径」改成双向。
                    </template>
                    <br />
                    换算不含 TCP/IP 头、重传,以及系统更新、SSH 这些不走代理的流量,
                    实际账单通常还要再高几个百分点。
                  </div>
                </template>
              </div>
            </section>

            <section class="nd__card">
              <div class="nd__card-head">
                资源
                <span v-if="latest" class="nd__card-note">
                  采样 <LbTimeText :value="latest.collected_at" /> · 间隔 5 分钟
                </span>
              </div>
              <div class="nd__card-body">
                <!-- 这是空态不是错误态:采集是可选能力,配置里能关掉。 -->
                <LbEmptyState
                  v-if="!latest"
                  variant="empty"
                  title="还没有采样"
                  description="采集按固定间隔在后台进行,也可以点上面的「采集资源」立刻取一次。"
                />
                <div v-else class="nd__usage">
                  <div v-for="u in [
                    { label: 'CPU', pct: latest.cpu_percent, sub: `负载 ${latest.load1.toFixed(2)}` },
                    {
                      label: '内存',
                      pct: memPercent(latest),
                      sub: `${formatBytes(latest.mem_used_kb * 1024)} / ${formatBytes(latest.mem_total_kb * 1024)}`,
                    },
                    {
                      label: '磁盘',
                      pct: diskPercent(latest),
                      sub: `${formatBytes(latest.disk_used_kb * 1024)} / ${formatBytes(latest.disk_total_kb * 1024)}`,
                    },
                  ]" :key="u.label" class="nd__usage-row">
                    <span class="nd__usage-label">{{ u.label }}</span>
                    <span class="nd__usage-track">
                      <span
                        class="nd__usage-fill"
                        :style="{ width: Math.min(u.pct, 100) + '%', background: usageColor(u.pct) }"
                      />
                    </span>
                    <span class="lb-mono nd__usage-pct" :style="{ color: usageColor(u.pct) }">
                      {{ u.pct.toFixed(0) }}%
                    </span>
                    <span class="lb-mono nd__usage-sub">{{ u.sub }}</span>
                  </div>
                  <div class="nd__card-foot">
                    阈值 {{ threshold.usageWarn }}% 转黄、{{ threshold.usageDanger }}% 转红。
                    128MB 的机器内存曲线本来就贴着高位走,阈值定低了会天天报警。
                  </div>
                </div>
              </div>
            </section>
          </div>
        </a-tab-pane>

        <a-tab-pane key="metrics" tab="资源">
          <div class="nd__range">
            <a-segmented
              v-model:value="metricsHours"
              :options="[
                { label: '6h', value: 6 },
                { label: '24h', value: 24 },
                { label: '72h', value: 72 },
                { label: '168h', value: 168 },
              ]"
              size="small"
            />
            <span class="nd__card-note">只读库中已有采样,不主动触发 SSH 采集</span>
          </div>
          <LbEmptyState
            v-if="metricsHistory.length === 0"
            variant="empty"
            title="这段时间没有采样"
            description="采集间隔默认 5 分钟,保留 7 天。也可以点上面的「采集资源」立刻取一次。"
          />
          <template v-else>
            <!-- 百分比类固定纵轴上限 100:不固定的话 3% 的曲线会被拉满整张图,
                 看起来像 CPU 打满了。 -->
            <div class="nd__chart">
              <div class="nd__chart-title">CPU</div>
              <MetricsChart
                :labels="metricLabels"
                :series="cpuSeries"
                :format="formatPercent"
                :max-override="100"
              />
            </div>
            <div class="nd__chart">
              <div class="nd__chart-title">内存</div>
              <MetricsChart
                :labels="metricLabels"
                :series="memSeries"
                :format="formatPercent"
                :max-override="100"
              />
            </div>
            <div class="nd__chart">
              <div class="nd__chart-title">网速</div>
              <MetricsChart :labels="metricLabels" :series="netSeries" :format="formatRate" />
            </div>
          </template>
        </a-tab-pane>

        <a-tab-pane key="deploys" tab="部署历史">
          <LbEmptyState
            v-if="deployments.length === 0"
            variant="empty"
            title="还没有部署记录"
            description="执行第一次部署后,这里会记录每一步的结果。"
          />
          <a-table
            v-else
            :columns="deployColumns"
            :data-source="deployments"
            row-key="id"
            size="small"
            :pagination="{ pageSize: 10, size: 'small', hideOnSinglePage: true, showSizeChanger: false }"
            :expand-row-by-click="true"
          >
            <template #bodyCell="{ column, record }">
              <template v-if="column.key === 'rev'">
                <span class="lb-mono">{{ record.revision }}</span>
              </template>
              <template v-else-if="column.key === 'status'">
                <LbStatusTag kind="deploy" :status="record.status" />
              </template>
              <template v-else-if="column.key === 'time'">
                <LbTimeText :value="record.started_at" mode="both" />
              </template>
              <template v-else-if="column.key === 'cost'">
                <span class="lb-mono">{{ durationOf(record) }}</span>
              </template>
            </template>
            <template #expandedRowRender="{ record }">
              <DeployStepList :record="record" />
            </template>
          </a-table>
        </a-tab-pane>

        <a-tab-pane key="traffic" tab="流量">
          <div v-if="!metered" class="nd__card-note">
            {{ notMeteredReason || '中转主机,面板不计流量' }}
          </div>
          <LbEmptyState
            v-else-if="trafficError"
            variant="error"
            title="流量数据暂时读不到"
            @retry="reload"
          />
          <template v-else>
            <LbSparkline :points="dailyPoints" type="bar" :height="140" />
            <div class="nd__card-note nd__spark-cap">
              近 30 天 · 按 UTC 日聚合 · 悬停查看当日用量 · 空心柱表示当天没有记录(不补 0、不插值)
            </div>
          </template>
        </a-tab-pane>

        <a-tab-pane key="entries" tab="入口">
          <NodeEntriesPanel
            :key="node.id"
            :node="node"
            :sub-entries="subEntries"
            @busy="(label) => (running = label)"
            @changed="reload"
          />
        </a-tab-pane>
      </a-tabs>
    </template>
  </div>

  <!-- 部署一旦发出就在节点上跑,关掉弹窗不会取消它 —— 所以执行中干脆不让关。
       z-index 高于 Modal.confirm 的默认 1000:确认框收起有一段淡出动画,
       同层的话进度弹窗会在那两百毫秒里被盖住半截。 -->
  <a-modal
    v-model:open="deployOpen"
    :title="deployRunning ? '正在部署' : '部署结果'"
    :width="620"
    :z-index="1100"
    :closable="!deployRunning"
    :mask-closable="false"
    :keyboard="!deployRunning"
    :footer="null"
  >
    <div v-if="deployRunning" class="nd__deploying">
      <a-spin />
      <div>
        <div class="nd__deploying-title">正在部署到 {{ node?.display_name || node?.name }}</div>
        <div class="nd__deploying-note">
          七个步骤在节点上顺序执行,通常 15~25 秒。步骤明细会在完成后一次给出 ——
          部署接口是同步的,中途没有可上报的进度。
        </div>
      </div>
    </div>
    <template v-else-if="deployAsRecord">
      <div
        class="nd__deploy-verdict"
        :class="deployAsRecord.status === 'SUCCESS' ? 'nd__deploy-verdict--ok' : 'nd__deploy-verdict--bad'"
      >
        <LbStatusTag kind="deploy" :status="deployAsRecord.status" />
        <span>
          {{
            deployAsRecord.status === 'SUCCESS'
              ? `rev ${deployAsRecord.revision} 已生效,sing-box 已重启,计数器基线已重置`
              : '节点当前运行的是回滚后的配置,用户未受影响'
          }}
        </span>
      </div>
      <DeployStepList :record="deployAsRecord" />
      <div class="nd__deploy-foot">
        <a-button type="primary" @click="deployOpen = false">完成</a-button>
      </div>
    </template>
  </a-modal>

  <!-- 唯一一个需要管理员再次提供口令的动作,所以必须是弹窗而不是工具条上的一键按钮。 -->
  <a-modal
    v-model:open="bootstrapOpen"
    title="重新引导节点"
    :width="460"
    ok-text="引导"
    cancel-text="取消"
    :mask-closable="false"
    @ok="doBootstrap"
  >
    <p class="nd__boot-note">
      用节点口令登录一次,把面板公钥重新装进 <code class="lb-mono">authorized_keys</code>。
      已有的 sing-box 与配置不受影响。
    </p>
    <a-form layout="vertical">
      <a-form-item label="节点登录密码" required>
        <a-input-password v-model:value="bootstrapPassword" autocomplete="new-password" />
        <div class="nd__boot-help">用完即弃:不保存、不写日志、不进审计详情。</div>
      </a-form-item>
    </a-form>
  </a-modal>

  <LbNameConfirm
    v-if="nameConfirmMeta && node"
    :open="nameConfirm !== null"
    :title="nameConfirmMeta.title"
    :name="node.name"
    :ok-text="nameConfirmMeta.okText"
    :impacts="nameConfirmMeta.impacts"
    :loading="nameConfirmLoading"
    prompt="输入内部名称以确认"
    @update:open="(v) => (nameConfirm = v ? nameConfirm : null)"
    @confirm="doNameConfirm"
  />

  <!-- 编辑表单由本页托管。抽屉时期它挂在列表页上,而整页之后列表页
       不再知道谁被打开了 —— 让它继续留在那边,就要把"当前正在编辑哪个节点"
       这件事在两个页面之间同步一遍,而那是一个必然会不同步的状态。 -->
  <NodeFormModal
    v-if="node"
    :open="editOpen"
    :node="node"
    :tiers="tiers"
    @update:open="(v) => (editOpen = v)"
    @saved="reload"
  />
</template>

<style scoped>
/* 整页容器。抽屉时期外层 padding 由 body-style 给,现在由页面自己负责。 */
.nd {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.nd__back {
  align-self: flex-start;
  font-size: 13px;
}

/* 标题与操作按钮同一行。窄屏换行 —— 桌面的紧凑排布可以缩间距,
   但不能把「部署」挤出可视区,那是这个页面唯一的主按钮。 */
.nd__bar {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.nd__head {
  display: flex;
  flex-direction: column;
  gap: 3px;
}

.nd__title {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.nd__name {
  font-size: 16px;
  font-weight: 600;
}

.nd__sub {
  font-size: 11px;
  font-weight: 400;
  color: #6b7480;
}

.nd__skel {
  display: flex;
  flex-direction: column;
  gap: 20px;
  padding-top: 16px;
}

.nd__fail {
  margin-top: 12px;
  padding: 12px 14px;
  background: #fdecea;
  border: 1px solid #f3cfc9;
  border-radius: 6px;
}

.nd__fail-title {
  font-size: 13px;
  font-weight: 600;
  color: #8e2117;
}

.nd__fail-body {
  margin-top: 5px;
  font-size: 12px;
  line-height: 1.75;
  color: #8e2117;
  white-space: pre-wrap;
}

.nd__fail-rb {
  margin-top: 5px;
  font-size: 11.5px;
  color: #8e2117;
}

.nd__fail-acts {
  display: flex;
  gap: 14px;
  margin-top: 8px;
  font-size: 12px;
}

.nd__badges {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 12px;
}

.nd__maint {
  font-size: 11.5px;
  color: #5f52a0;
}

.nd__tools {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  margin-top: 12px;
  padding: 10px 12px;
  background: #f6f7f9;
  border: 1px solid #edeff2;
  border-radius: 6px;
}

.nd__tools-label {
  font-size: 11.5px;
  font-weight: 600;
  color: #576070;
  margin-right: 2px;
}

.nd__tools-note {
  margin-left: auto;
  font-size: 11px;
  color: #6b7480;
}

.nd__tools-running {
  margin-left: auto;
  font-size: 11px;
  color: #2563b8;
}

.nd__panel {
  margin-top: 12px;
  padding: 12px 14px;
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.nd__panel-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  font-size: 12.5px;
}

.nd__panel-title {
  font-weight: 600;
}

.nd__panel-note {
  font-size: 11.5px;
  line-height: 1.7;
  color: #6b7480;
}

.nd__panel-ok {
  font-size: 12px;
  color: #1b7a4b;
}

.nd__panel-warn {
  padding: 9px 11px;
  background: #fcf3e3;
  border: 1px solid #efdcb4;
  border-radius: 6px;
  font-size: 11.5px;
  line-height: 1.7;
  color: #5c4405;
}

.nd__pre {
  margin: 0;
  padding: 9px 11px;
  background: #f6f7f9;
  border: 1px solid #edeff2;
  border-radius: 4px;
  font-size: 11px;
  line-height: 1.7;
  color: #576070;
  white-space: pre-wrap;
  overflow-x: auto;
}

.nd__kv {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 10px 20px;
}

.nd__kv > div {
  display: flex;
  flex-direction: column;
  gap: 3px;
  min-width: 0;
}

.nd__kv-wide {
  grid-column: 1 / -1;
}

.nd__kv span {
  font-size: 11.5px;
  color: #6b7480;
}

.nd__kv b {
  font-size: 12.5px;
  font-weight: 500;
}

.nd__diff-sum {
  font-size: 12px;
  color: #576070;
}

.nd__diff {
  padding: 9px 11px;
  background: #f6f7f9;
  border: 1px solid #edeff2;
  border-radius: 4px;
  font-family: 'IBM Plex Mono', ui-monospace, monospace;
  font-size: 11.5px;
  line-height: 1.9;
}

.nd__diff-add {
  color: #1b7a4b;
}

.nd__diff-del {
  color: #b4291d;
}

.nd__dest {
  border: 1px solid #edeff2;
  border-radius: 6px;
  overflow: hidden;
}

.nd__dest-row {
  display: grid;
  grid-template-columns: 1.6fr 0.6fr 0.7fr 0.9fr;
  align-items: center;
  gap: 10px;
  padding: 8px 11px;
  font-size: 12px;
}

.nd__dest-row + .nd__dest-row {
  border-top: 1px solid #edeff2;
}

.nd__dest-row--head {
  background: #f6f7f9;
  font-size: 11px;
  font-weight: 600;
  color: #576070;
}

.nd__grid {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.nd__card {
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

.nd__card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 11px 16px;
  border-bottom: 1px solid #edeff2;
  font-size: 13px;
  font-weight: 600;
}

.nd__card-note {
  font-size: 11px;
  font-weight: 400;
  color: #6b7480;
}

.nd__card-body {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 16px;
}

.nd__card-foot {
  padding: 10px 16px;
  border-top: 1px solid #edeff2;
  font-size: 11px;
  line-height: 1.7;
  color: #6b7480;
}

.nd__usage {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.nd__usage-row {
  display: grid;
  grid-template-columns: 44px 1fr 44px auto;
  align-items: center;
  gap: 10px;
  font-size: 11.5px;
}

.nd__usage-label {
  color: #6b7480;
}

.nd__usage-track {
  height: 6px;
  background: #edeff2;
  border-radius: 2px;
  overflow: hidden;
}

.nd__usage-fill {
  display: block;
  height: 6px;
  border-radius: 2px;
}

.nd__usage-pct {
  text-align: right;
}

.nd__usage-sub {
  font-size: 10.5px;
  color: #6b7480;
}

.nd__chart {
  margin-bottom: 16px;
}

.nd__chart-title {
  margin-bottom: 4px;
  font-size: 12.5px;
  font-weight: 500;
}

.nd__range {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 12px;
}

.nd__spark-cap {
  margin-top: 4px;
}

.nd__sub-list {
  border: 1px solid #edeff2;
  border-radius: 6px;
  overflow: hidden;
}

.nd__sub-item {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  padding: 10px 12px;
  font-size: 12.5px;
}

.nd__sub-item + .nd__sub-item {
  border-top: 1px solid #edeff2;
}

.nd__sub-name {
  font-weight: 500;
}

.nd__sub-addr {
  font-size: 11.5px;
  color: #6b7480;
}

.nd__deploying {
  display: flex;
  align-items: flex-start;
  gap: 14px;
  padding: 12px 0;
}

.nd__deploying-title {
  font-size: 13px;
  font-weight: 600;
}

.nd__deploying-note {
  margin-top: 5px;
  font-size: 12px;
  line-height: 1.75;
  color: #6b7480;
}

.nd__deploy-verdict {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 14px;
  padding: 10px 12px;
  border-radius: 6px;
  font-size: 12px;
}

.nd__deploy-verdict--ok {
  background: #e9f5ee;
  border: 1px solid #c3e3d0;
  color: #14603b;
}

.nd__deploy-verdict--bad {
  background: #fdecea;
  border: 1px solid #f3cfc9;
  color: #8e2117;
}

.nd__deploy-foot {
  display: flex;
  justify-content: flex-end;
  margin-top: 16px;
}

.nd__boot-note {
  margin: 0 0 14px;
  font-size: 12.5px;
  line-height: 1.75;
  color: #576070;
}

.nd__boot-note code,
.nd__panel-title code {
  padding: 1px 5px;
  background: #f1f3f5;
  border-radius: 3px;
  font-size: 12px;
}

.nd__boot-help {
  margin-top: 4px;
  font-size: 12px;
  color: #6b7480;
}

@media (max-width: 767px) {
  .nd__kv {
    grid-template-columns: 1fr;
  }

  .nd__dest-row {
    grid-template-columns: 1fr auto;
  }
}
</style>
