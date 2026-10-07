<script setup lang="ts">
import { computed, onErrorCaptured, onMounted, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AccessTier,
  type Node,
  type NodeHealth,
  type NodeConfigState,
  type NodeCycleUsage,
  type NodeMetrics,
} from '@/api/client'
import NodeFormModal from '@/components/node/NodeFormModal.vue'
import ExpiryModal from '@/components/expiry/ExpiryModal.vue'
import NodeOpProgressModal from '@/components/node/NodeOpProgressModal.vue'
import { useNodeRecheck } from '@/components/node/useNodeRecheck'
import { summarizeHealth, type HealthSummary } from '@/components/node/healthSummary'
import { confirmDeployNode } from '@/components/node/nodeOps'
import {
  expiryFilterOptions,
  expiryStatusMeta,
  matchExpiryFilter,
  type ExpiryFilter,
} from '@/components/expiry/expiryMeta'
import {
  LbBatchBar,
  LbCopyField,
  LbEmptyState,
  LbFilterBar,
  LbIcon,
  LbInfoTip,
  LbMetricCard,
  LbQuotaBar,
  LbResultList,
  LbRowCard,
  LbSectionTitle,
  LbStatusTag,
  configStatusMeta,
  lbDangerConfirm,
  type LbResultItem,
} from '@/components/lb'
import { useNarrow } from '@/composables/useNarrow'
import { usePagination } from '@/composables/usePagination'
import { configState, needsDeploy, nodeBadges } from '@/components/lb/derive'
import { daysUntil, formatBytes, formatDate, formatUTCDay, formatUTCTime } from '@/utils/format'

/**
 * 节点列表(V20 精简版)。
 *
 * 打开这一页要能一眼回答六件事:哪台机器、什么地址、是否正常、用了多少流量、
 * 何时重置、何时续费。所以默认列只留:编号 / 节点 / 管理 IP / 服务巡检 / 本周期流量 /
 * 到期时间 / 操作。原来独立的「运行」「配置」「云实例」「最后同步」四列去掉了 ——
 * 运行与配置状态挪到节点名旁边(**仍是两个独立标签**,一台在跑旧配置、上次部署失败的
 * 机器合成一个标签就看不出它其实还在服务用户);云实例停机、流量同步失败这类重要异常
 * 进「服务巡检」的摘要,不随删列消失。
 *
 * 名称区只显示内部名称:订阅名称、协议、端口、转发关系都在详情的对应分组里。
 * 搜索仍然按内部名称、订阅名称与地址找 —— 用户报障时给的往往是订阅名或地址。
 */
const narrow = useNarrow()
const nodes = ref<Node[]>([])
const tiers = ref<AccessTier[]>([])
const loading = ref(true)
const loadError = ref<{ message: string; status?: number; at: string } | null>(null)

/** 列级数据:各自降级。一整列的「—」看起来像所有机器都挂了,所以要显式说出来。 */
const cycles = ref<Record<number, NodeCycleUsage>>({})
const cycleError = ref(false)
const metrics = ref<Record<number, NodeMetrics>>({})
const metricsError = ref(false)
/** 流量同步最近一轮在哪些机器上失败了(进巡检摘要的异常徽标)。 */
const syncErrors = ref<Record<number, string>>({})

const router = useRouter()

function openDetail(id: number, tab?: string) {
  router.push({ name: 'node-detail', params: { id: String(id), ...(tab ? { tab } : {}) } })
}
const panelKey = ref('')

// ---------- 筛选 ----------

const blankFilters = {
  keyword: '',
  run: undefined as string | undefined,
  config: undefined as NodeConfigState | undefined,
  tierID: undefined as number | undefined,
  expiry: 'ALL' as ExpiryFilter,
  subOff: false,
}
const filters = reactive({ ...blankFilters })

const activeFilterCount = computed(
  () =>
    (filters.keyword.trim() ? 1 : 0) +
    (filters.run !== undefined ? 1 : 0) +
    (filters.config !== undefined ? 1 : 0) +
    (filters.tierID !== undefined ? 1 : 0) +
    (filters.expiry !== 'ALL' ? 1 : 0) +
    (filters.subOff ? 1 : 0),
)

function clearFilters() {
  Object.assign(filters, blankFilters)
}

const visible = computed(() =>
  nodes.value
    .filter((n) => {
      const kw = filters.keyword.trim().toLowerCase()
      if (kw) {
        // 列表上不再常驻展示订阅名称与地址,但它们必须仍然搜得到:
        // 用户报障时给的是订阅名或地址,而管理员手上只有这个搜索框。
        const hay = [n.name, n.display_name, n.host, n.sub_ipv4_address, n.ipv6_address]
          .join(' ')
          .toLowerCase()
        if (!hay.includes(kw)) return false
      }
      if (filters.run !== undefined && n.status !== filters.run) return false
      if (filters.config !== undefined && configState(n) !== filters.config) return false
      if (
        filters.tierID !== undefined &&
        !(n.inbounds ?? []).some((i) => i.access_tier_id === filters.tierID)
      ) {
        return false
      }
      if (!matchExpiryFilter(n.expiry, filters.expiry)) return false
      if (filters.subOff && n.subscription_enabled) return false
      return true
    })
    // 与订阅、门户同一个顺序:排序值升序,相同则按 id。改完排序值到下一次 load()
    // 之间列表还是旧顺序,所以这里再排一遍;id 兜底不能省 —— 全部留 0 时没有兜底
    // 就是不稳定排序。
    .sort((a, b) => a.sort_order - b.sort_order || a.id - b.id),
)

