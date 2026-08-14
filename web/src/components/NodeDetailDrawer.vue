<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message, Modal } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AccessTier,
  type ConfigDiff,
  type DailyPoint,
  type DeployResult,
  type DeploymentRecord,
  type DestCheckResult,
  type Node,
  type NodeCycleUsage,
  type NodeMetrics,
  type ProbeResult,
} from '@/api/client'
import { formatBytes, formatDuration, shortHash } from '@/utils/format'
import DeployStepList from '@/components/DeployStepList.vue'
import MetricsChart from '@/components/MetricsChart.vue'
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
import { color, threshold, usageColor } from '@/theme/tokens'

/**
 * 节点详情。原实现是 12 个 size="small" 按钮排成一排 ——
 * 只读的「探测」和会抹掉节点服务的「卸载服务」尺寸完全相同,仅靠 danger 变红区分,
 * 手滑一格的代价差了几个数量级。
 *
 * 这里分三层:
 *   只读检查(6 个)  常驻工具条。点错了最坏结果是白等几秒。
 *   部署            唯一的主按钮 —— 它是这个页面存在的理由。
 *   会改变节点的     全部进「⋯」,不可逆的四项在分隔线之下。
 */
const props = defineProps<{ nodeId: number | null; tiers: AccessTier[] }>()
const emit = defineEmits<{ close: []; changed: []; edit: [node: Node] }>()

const node = ref<Node | null>(null)
const loading = ref(false)
const loadError = ref<{ message: string; status?: number; at: string } | null>(null)
const tab = ref('overview')

const deployments = ref<DeploymentRecord[]>([])
const daily = ref<DailyPoint[]>([])
const cycle = ref<NodeCycleUsage | null>(null)
const trafficError = ref(false)

/** 工具条上正在跑的动作名。同一时刻只允许一个。 */
const running = ref('')

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
  api
    .nodeTraffic(id, 30)
    .then((r) => {
      daily.value = r.daily
      cycle.value = r.cycle
    })
    .catch(() => {
      daily.value = []
      cycle.value = null
      trafficError.value = true
    })
  loadMetrics(id)
}

function reload() {
  if (props.nodeId !== null) load(props.nodeId)
}

// ---------- 只读检查 ----------
//
// 结果贴在工具条下方的面板里,不用吐司:探测会写回架构、版本、构建标签,
// 用一条三秒吐司交付「已更新 3 项」等于让管理员自己去 descriptions 里找哪里变了。

const sshResult = ref<{ ok: boolean; text: string } | null>(null)
const probe = ref<ProbeResult | null>(null)
const diff = ref<ConfigDiff | null>(null)
const destResults = ref<DestCheckResult[]>([])
/** 当前展开的结果面板。同一时刻只显示一个,免得往下堆四块。 */
const panel = ref<'' | 'ssh' | 'probe' | 'diff' | 'dest'>('')

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
    const r = await api.testNodeSSH(props.nodeId!)
    sshResult.value = { ok: true, text: r.uname }
  })

const doProbe = () =>
  readonlyAction('探测', 'probe', async () => {
    probe.value = await api.probeNode(props.nodeId!)
    emit('changed')
    // 探测会写回节点档案,重新读一次让上面的字段跟着变。
    node.value = await api.node(props.nodeId!)
  })

const doDiff = () =>
  readonlyAction('比对配置', 'diff', async () => {
    diff.value = await api.nodeConfigDiff(props.nodeId!)
  })

const doScanDests = () =>
  readonlyAction('扫描握手目标', 'dest', async () => {
    destResults.value = (await api.scanNodeDests(props.nodeId!)).items
  })

const doSyncTraffic = () =>
  readonlyAction('同步流量', '', async () => {
    const r = await api.syncNodeTraffic(props.nodeId!)
    // 这个只更新数字,结果就在页面上 —— 吐司 + 就地刷新即可,不用开面板。
    message.success(`流量已同步 · 新增 ${formatBytes(r.bytes_added)}`)
    panel.value = ''
    reload()
  })