// ---------- 选择 ----------

const selected = ref<number[]>([])
watch(visible, () => {
  selected.value = selected.value.filter((id) => visible.value.some((n) => n.id === id))
})
const rowSelection = computed(() => ({
  selectedRowKeys: selected.value,
  onChange: (keys: (string | number)[]) => (selected.value = keys.map(Number)),
}))

// ---------- 指标 ----------

const stats = computed(() => ({
  online: nodes.value.filter((n) => n.status === 'ONLINE').length,
  total: nodes.value.length,
  pending: nodes.value.filter((n) => needsDeploy(n)).length,
  subOff: nodes.value.filter((n) => !n.subscription_enabled).length,
  expiring: nodes.value.filter((n) => n.expiry?.state === 'SOON' || n.expiry?.state === 'OVERDUE').length,
  cycleUsed: Object.values(cycles.value).reduce((s, c) => s + c.used_bytes, 0),
}))

const metricState = computed(() =>
  loadError.value ? 'error' : loading.value ? 'loading' : nodes.value.length ? 'ready' : 'empty',
)

const summaryLine = computed(() => {
  if (loadError.value) return '节点列表读取失败。'
  if (loading.value && !nodes.value.length) return '正在读取节点…'
  const s = stats.value
  if (!s.total) return '还没有添加任何机器。'
  const head = `${s.total} 台机器,${s.online} 台在线。`
  const tails: string[] = []
  if (s.pending) tails.push(`${s.pending} 台待部署`)
  if (s.subOff) tails.push(`${s.subOff} 台已停发订阅`)
  if (s.expiring) tails.push(`${s.expiring} 台即将到期或已到期`)
  return tails.length ? `${head}${tails.join(',')}。` : head
})

const billedCount = computed(
  () => Object.values(cycles.value).filter((c) => c.quota_bytes > 0).length,
)

const offlineCount = computed(() => nodes.value.filter((n) => n.status === 'OFFLINE').length)

// ---------- 取数 ----------

async function load() {
  loading.value = true
  loadError.value = null
  try {
    const [n, t] = await Promise.all([api.nodes(), api.accessTiers()])
    nodes.value = n.items
    tiers.value = t.items
  } catch (err) {
    loadError.value = {
      message: err instanceof ApiError ? err.message : '加载节点列表失败',
      status: err instanceof ApiError ? err.status : undefined,
      at: new Date().toLocaleTimeString(),
    }
    nodes.value = []
  } finally {
    loading.value = false
  }
  loadColumns()
}

/** 列级数据单独取。它们失败不能升级成整表失败 —— 资源采样本来就能在配置里关掉。 */
function loadColumns() {
  cycleError.value = false
  metricsError.value = false
  api
    .nodesCycleTraffic()
    .then((r) => (cycles.value = Object.fromEntries(r.items.map((c) => [c.node_id, c]))))
    .catch(() => {
      cycles.value = {}
      cycleError.value = true
    })
  api
    .nodeMetricsLatest()
    .then((r) => (metrics.value = Object.fromEntries(r.items.map((m) => [m.node_id, m]))))
    .catch(() => {
      metrics.value = {}
      metricsError.value = true
    })
  api
    .trafficStatus()
    .then((r) => (syncErrors.value = Object.fromEntries(r.failing_nodes.map((f) => [f.node_id, f.error]))))
    .catch(() => (syncErrors.value = {}))
}

onMounted(async () => {
  await load()
  void loadHealth()
  api.panelKey().then((r) => (panelKey.value = r.public_key)).catch(() => (panelKey.value = ''))
})

// ---------- 表单与行操作 ----------

const formOpen = ref(false)
const editing = ref<Node | null>(null)
const busy = ref<Record<number, string>>({})
/** 「续费 / 修改到期时间」弹窗(V20)。 */
const expiryTarget = ref<Node | null>(null)
/** 管理地址变更后的全面重检(V20):进度弹窗里逐步显示连接、转发、服务、采集。 */
const recheck = useNodeRecheck(() => load())

function openCreate() {
  editing.value = null
  formOpen.value = true
}
function openEdit(n: Node) {
  editing.value = n
  formOpen.value = true
}

async function run(id: number, label: string, fn: () => Promise<unknown>, ok: string) {
  busy.value = { ...busy.value, [id]: label }
  try {
    await fn()
    message.success(ok)
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : `${label}失败`)
  } finally {
    const next = { ...busy.value }
    delete next[id]
    busy.value = next
  }
}

/** 行主操作随状态变,只留一个。 */
type NodeAction = 'deploy' | 'testSSH' | 'resumeSub' | 'probe' | 'detail'

function primaryAction(n: Node): NodeAction {
  if (n.status === 'OFFLINE') return 'testSSH'
  if (n.status === 'DISABLED') return 'detail'
  if (!n.subscription_enabled) return 'resumeSub'
  if (needsDeploy(n)) return 'deploy'
  if (!n.singbox_version && n.role !== 'RELAY') return 'probe'
  return 'detail'
}

const actionLabel: Record<NodeAction, string> = {
  deploy: '部署',
  testSSH: '测试 SSH',
  resumeSub: '恢复下发',
  probe: '探测',
  detail: '详情',
}

function runPrimary(n: Node) {
  switch (primaryAction(n)) {
    case 'deploy':
      return confirmDeploy(n)
    case 'testSSH':
      return run(n.id, '测试 SSH', () => api.testNodeSSH(n.id), 'SSH 连接正常')
    case 'resumeSub':
      return run(
        n.id,
        '恢复下发',
        () => api.updateNode(n.id, { subscription_enabled: true }),
        '已恢复下发,用户下次更新订阅即可看到',
      )
    case 'probe':
      return run(n.id, '探测', () => api.probeNode(n.id), '探测完成,节点档案已更新')
    default:
      openDetail(n.id)
  }
}

/** 影响清单只有 nodeOps.confirmDeployNode 一份,入口页与详情页用的也是它。 */
function confirmDeploy(n: Node) {
  confirmDeployNode(n, () => {
    void run(
      n.id,
      '部署',
      async () => {
        const r = await api.deployNode(n.id)
        if (r.status !== 'SUCCESS') throw new ApiError(0, r.error_message || '部署未成功,详情见部署记录')
        if (r.unchanged) message.info('配置已一致,没有重启服务')
      },
      '部署已完成,详情见部署记录',
    )
  })
}

function confirmToggle(n: Node) {
  const enable = n.status === 'DISABLED'
  lbDangerConfirm({
    title: enable ? `启用节点 ${n.name}` : `禁用节点 ${n.name}?`,
    okText: enable ? '启用' : '禁用',
    okType: enable ? 'primary' : 'danger',
    impacts: enable
      ? ['节点重新进入用户订阅', '需要重新部署才能下发当前的用户凭据']
      : [
          '整个节点停用,不再出现在任何人的订阅里',
          '节点上的 sing-box 不会被停掉,已连上的客户端仍可能继续用',
          '与「停发订阅」不同 —— 后者只是不进新订阅,节点照常参与管理',
          '商家到期提醒照常发:节点停用不代表商家停止收费',
        ],
    onOk: () =>
      run(
        n.id,
        enable ? '启用' : '禁用',
        () => api.setNodeEnabled(n.id, enable),
        enable ? '已启用' : '已禁用,该节点不再出现在用户订阅中',
      ),
  })
}

// ---------- 编号(排序号)就地编辑 ----------

const sortEditID = ref<number | null>(null)
const sortEditValue = ref(0)

function startSortEdit(n: Node) {
  sortEditID.value = n.id
  sortEditValue.value = n.sort_order
}

async function commitSortEdit() {
  const id = sortEditID.value
  if (id === null) return
  sortEditID.value = null
  const value = Math.max(0, Math.round(Number(sortEditValue.value) || 0))
  const n = nodes.value.find((x) => x.id === id)
  if (!n || n.sort_order === value) return
  try {
    await api.setNodeSortOrder(id, value)
    n.sort_order = value
    message.success(`排序号已改为 ${value},订阅与门户里的先后随之变化`)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '修改排序号失败')
  }
}

// ---------- 批量 ----------

const batchOpen = ref(false)
const batchTitle = ref('')
const batchItems = ref<LbResultItem[]>([])
const batchRunning = ref(false)

async function runBatch(title: string, fn: (n: Node) => Promise<unknown>) {
  const targets = nodes.value.filter((n) => selected.value.includes(n.id))
  batchTitle.value = title
  batchItems.value = targets.map((n) => ({ id: n.id, name: n.name }))
  batchOpen.value = true
  batchRunning.value = true
  for (let i = 0; i < targets.length; i++) {
    try {
      await fn(targets[i]!)
      batchItems.value[i] = { ...batchItems.value[i]!, ok: true, detail: '已完成' }
    } catch (err) {
      batchItems.value[i] = {
        ...batchItems.value[i]!,
        ok: false,
        detail: err instanceof ApiError ? err.message : '失败',
      }
    }
    batchItems.value = [...batchItems.value]
  }
  batchRunning.value = false
  await load()
}

function confirmBatchDeploy() {
  lbDangerConfirm({
    title: `部署 ${selected.value.length} 个节点?`,
    okText: '批量部署',
    okType: 'primary',
    impacts: [
      '每台先做前置检查并自动补齐缺失条件;配置已一致且服务在跑的机器不重启',
      '有变更的机器会重启 sing-box,断开其上全部在线连接',
      '逐个执行,失败不影响其余;已成功的不会回滚 —— 批量操作不是事务',
    ],
    onOk: () => runBatch('批量部署', (n) => api.deployNode(n.id)),
  })
}

async function retryOne(item: LbResultItem) {
  const idx = batchItems.value.findIndex((i) => i.id === item.id)
  if (idx < 0) return
  batchItems.value[idx] = { ...batchItems.value[idx]!, ok: undefined, detail: '重试中' }
  batchItems.value = [...batchItems.value]
  try {
    await api.deployNode(Number(item.id))
    batchItems.value[idx] = { ...batchItems.value[idx]!, ok: true, detail: '已完成' }
  } catch (err) {
    batchItems.value[idx] = {
      ...batchItems.value[idx]!,
      ok: false,
      detail: err instanceof ApiError ? err.message : '失败',
    }
  }
  batchItems.value = [...batchItems.value]
  await load()
}