const doCollectMetrics = () =>
  readonlyAction('采集资源', '', async () => {
    const m = await api.collectNodeMetrics(props.nodeId!)
    message.success(`已采集 · 内存 ${memPercent(m).toFixed(0)}%`)
    panel.value = ''
    await loadMetrics(props.nodeId!)
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
    await api.checkNodeDest(props.nodeId!, dest, true)
    message.success(`已应用 ${dest},需要部署才在节点上生效`)
    destResults.value = []
    panel.value = ''
    emit('changed')
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
    deployResult.value = await api.deployNode(props.nodeId!)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '部署失败')
    deployOpen.value = false
  } finally {
    deployRunning.value = false
    running.value = ''
    emit('changed')
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
    emit('changed')
    reload()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : `${label}失败`)
  } finally {
    running.value = ''
  }
}

const doInstall = () =>
  run('安装 sing-box', async () => {
    const r = await api.installNode(props.nodeId!)
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
    onOk: () => run('重启', () => api.restartNode(props.nodeId!), '已重启'),
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
  const id = props.nodeId
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
    emit('changed')
    if (kind === 'delete') emit('close')
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
  const id = props.nodeId!
  const password = bootstrapPassword.value
  bootstrapOpen.value = false
  // 口令用完立刻抹掉,不留在组件状态里,也不进日志与审计详情。
  bootstrapPassword.value = ''

  running.value = '重新引导'
  try {
    const r = await api.bootstrapNode(id, password)
    message.success(r.already_present ? '节点上已有面板公钥,连接正常' : '面板公钥已装入并验证通过')
    emit('changed')
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
  if (props.nodeId !== null) loadMetrics(props.nodeId)
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
  () => props.nodeId,
  (id) => {
    probe.value = null
    diff.value = null
    destResults.value = []
    sshResult.value = null
    panel.value = ''
    metricsHistory.value = []
    deployments.value = []
    cycle.value = null
    tab.value = 'overview'
    if (id !== null) load(id)
    else node.value = null
  },
  { immediate: true },
)

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
  <!-- 有检查在跑时不接受遮罩点击与 ESC。
       探测、扫描握手目标这类动作要几秒到十几秒,期间页面上只有一个按钮在转圈,
       而抽屉外面整片都是遮罩 —— 随手点一下就把它关了,几秒后结果返回时已经
       没有地方可以呈现,看起来就是「点了探测,等了一会儿,详情页自己没了」。
       右上角的 × 照常可用:那是明确的关闭意图,不是误触。 -->
  <a-drawer
    :open="nodeId !== null"
    :width="720"
    :mask-closable="running === ''"
    :keyboard="running === ''"
    :body-style="{ padding: '0 20px 20px' }"
    @close="emit('close')"
  >
    <template #title>
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
          {{ node.name }} · {{ node.host }} · 公网 {{ node.proxy_port }} → 主机
          {{ node.listen_port }} · SSH {{ node.ssh_port }}
          <!-- 带端口显示 IPv6 必须加方括号:2a02:…::1:9443 分不清哪一段是端口。 -->
          <template v-if="node.ipv6_address">
            · IPv6 [{{ node.ipv6_address }}]:{{ node.ipv6_proxy_port || node.proxy_port }}
          </template>
        </div>
      </div>
    </template>

    <template #extra>
      <a-space v-if="node">
        <!-- 库里的配置已经在节点上生效时不做成主按钮:那一下点下去只会白白
             重启一次 sing-box、断掉全部在线连接,换回一模一样的配置。 -->
        <a-button
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
                <a-menu-item @click="emit('edit', node!)">编辑节点</a-menu-item>
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
    </template>

    <LbEmptyState
      v-if="loadError"
      variant="error"
      :title="loadError.status === 404 ? '节点不存在或已被删除' : loadError.message"
      description="列表可能已经过期。关闭抽屉会自动刷新一次列表。"
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
        <a-button size="small" :loading="running === '比对配置'" @click="doDiff">比对配置</a-button>
        <a-button size="small" :loading="running === '扫描握手目标'" @click="doScanDests">
          扫描握手目标
        </a-button>
        <a-button size="small" :loading="running === '同步流量'" @click="doSyncTraffic">同步流量</a-button>
        <a-button size="small" :loading="running === '采集资源'" @click="doCollectMetrics">
          采集资源
        </a-button>
        <!-- 正在跑什么必须写出来。只有按钮上一个小转圈的话,管理员会以为没点上
             而反复点,也不明白为什么这时候点别处关不掉抽屉。 -->
        <span v-if="running" class="nd__tools-running">
          {{ running }}中…&nbsp;结果稍后显示在下方,期间点击别处不会关掉本页
        </span>
        <span v-else class="nd__tools-note">都不改动节点状态</span>
      </div>

      <!-- 只读动作的结果面板。探测会写回三四个字段,一条吐司交付不了。 -->
      <section v-if="panel === 'ssh' && sshResult" class="nd__panel">
        <div class="nd__panel-head">
          <LbStatusTag
            :meta="
              sshResult.ok
                ? { text: 'SSH 连接正常', shape: 'check', fg: '#1B7A4B', bg: '#E9F5EE', bd: '#C3E3D0' }
                : { text: 'SSH 连接失败', shape: 'cross', fg: '#B4291D', bg: '#FDECEA', bd: '#F3CFC9' }
            "
          />
          <a @click="panel = ''">收起</a>
        </div>
        <pre class="nd__pre lb-mono">{{ sshResult.text }}</pre>
        <div v-if="!sshResult.ok" class="nd__panel-note">
          部署按钮此时仍然可点 —— 由后端拒绝并给出同样的原因,前端不自作主张禁用。
        </div>
      </section>

      <section v-else-if="panel === 'probe' && probe" class="nd__panel">
        <div class="nd__panel-head">
          <span class="nd__panel-title">探测完成 · 节点档案已更新</span>
          <a @click="panel = ''">收起</a>
        </div>
        <div class="nd__kv">
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
          <a @click="panel = ''">收起</a>
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
          <a @click="panel = ''">收起</a>
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

      <a-tabs v-model:activeKey="tab" size="small">
        <a-tab-pane key="overview" tab="概览">
          <div class="nd__grid">
            <section class="nd__card">
              <div class="nd__card-head">连接与端口</div>
              <div class="nd__card-body">
                <div class="nd__kv">
                  <div><span>SSH</span><b class="lb-mono">{{ node.ssh_user }}@{{ node.host }}:{{ node.ssh_port }}</b></div>
                  <div>
                    <span>代理端口</span>
                    <b class="lb-mono">公网 {{ node.proxy_port }} → 主机 {{ node.listen_port }}</b>
                  </div>
                  <div><span>API 端口</span><b class="lb-mono">{{ node.api_port }} 仅回环</b></div>
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

            <section class="nd__card">
              <div class="nd__card-head">
                REALITY 与配置版本
                <a @click="doScanDests">扫描握手目标</a>
              </div>
              <div class="nd__card-body">
                <div class="nd__kv">
                  <div><span>握手目标</span><b class="lb-mono">{{ node.reality_dest }}:{{ node.reality_dest_port }}</b></div>
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
                  <div class="nd__kv-wide">
                    <span>上次实测</span>
                    <b><LbTimeText :value="node.handshake_checked_at" empty="从未实测" /></b>
                  </div>
                </div>
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
                <LbEmptyState
                  v-if="trafficError"
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
          <LbEmptyState
            v-if="trafficError"
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

        <a-tab-pane key="sub" tab="订阅展开">
          <div class="nd__sub-list">
            <div v-for="e in subEntries" :key="e.name" class="nd__sub-item">
              <span class="nd__sub-name">{{ e.name }}</span>
              <span class="lb-mono nd__sub-addr">{{ e.addr }}</span>
            </div>
          </div>
          <div class="nd__card-foot">
            <template v-if="node.ipv6_address">
              两条指向<strong>同一个 sing-box 入站</strong>,UUID、REALITY 公钥、short ID 完全相同。
              改 IPv6 保存即生效,不需要重新部署。
            </template>
            <template v-else>
              未配置 IPv6,订阅里只有一条。填上 IPv6 后会额外生成
              「{{ node.display_name || node.name }}-IPV6」,同样不需要重新部署。
            </template>
          </div>
          <div v-if="!node.subscription_enabled" class="nd__panel-warn">
            该节点已关闭「下发到用户订阅」,以上条目不会进入新生成的订阅。
            节点仍在运行,已导入旧订阅的客户端还能用。
          </div>
        </a-tab-pane>
      </a-tabs>
    </template>
  </a-drawer>

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
</template>

<style scoped>
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