// ---------- 服务巡检 ----------

const health = ref<Record<number, NodeHealth>>({})
const healthEnabled = ref(true)
const healthError = ref('')
const runningHealth = ref(false)

async function loadHealth() {
  healthError.value = ''
  try {
    const r = await api.nodeHealth()
    healthEnabled.value = r.enabled
    const map: Record<number, NodeHealth> = {}
    for (const h of r.items) map[h.node_id] = h
    health.value = map
  } catch (err) {
    health.value = {}
    healthError.value = err instanceof ApiError ? err.message : '巡检结果读取失败'
  }
}

async function runHealthNow() {
  runningHealth.value = true
  try {
    const r = await api.runNodeHealth()
    const map: Record<number, NodeHealth> = {}
    for (const h of r.items) map[h.node_id] = h
    health.value = map
    message.success('已巡检一轮')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '巡检失败')
  } finally {
    runningHealth.value = false
  }
}

function healthOf(n: Node): HealthSummary {
  return summarizeHealth(n, health.value[n.id], { syncError: syncErrors.value[n.id], cloud: n.cloud ?? null })
}

// ---------- 列 ----------

// 列宽之和控制在 1120 以内:1440 宽的屏幕减去侧栏与内距正好剩 1144,
// 再宽一点右侧固定列就会盖住「到期时间」。
const columns = [
  { title: '编号', key: 'sort', width: 64, fixed: 'left' as const },
  { title: '节点', key: 'node', width: 250, fixed: 'left' as const },
  { title: '管理 IP', key: 'host', width: 176 },
  { title: '服务巡检', key: 'health', width: 146 },
  { title: '本周期流量', key: 'cycle', width: 180 },
  { title: '到期时间', key: 'expiry', width: 150 },
  { title: '操作', key: 'actions', width: 112, fixed: 'right' as const },
]

/**
 * 「本周期流量」第二行:重置时间或「不重置」。
 * 只渲染后端给的 next_reset_at,不在前端按 reset_day 自己推。
 */
function cycleResetText(id: number): string {
  const c = cycles.value[id]
  if (!c) return ''
  if (!c.next_reset_at) return '不重置 · 累计至今'
  const left = daysUntil(c.next_reset_at)
  const tail = left === null ? '' : left <= 0 ? ' · 今天' : ` · ${left} 天后`
  return `${formatUTCDay(c.next_reset_at)} 重置${tail}`
}

/** 计费口径的说明收进提示,不再常驻一行。数值口径一个字没变。 */
function cycleTitle(id: number): string {
  const c = cycles.value[id]
  if (!c) return ''
  const base = c.billing_factor > 1
    ? `按商家的双向计费口径折算(×${c.billing_factor}),sing-box 原始计数 ${formatBytes(c.proxy_bytes)}`
    : '按出站计费口径,与 sing-box 计数 1:1'
  return `${base};重置时间按 UTC 00:00`
}

async function copyHost(host: string) {
  try {
    await navigator.clipboard.writeText(host)
    message.success('已复制管理地址')
  } catch {
    message.error('复制失败,请手动选中复制')
  }
}

function expiryCell(n: Node): { date: string; auto: boolean } {
  const e = n.expiry
  if (!e || !e.expires_at) return { date: '', auto: false }
  return { date: formatDate(e.expires_at), auto: e.auto_renew }
}

onErrorCaptured((err) => {
  message.error(`节点列表渲染失败:${err instanceof Error ? err.message : String(err)}`)
})

const pager = usePagination('nodes', () => visible.value.length)

const keyOpen = ref(false)
</script>

<template>
  <div class="lb-page nv">
    <div class="lb-page__head">
      <div class="lb-page__title-wrap">
        <h1 class="lb-page__title">
          <span>自建节点</span>
          <LbInfoTip
            text="按编号(排序号)升序,与订阅、门户同序;编号可以点击就地修改。一台机器只承载一个节点 —— 两个节点指向同一台机器会互相覆盖配置。SSH 与部署一律走 IPv4。协议、端口、订阅名称与转发关系在详情里。"
            :width="340"
          />
        </h1>
        <div class="lb-page__summary">{{ summaryLine }}</div>
      </div>
      <div class="lb-page__actions">
        <a-button @click="keyOpen = true">SSH 公钥</a-button>
        <a-button :loading="loading" @click="load">刷新</a-button>
        <a-button :loading="runningHealth" @click="runHealthNow">立即巡检</a-button>
        <a-button type="primary" @click="openCreate">+ 添加节点</a-button>
      </div>
    </div>

    <section class="lb-metrics">
      <LbMetricCard
        label="在线节点"
        :state="metricState"
        :value="stats.online"
        :total="stats.total"
        empty-hint="尚未添加节点"
        :hint="offlineCount ? `${offlineCount} 台离线` : stats.total ? '全部在线' : undefined"
      />
      <LbMetricCard label="待部署" :state="metricState" :value="stats.pending" :tone="stats.pending ? 'warning' : 'default'">
        <template #foot>
          <a v-if="stats.pending" class="nv__link" @click="filters.config = 'PENDING'">筛选 ›</a>
          <span v-else>配置都已同步</span>
        </template>
      </LbMetricCard>
      <LbMetricCard label="即将到期 / 已到期" :state="metricState" :value="stats.expiring" :tone="stats.expiring ? 'warning' : 'default'">
        <template #foot>
          <a v-if="stats.expiring" class="nv__link" @click="filters.expiry = 'SOON'">筛选 ›</a>
          <span v-else>没有要续费的</span>
        </template>
      </LbMetricCard>
      <LbMetricCard
        label="本周期流量合计"
        tip="按各节点自己的周期边界与计费口径汇总。双向计费的机器已按口径折算;中转主机不计。"
        :state="cycleError ? 'error' : metricState"
        :value="formatBytes(stats.cycleUsed).split(' ')[0]"
        :unit="formatBytes(stats.cycleUsed).split(' ')[1]"
        :hint="billedCount ? `${billedCount} 台计费节点` : '没有设额度的节点'"
      />
    </section>

    <a-alert
      v-if="healthError && !loadError"
      type="warning"
      show-icon
      :message="`「服务巡检」列暂时读不到(${healthError}),其余数据正常`"
    >
      <template #action>
        <a-button size="small" @click="loadHealth">只重试这一列</a-button>
      </template>
    </a-alert>
    <a-alert
      v-else-if="!healthEnabled && !loadError"
      type="info"
      show-icon
      message="服务巡检未启用 —— 没有人在定期检查 sing-box / nginx 还在不在跑"
    />
    <a-alert
      v-if="(cycleError || metricsError) && !loadError"
      type="warning"
      show-icon
      :message="
        cycleError && metricsError
          ? '「本周期流量」与资源采样暂时读不到,其余数据正常'
          : cycleError
            ? '「本周期流量」列暂时读不到,其余数据正常'
            : '资源采样暂时读不到,不影响节点运行状态'
      "
    >
      <template #action>
        <a-button size="small" @click="loadColumns">只重试这些列</a-button>
      </template>
    </a-alert>

    <section>
      <LbSectionTitle title="全部节点" :count="`${visible.length} / ${nodes.length} 台`" />
      <div class="lb-card lb-card--flush">
        <LbFilterBar :active-count="activeFilterCount" @clear="clearFilters">
          <a-input v-model:value="filters.keyword" placeholder="内部名称 / 订阅名称 / 地址" allow-clear>
            <template #prefix><LbIcon name="search" :size="14" /></template>
          </a-input>
          <a-select v-model:value="filters.run" placeholder="运行状态" allow-clear style="width: 120px">
            <a-select-option value="ONLINE">运行中</a-select-option>
            <a-select-option value="OFFLINE">离线</a-select-option>
            <a-select-option value="DEPLOY_FAILED">部署失败</a-select-option>
            <a-select-option value="PENDING">待初始化</a-select-option>
            <a-select-option value="DISABLED">已禁用</a-select-option>
          </a-select>
          <a-select v-model:value="filters.config" placeholder="配置状态" allow-clear style="width: 120px">
            <a-select-option value="IN_SYNC">已同步</a-select-option>
            <a-select-option value="PENDING">待部署</a-select-option>
            <a-select-option value="DEPLOY_FAILED">部署失败</a-select-option>
            <a-select-option value="NEVER_DEPLOYED">未部署</a-select-option>
            <a-select-option value="UNKNOWN">未知</a-select-option>
          </a-select>
          <a-select v-model:value="filters.tierID" placeholder="访问等级" allow-clear style="width: 110px">
            <a-select-option v-for="t in tiers" :key="t.id" :value="t.id">{{ t.name }}</a-select-option>
          </a-select>
          <a-select v-model:value="filters.expiry" style="width: 140px">
            <a-select-option v-for="o in expiryFilterOptions" :key="o.value" :value="o.value">{{ o.label }}</a-select-option>
          </a-select>
          <label class="lb-filter__toggle" :class="{ 'lb-filter__toggle--on': filters.subOff }">
            <a-switch v-model:checked="filters.subOff" size="small" />
            仅停发订阅
          </label>
        </LbFilterBar>

        <LbBatchBar
          :selected-count="selected.length"
          :filtered-total="visible.length"
          :total="nodes.length"
          unit="台"
          @clear="selected = []"
        >
          <a-button size="small" type="primary" @click="confirmBatchDeploy">批量部署</a-button>
          <a-button size="small" @click="runBatch('批量同步流量', (n) => api.syncNodeTraffic(n.id))">
            同步流量
          </a-button>
        </LbBatchBar>

        <LbEmptyState
          v-if="loadError"
          variant="error"
          :title="loadError.message"
          description="不显示「暂无数据」—— 那会被读成一台机器都没有。"
          :http-status="loadError.status"
          :occurred-at="loadError.at"
          @retry="load"
        />
        <LbEmptyState
          v-else-if="!loading && nodes.length === 0"
          variant="empty"
          title="还没有节点"
          description="添加第一台 VPS,然后依次执行探测、安装、部署。用户需要节点才能使用。"
        >
          <template #action>
            <a-button type="primary" size="small" @click="openCreate">添加节点</a-button>
          </template>
        </LbEmptyState>
        <LbEmptyState
          v-else-if="!loading && visible.length === 0"
          variant="filtered"
          title="没有符合条件的节点"
          :description="`当前有 ${activeFilterCount} 项筛选生效,${nodes.length} 台机器被筛掉。`"
          @clear="clearFilters"
        />

        <!-- <768 整表换卡片:横向滚动会把「操作」列推到屏幕外。信息优先级与桌面一致。 -->
        <div v-else-if="narrow" class="nv__cards">
          <LbRowCard v-for="n in pager.slice(visible)" :key="n.id">
            <template #head>
              <span class="nv__meta lb-tabular">#{{ n.sort_order }}</span>
              <a class="nv__card-name" @click="openDetail(n.id)">{{ n.name }}</a>
              <span v-if="n.role === 'RELAY'" class="lb-chip nv__chip">中转</span>
              <LbStatusTag kind="node" :status="n.status" />
            </template>

            <div class="nv__stack nv__stack--row">
              <LbStatusTag :meta="configStatusMeta[configState(n)]" :suffix="configState(n) === 'NOT_APPLICABLE' ? '' : `rev ${n.config_revision}`" />
              <LbStatusTag v-for="(b, i) in nodeBadges(n, metrics[n.id]?.collected_at)" :key="i" :meta="b" />
            </div>
            <div class="nv__host lb-mono">
              {{ n.host }}
              <a class="nv__copy" @click="copyHost(n.host)">复制</a>
            </div>
            <div class="nv__stack nv__stack--row">
              <span class="nv__reset">巡检</span>
              <LbStatusTag :meta="healthOf(n).meta" />
              <LbStatusTag v-for="(b, i) in healthOf(n).badges" :key="`b${i}`" :meta="b" />
            </div>
            <div v-if="n.maintenance_message" class="nv__card-maint">{{ n.maintenance_message }}</div>
            <div v-if="n.role === 'RELAY'" class="nv__reset">中转主机,面板不计流量</div>
            <template v-else>
              <LbQuotaBar
                :used-bytes="cycles[n.id]?.used_bytes ?? null"
                :quota-bytes="cycles[n.id]?.quota_bytes ?? n.traffic_quota_bytes"
                :warning-level="cycles[n.id]?.warning_level"
              />
              <div v-if="cycleResetText(n.id)" class="nv__reset" :title="cycleTitle(n.id)">{{ cycleResetText(n.id) }}</div>
            </template>
            <div class="nv__stack nv__stack--row">
              <span class="nv__reset">到期</span>
              <a class="nv__expiry" @click="expiryTarget = n">
                <LbStatusTag :meta="expiryStatusMeta(n.expiry)" />
                <span v-if="expiryCell(n).date" class="nv__reset">{{ expiryCell(n).date }}</span>
                <span v-if="expiryCell(n).auto" class="lb-chip nv__chip">自动续费</span>
              </a>
            </div>

            <template #foot>
              <a-button
                :type="primaryAction(n) === 'detail' ? 'default' : 'primary'"
                :loading="!!busy[n.id]"
                @click="runPrimary(n)"
              >
                {{ actionLabel[primaryAction(n)] }}
              </a-button>
              <a-dropdown placement="topRight">
                <a-button class="lb-touch-target" :aria-label="`${n.name} 的更多操作`">
                  <LbIcon name="more" :size="16" />
                </a-button>
                <template #overlay>
                  <a-menu>
                    <a-menu-item @click="openDetail(n.id)">详情</a-menu-item>
                    <a-menu-item @click="openEdit(n)">编辑节点</a-menu-item>
                    <a-menu-item @click="expiryTarget = n">续费 / 修改到期时间</a-menu-item>
                    <a-menu-item @click="confirmDeploy(n)">部署</a-menu-item>
                  </a-menu>
                </template>
              </a-dropdown>
            </template>
          </LbRowCard>

          <a-pagination
            v-if="visible.length > pager.pageSize.value"
            v-model:current="pager.current.value"
            :page-size="pager.pageSize.value"
            :total="visible.length"
            :show-size-changer="false"
            simple
            class="nv__pager"
          />
        </div>

        <a-table
          v-else
          :columns="columns"
          :data-source="visible"
          :loading="loading"
          :row-selection="rowSelection"
          :pagination="pager.options.value"
          row-key="id"
          size="small"
          :scroll="{ x: 1080 }"
        >
          <template #bodyCell="{ column, record }">
            <!-- 编号 = 业务排序号(不是数据库主键),点击就地改。 -->
            <template v-if="column.key === 'sort'">
              <a-input-number
                v-if="sortEditID === record.id"
                v-model:value="sortEditValue"
                size="small"
                :min="0"
                :max="1000000"
                :controls="false"
                style="width: 56px"
                autofocus
                @press-enter="commitSortEdit"
                @blur="commitSortEdit"
              />
              <a
                v-else
                class="nv__sort lb-tabular"
                title="排序号,决定订阅与门户里的先后;点击修改"
                @click="startSortEdit(record)"
              >
                #{{ record.sort_order }}
              </a>
            </template>

            <template v-else-if="column.key === 'node'">
              <div class="nv__name-row">
                <a class="nv__name" @click="openDetail(record.id)">{{ record.name }}</a>
                <span v-if="record.role === 'RELAY'" class="lb-chip nv__chip">中转</span>
              </div>
              <!-- 运行状态与配置状态仍是两个独立标签,只是从两列挪到名字旁边。 -->
              <div class="nv__stack nv__stack--row nv__tags">
                <LbStatusTag kind="node" :status="record.status" />
                <LbStatusTag
                  :meta="configStatusMeta[configState(record)]"
                  :suffix="configState(record) === 'NOT_APPLICABLE' ? '' : `rev ${record.config_revision}`"
                />
                <LbStatusTag
                  v-for="(b, i) in nodeBadges(record, metrics[record.id]?.collected_at)"
                  :key="i"
                  :meta="b"
                />
                <span v-if="record.maintenance_message" class="nv__maint" :title="record.maintenance_message">
                  {{ record.maintenance_message }}
                </span>
                <span v-if="busy[record.id]" class="nv__busy">{{ busy[record.id] }}中…</span>
              </div>
            </template>

            <template v-else-if="column.key === 'host'">
              <span class="nv__hostcell">
                <span class="lb-mono nv__hosttext" :title="record.host">{{ record.host }}</span>
                <a-button
                  size="small"
                  class="lb-btn-circle lb-btn-ghost lb-btn-ghost--text nv__copybtn"
                  :aria-label="`复制 ${record.host}`"
                  title="复制管理地址"
                  @click="copyHost(record.host)"
                >
                  <LbIcon name="copy" :size="13" />
                </a-button>
              </span>
            </template>

            <template v-else-if="column.key === 'health'">
              <a-popover placement="bottomLeft" :mouse-enter-delay="0.2">
                <template #content>
                  <div class="nv__hpop">
                    <div v-if="!healthOf(record).items.length && !healthOf(record).badges.length" class="nv__reset">
                      这台机器上没有面板托管的服务要查,或者巡检还没跑到它。
                    </div>
                    <div v-for="it in healthOf(record).items" :key="it.name" class="nv__hitem" :title="it.detail">
                      <span class="nv__hname">{{ it.name }}</span>
                      <LbStatusTag kind="service" :status="it.state" />
                    </div>
                    <div v-for="(b, i) in healthOf(record).badges" :key="`b${i}`" class="nv__hitem">
                      <LbStatusTag :meta="b" />
                    </div>
                    <div v-if="healthOf(record).checkedAt" class="nv__reset">
                      巡检于 {{ formatUTCTime(healthOf(record).checkedAt) }}
                      <template v-if="healthOf(record).preChange">—— 那时管理地址还没改,结果说的是旧机器</template>
                    </div>
                  </div>
                </template>
                <div class="nv__stack">
                  <LbStatusTag :meta="healthOf(record).meta" />
                  <LbStatusTag v-for="(b, i) in healthOf(record).badges.slice(0, 2)" :key="i" :meta="b" />
                </div>
              </a-popover>
            </template>

            <template v-else-if="column.key === 'cycle'">
              <div v-if="record.role === 'RELAY'" class="nv__reset">中转主机,面板不计流量</div>
              <template v-else>
                <div :title="cycleTitle(record.id)">
                  <LbQuotaBar
                    :used-bytes="cycles[record.id]?.used_bytes ?? null"
                    :quota-bytes="cycles[record.id]?.quota_bytes ?? record.traffic_quota_bytes"
                    :warning-level="cycles[record.id]?.warning_level"
                  />
                </div>
                <div v-if="cycleResetText(record.id)" class="nv__reset">{{ cycleResetText(record.id) }}</div>
              </template>
            </template>

            <!-- 供应商到期,与流量重置时间分开两列,互不混用。 -->
            <template v-else-if="column.key === 'expiry'">
              <a class="nv__expiry" title="续费 / 修改到期时间" @click="expiryTarget = record">
                <LbStatusTag :meta="expiryStatusMeta(record.expiry)" />
                <span v-if="expiryCell(record).date" class="nv__reset lb-tabular">{{ expiryCell(record).date }}</span>
                <span v-if="expiryCell(record).auto" class="lb-chip nv__chip" title="已登记商家自动续费:面板不代为扣款,到期仍会提醒">自动续费</span>
              </a>
            </template>

            <template v-else-if="column.key === 'actions'">
              <div class="nv__actions">
                <a-button
                  size="small"
                  :type="primaryAction(record) === 'detail' ? 'default' : 'primary'"
                  :class="{ 'lb-btn-ghost': primaryAction(record) === 'detail' }"
                  :loading="!!busy[record.id]"
                  @click="runPrimary(record)"
                >
                  {{ actionLabel[primaryAction(record)] }}
                </a-button>
                <a-dropdown placement="bottomRight">
                  <a-button
                    size="small"
                    class="lb-btn-circle lb-btn-ghost lb-btn-ghost--text"
                    :aria-label="`${record.name} 的更多操作`"
                    :title="`${record.name} 的更多操作`"
                  >
                    <LbIcon name="more" :size="16" />
                  </a-button>
                  <template #overlay>
                    <a-menu>
                      <a-menu-item v-if="primaryAction(record) !== 'detail'" @click="openDetail(record.id)">详情</a-menu-item>
                      <a-menu-item @click="openEdit(record)">编辑节点</a-menu-item>
                      <a-menu-item @click="expiryTarget = record">续费 / 修改到期时间</a-menu-item>
                      <a-menu-item @click="recheck.start(record)">全面重检</a-menu-item>
                      <a-menu-item @click="run(record.id, '探测', () => api.probeNode(record.id), '探测完成')">探测</a-menu-item>
                      <a-menu-item @click="run(record.id, '同步流量', () => api.syncNodeTraffic(record.id), '流量已同步')">
                        同步流量
                      </a-menu-item>
                      <a-menu-item @click="confirmDeploy(record)">部署</a-menu-item>
                      <a-menu-divider />
                      <a-menu-item :danger="record.status !== 'DISABLED'" @click="confirmToggle(record)">
                        {{ record.status === 'DISABLED' ? '启用节点' : '禁用节点' }}
                      </a-menu-item>
                      <a-menu-item @click="openDetail(record.id)">更多操作(详情页)</a-menu-item>
                    </a-menu>
                  </template>
                </a-dropdown>
              </div>
            </template>
          </template>
        </a-table>
      </div>
    </section>

    <NodeFormModal
      v-model:open="formOpen"
      :node="editing"
      :tiers="tiers"
      :next-reset-at="editing ? (cycles[editing.id]?.next_reset_at ?? null) : null"
      @saved="
        (id) => {
          load()
          if (!editing) openDetail(id)
        }
      "
      @deploy="(id) => run(id, '部署', () => api.deployNode(id), '部署已执行,详情见部署记录')"
      @recheck="(id, saved) => recheck.afterSave(nodes.find((n) => n.id === id) ?? null, saved)"
    />
    <NodeOpProgressModal
      :open="recheck.open.value"
      :title="recheck.title.value"
      :running="recheck.running.value"
      :deploy="recheck.result.value"
      :error="recheck.error.value"
      :note="recheck.note.value"
      @update:open="recheck.close"
    />

    <ExpiryModal
      v-if="expiryTarget"
      :open="true"
      kind="NODE"
      :object-id="expiryTarget.id"
      :name="expiryTarget.name"
      @update:open="(v) => { if (!v) expiryTarget = null }"
      @changed="load"
    />

    <a-modal v-model:open="keyOpen" title="面板 SSH 公钥" :width="620" :footer="null">
      <p class="nv__key-note">
        新增节点时装进节点 <code>authorized_keys</code> 的就是这一行。面板对节点的所有操作都用它,
        轮换或吊销时不必动你自己的日常密钥。这一行是公钥,贴到哪里都不构成泄露。
      </p>
      <LbCopyField :value="panelKey || '(尚未生成 —— 首次连接节点时自动生成)'" />
    </a-modal>

    <a-modal
      v-model:open="batchOpen"
      :title="batchTitle"
      :width="560"
      :closable="!batchRunning"
      :mask-closable="false"
      :keyboard="!batchRunning"
      :footer="null"
    >
      <LbResultList :items="batchItems" retryable @retry="retryOne" />
      <div v-if="!batchRunning" class="nv__batch-foot">
        <a-button type="primary" @click="batchOpen = false">完成</a-button>
      </div>
    </a-modal>
  </div>
</template>

<style scoped>
.nv__link {
  font-weight: 500;
}

.nv__name-row {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  min-width: 0;
}

.nv__name {
  font-size: 14px;
  font-weight: 600;
  color: var(--text);
}
.nv__name:hover {
  color: var(--brand);
}

.nv__tags {
  margin-top: 4px;
}

.nv__sort {
  font-size: 12.5px;
  font-weight: 600;
  color: var(--text2);
  text-decoration: underline dotted;
  text-underline-offset: 3px;
}
.nv__sort:hover {
  color: var(--brand);
}

.nv__meta {
  font-size: 11.5px;
  color: var(--text3);
}

.nv__chip {
  font-size: 11px;
  padding: 1px 7px;
}

.nv__host {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  margin-top: 3px;
  font-size: 12px;
  color: var(--text2);
}
.nv__copy {
  font-size: 11.5px;
}

.nv__hostcell {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  min-width: 0;
}
.nv__hosttext {
  font-size: 12.5px;
  color: var(--text);
}
.nv__copybtn {
  opacity: 0.6;
}
.nv__copybtn:hover {
  opacity: 1;
}

.nv__hpop {
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-width: 200px;
}
.nv__hitem {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
}
.nv__hname {
  font-size: 12.5px;
  color: var(--text2);
}

.nv__stack {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 4px;
}
.nv__stack--row {
  flex-direction: row;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
}

.nv__expiry {
  display: inline-flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 3px;
  color: inherit;
}

.nv__cards {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 12px;
}

.nv__card-name {
  flex: 1;
  min-width: 0;
  font-size: 14px;
  font-weight: 600;
  color: var(--text);
}

.nv__card-maint {
  font-size: 11.5px;
  color: var(--purple);
}

.nv__pager {
  align-self: center;
  padding: 4px 0 2px;
}

.nv__maint {
  max-width: 160px;
  font-size: 11.5px;
  color: var(--purple);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.nv__busy {
  font-size: 11.5px;
  color: var(--brand);
}

.nv__reset {
  margin-top: 2px;
  font-size: 11.5px;
  color: var(--text3);
}

.nv__actions {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 6px;
}

.nv__key-note {
  margin: 0 0 12px;
  font-size: 13px;
  line-height: 1.7;
  color: var(--text2);
}

.nv__key-note code {
  padding: 1px 5px;
  background: var(--fill);
  border-radius: 5px;
  font-family: var(--mono);
  font-size: 12px;
}

.nv__batch-foot {
  display: flex;
  justify-content: flex-end;
  margin-top: 14px;
}
</style>
