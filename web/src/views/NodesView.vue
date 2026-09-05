<script setup lang="ts">
import { computed, onErrorCaptured, onMounted, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  PROTOCOL_LABEL,
  PROTOCOL_SHORT,
  type AccessTier,
  type Node,
  type NodeHealth,
  type NodeConfigState,
  type NodeCycleUsage,
  type NodeMetrics,
} from '@/api/client'
import NodeFormModal from '@/components/node/NodeFormModal.vue'
import {
  LbBatchBar,
  LbCopyField,
  LbEmptyState,
  LbFilterBar,
  LbMetricCard,
  LbQuotaBar,
  LbResultList,
  LbRowCard,
  LbStatusTag,
  LbTimeText,
  configStatusMeta,
  lbDangerConfirm,
  type LbResultItem,
} from '@/components/lb'
import { useNarrow } from '@/composables/useNarrow'
import { cloudStatusMeta } from '@/components/cloud/cloudMeta'
import { usePagination } from '@/composables/usePagination'
import { configState, needsDeploy, nodeBadges } from '@/components/lb/derive'
import { daysUntil, formatBytes, formatUTCDay } from '@/utils/format'
import { threshold } from '@/theme/tokens'

/**
 * 节点列表。与用户列表最大的差别是**运行状态与配置状态分两列**:
 * 节点只有一个 status 枚举,DEPLOY_FAILED 与 ONLINE 互斥地挤在同一格 ——
 * 一台在跑旧配置、部署失败的机器只显示「部署失败」,看不出它其实还在服务用户。
 *
 * 配置状态取后端字段,不在列表里逐行调 config-diff:那个接口要连 SSH 读
 * 节点上的实际配置,10 台机器就是 10 条 SSH 会话。
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

/**
 * 详情改成整页,列表这边只负责跳过去。
 *
 * 抽屉时期这里存着「当前打开的是哪个节点」,而那份状态要和抽屉里的加载、
 * 编辑表单、错误兜底一起维护 —— 整页之后它归详情页自己,列表页不再知道
 * 谁被打开了,也就不会有两处不同步的可能。
 */
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
  subOff: false,
}
const filters = reactive({ ...blankFilters })

const activeFilterCount = computed(
  () =>
    (filters.keyword.trim() ? 1 : 0) +
    (filters.run !== undefined ? 1 : 0) +
    (filters.config !== undefined ? 1 : 0) +
    (filters.tierID !== undefined ? 1 : 0) +
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
        // 订阅地址也要能搜到:用户报障时给的是它,而管理员手上只有这个搜索框。
        const hay = [n.name, n.display_name, n.host, n.sub_ipv4_address, n.ipv6_address]
          .join(' ')
          .toLowerCase()
        if (!hay.includes(kw)) return false
      }
      if (filters.run !== undefined && n.status !== filters.run) return false
      if (filters.config !== undefined && configState(n) !== filters.config) return false
      // 按等级筛的是【入口】:一台机器上可以既有普通组入口又有 VIP 入口,
      // 只要有一个命中就把这台机器留下 —— 否则筛出来的是一份空列表,
      // 而管理员会以为那一档一个节点都没有。
      if (
        filters.tierID !== undefined &&
        !(n.inbounds ?? []).some((i) => i.access_tier_id === filters.tierID)
      ) {
        return false
      }
      if (filters.subOff && n.subscription_enabled) return false
      return true
    })
    // 与订阅、门户同一个顺序:排序值升序,相同则按 id。后端 List 已经这样排了,
    // 这里再排一遍是因为管理员改完排序值就该立刻在这一页看到位置变化 ——
    // 而列表在 load() 之前还是旧顺序。id 兜底不能省:全部留 0 时没有兜底就是不稳定排序。
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
  cycleUsed: Object.values(cycles.value).reduce((s, c) => s + c.used_bytes, 0),
}))

const metricState = computed(() =>
  loadError.value ? 'error' : loading.value ? 'loading' : nodes.value.length ? 'ready' : 'empty',
)

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

/**
 * 行主操作随状态变,只留一个。管理员打开这一页永远是为了处理某台机器的某件事,
 * 让他先读完六个等重的文字链再自己判断该点哪个,是把分诊工作推给了他。
 */
type NodeAction = 'deploy' | 'testSSH' | 'resumeSub' | 'probe' | 'detail'

function primaryAction(n: Node): NodeAction {
  if (n.status === 'OFFLINE') return 'testSSH'
  if (n.status === 'DISABLED') return 'detail'
  if (!n.subscription_enabled) return 'resumeSub'
  if (needsDeploy(n)) return 'deploy'
  if (!n.singbox_version) return 'probe'
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

/** 部署是可逆的(有自动回滚),所以是危险确认档而不是输入名称档。 */
function confirmDeploy(n: Node) {
  lbDangerConfirm({
    title: `部署到 ${n.display_name || n.name}?`,
    okText: '部署',
    okType: 'primary',
    impacts: [
      '会重启 sing-box,断开这台机器上全部在线连接',
      '部署前会强制同步一次流量,未落库的计数不会丢',
      '健康检查不通过时自动回滚到上一版本',
    ],
    footer: '部署一旦发出就在节点上跑,关掉页面不会取消它。',
    onOk: () => run(n.id, '部署', () => api.deployNode(n.id), '部署已执行,详情见部署记录'),
  })
}

function confirmToggle(n: Node) {
  const enable = n.status === 'DISABLED'
  lbDangerConfirm({
    title: enable ? `启用节点 ${n.display_name || n.name}` : `禁用节点 ${n.display_name || n.name}?`,
    okText: enable ? '启用' : '禁用',
    okType: enable ? 'primary' : 'danger',
    impacts: enable
      ? ['节点重新进入用户订阅', '需要重新部署才能下发当前的用户凭据']
      : [
          '整个节点停用,不再出现在任何人的订阅里',
          '节点上的 sing-box 不会被停掉,已连上的客户端仍可能继续用',
          '与「停发订阅」不同 —— 后者只是不进新订阅,节点照常参与管理',
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

// ---------- 批量 ----------

const batchOpen = ref(false)
const batchTitle = ref('')
const batchItems = ref<LbResultItem[]>([])
const batchRunning = ref(false)

async function runBatch(title: string, fn: (n: Node) => Promise<unknown>) {
  const targets = nodes.value.filter((n) => selected.value.includes(n.id))
  batchTitle.value = title
  batchItems.value = targets.map((n) => ({ id: n.id, name: n.display_name || n.name }))
  batchOpen.value = true
  batchRunning.value = true

  // 逐个执行,中途失败不中止剩余的。已成功的不会回滚 —— 批量不是事务。
  for (let i = 0; i < targets.length; i++) {
    try {
      await fn(targets[i])
      batchItems.value[i] = { ...batchItems.value[i], ok: true, detail: '已完成' }
    } catch (err) {
      batchItems.value[i] = {
        ...batchItems.value[i],
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
      '每台机器的 sing-box 都会重启,断开其上全部在线连接',
      '逐个执行,失败不影响其余',
      '已成功的不会回滚 —— 批量操作不是事务',
    ],
    onOk: () => runBatch('批量部署', (n) => api.deployNode(n.id)),
  })
}

async function retryOne(item: LbResultItem) {
  const idx = batchItems.value.findIndex((i) => i.id === item.id)
  if (idx < 0) return
  batchItems.value[idx] = { ...batchItems.value[idx], ok: undefined, detail: '重试中' }
  batchItems.value = [...batchItems.value]
  try {
    await api.deployNode(Number(item.id))
    batchItems.value[idx] = { ...batchItems.value[idx], ok: true, detail: '已完成' }
  } catch (err) {
    batchItems.value[idx] = {
      ...batchItems.value[idx],
      ok: false,
      detail: err instanceof ApiError ? err.message : '失败',
    }
  }
  batchItems.value = [...batchItems.value]
  await load()
}

// ---------- 列 ----------

// 节点列左固定、操作列右固定:768–1279 中间那一档要横向滚动,
// 不固定的话滚动之后既看不出这是哪台机器,也够不着操作按钮。

/**
 * 服务巡检结果,按节点 ID 索引。
 *
 * 与「运行状态」分成两列:那一列答的是"上次探测能不能连上、上次部署成不成功",
 * 这一列答的是"此刻 sing-box / nginx 还在不在跑"。一台 ONLINE 的机器完全可能
 * 跑着一个已经死掉的 sing-box —— 挤在一格里就再也看不出这件事了。
 */
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
    // **列级失败不升级成整表失败。** 只让这一列显示「—」,
    // 并在表格上方说出来 —— 否则一整列的「—」看起来像所有机器都挂了。
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

const columns = [
  { title: '节点', key: 'node', width: 250, fixed: 'left' as const },
  { title: '运行状态', key: 'run', width: 160 },
  { title: '配置状态', key: 'config', width: 160 },
  { title: '服务巡检', key: 'health', width: 150 },
  { title: '云实例', key: 'cloud', width: 110 },
  { title: '最后同步', key: 'sync', width: 110 },
  { title: '本周期流量', key: 'cycle', width: 215 },
  { title: '操作', key: 'actions', width: 190, fixed: 'right' as const },
]

/**
 * 「本周期流量」后面跟的重置日。
 *
 * 只渲染后端算好的 next_reset_at,不在前端按 reset_day 自己推一遍 ——
 * 周期边界只有 traffic.CalculateNodePeriod 一处实现(重置日 31 在二月要落到
 * 当月最后一天而不是顺延)。各算各的会让列表说「9 月 1 日」、详情说
 * 「8 月 31 日」,两边都不报错,管理员只能靠猜。
 */
function cycleResetText(id: number): string {
  const c = cycles.value[id]
  if (!c) return ''
  // 不重置的节点这一列是「创建以来的累计」,不是某个周期内的量 —— 表头写的是
  // 「本周期流量」,不说明的话看起来像本月用了这么多。
  if (!c.next_reset_at) return '不重置 · 累计至今'
  const left = daysUntil(c.next_reset_at)
  const tail = left === null ? '' : left <= 0 ? ' · 今天' : ` · ${left} 天后`
  return `${formatUTCDay(c.next_reset_at)} 00:00 UTC 重置${tail}`
}

/**
 * 双向计费的节点要标出来。这一列的数字已经被后端 ×2 折算过,
 * 不标的话它和相邻那台出站计费的机器看起来是同一个口径 —— 而两者差一倍。
 */
function billingNote(id: number): string {
  const c = cycles.value[id]
  if (!c || c.billing_factor <= 1) return ''
  return `双向计费 ×${c.billing_factor} · 代理转发 ${formatBytes(c.proxy_bytes)}`
}

/**
 * 列表里的协议标记。多入站之后一台机器上可以有好几种,这里做去重汇总。
 *
 * 取【已部署】的协议:列表回答的是「这台机器现在是什么样」,而
 * deployed_protocol 才是节点上真正在跑的那个。用期望值的话,改完协议
 * 还没部署的那段时间里,列表说 SS2022、用户订阅里却仍是 vless://。
 *
 * 从未部署过的入口回落到期望值并加问号 —— 那个入口在节点上还不存在。
 */
function protocolShort(n: Node): string {
  const seen = new Set<string>()
  for (const i of n.inbounds ?? []) {
    seen.add(i.deployed_protocol ? PROTOCOL_SHORT[i.deployed_protocol] : PROTOCOL_SHORT[i.protocol] + '?')
  }
  // Mieru 入口也要数进来:漏掉的话,一台只有 Mieru 入口的机器在列表里
  // 显示「无入口」,而它其实好好地在服务用户。
  if ((n.mieru_inbounds ?? []).length) seen.add('Mieru')
  return [...seen].join(' / ') || '无入口'
}

function protocolTitle(n: Node): string {
  const list = n.inbounds ?? []
  if (!list.length) {
    return '这台机器上一个入口都没有 —— sing-box 会正常运行,但谁都连不上'
  }
  return list
    .map((i) => {
      if (!i.deployed_protocol) {
        return `${i.display_name}:尚未部署过,部署后将使用 ${PROTOCOL_LABEL[i.protocol]}`
      }
      const running = PROTOCOL_LABEL[i.deployed_protocol]
      if (i.deployed_protocol === i.protocol) {
        return `${i.display_name}:正在运行 ${running},订阅里下发的也是它`
      }
      // 期望与生效不一致 —— 这正是「改了协议还没部署」。必须说全,
      // 只显示其中一个会让管理员以为切换已经完成。
      return `${i.display_name}:正在运行 ${running};已改为 ${PROTOCOL_LABEL[i.protocol]},部署后生效`
    })
    .join('；')
}

/**
 * 列表里的端口摘要。一台机器上的入口可能有好几个,逐个列全。
 *
 * 写的是【公网端口】——那是用户实际要连的号码。主机监听端口与它不同时
 * 一并写出来:NAT 机器上两者的差别正是排查「连不上」时第一个要看的东西。
 */
function portSummary(n: Node): string {
  const parts = (n.inbounds ?? []).map((i) => {
    const pub = i.public_port || i.listen_port
    return pub === i.listen_port ? `${pub}` : `${pub}→${i.listen_port}`
  })
  // Mieru 的端口是一段而不是一个数 —— 写成起止,单端口时只写一个号码。
  for (const m of n.mieru_inbounds ?? []) {
    parts.push(
      m.listen_port_start === m.listen_port_end
        ? `${m.listen_port_start}`
        : `${m.listen_port_start}-${m.listen_port_end}`,
    )
  }
  return parts.length ? parts.join(' ') : '无入口'
}

/**
 * 这台机器上入口的访问等级,去重。
 *
 * 等级已经降到入口(迁移 0020),一台机器上完全可以既有对所有人开放的入口、
 * 又有只给 VIP 的入口 —— 只显示其中一个会让人以为整台机器都是那一档。
 */
function tierSummary(n: Node): string {
  if (n.role === 'RELAY') return '转发规则各自设定'
  const names = [
    ...new Set([
      ...(n.inbounds ?? []).map((i) => i.access_tier_name),
      ...(n.mieru_inbounds ?? []).map((m) => m.access_tier_name),
    ]),
  ]
  return names.length ? names.join(' / ') : '无入口'
}

/** 这台机器上有没有入口的出口指向别处(链式中转)。 */
function hasChain(n: Node): boolean {
  return (n.inbounds ?? []).some((i) => i.chain_target_kind !== '')
}

/**
 * 详情抽屉渲染出错时的兜底。
 *
 * Vue 在渲染期抛错会卸载出错组件的子树,而 a-drawer 的遮罩是另一个 DOM 节点,
 * 会原样留在屏幕上 —— 管理员看到的是「详情页没了、屏幕一片灰」,不知道发生了
 * 什么,也只能靠点遮罩脱身。已经踩过一次:探测成功时后端把 problems 发成 null,
 * 模板里 `problems.length` 当场抛 TypeError。
 *
 * 这里把抽屉关掉并说出原因。不 return false —— 错误仍要冒泡到控制台,
 * 否则下次排查时什么线索都没有。
 */
onErrorCaptured((err) => {
  // 详情已经不在这个页面里了,这里只兜住列表自身的渲染错误 ——
  // 比如某个字段被后端发成了 null,而模板拿它当数组用。
  // 不 return false:错误仍要冒泡到控制台,否则下次排查时什么线索都没有。
  message.error(`节点列表渲染失败:${err instanceof Error ? err.message : String(err)}`)
})

const pager = usePagination('nodes', () => visible.value.length)

const keyOpen = ref(false)
</script>

<template>
  <div class="nv">
    <div class="nv__head">
      <div>
        <h2 class="nv__title">节点管理</h2>
        <div class="nv__sub">
          {{ nodes.length }} 台机器 · 按排序值升序(与订阅、门户同序) · 一台机器只承载一个节点 ·
          SSH 与部署一律走 IPv4
        </div>
      </div>
      <a-space>
        <a-button @click="keyOpen = true">复制面板 SSH 公钥</a-button>
        <a-button :loading="loading" @click="load">刷新</a-button>
        <!-- 立刻巡检一轮。它可能顺带触发自动恢复,所以要几秒到十几秒;
             不放在「刷新」里 —— 刷新是纯读,而这一下会去连每一台机器。 -->
        <a-button :loading="runningHealth" @click="runHealthNow">立即巡检</a-button>
        <a-button type="primary" @click="openCreate">添加节点</a-button>
      </a-space>
    </div>

    <div class="nv__metrics">
      <LbMetricCard
        label="在线节点"
        :state="metricState"
        :value="stats.online"
        :total="stats.total"
        empty-hint="尚未添加节点"
      />
      <LbMetricCard label="待部署" :state="metricState" :value="stats.pending" tone="warning">
        <template #action>
          <a v-if="stats.pending" @click="filters.config = 'PENDING'">筛选</a>
        </template>
      </LbMetricCard>
      <LbMetricCard label="停发订阅" :state="metricState" :value="stats.subOff">
        <template #action>
          <a v-if="stats.subOff" @click="filters.subOff = true">筛选</a>
        </template>
      </LbMetricCard>
      <LbMetricCard
        label="本周期流量合计"
        :state="cycleError ? 'error' : metricState"
        :value="formatBytes(stats.cycleUsed)"
        hint="按各节点自己的周期边界与计费口径汇总"
      />
    </div>

    <!-- 列级降级要显式说出来:一整列的「—」看起来像所有机器都挂了。 -->
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
    <!-- 巡检整个没启用是另一回事:那不是"读不到",是"根本没在查"。
         显示成一样的话,管理员会去重试一个永远不会有结果的接口。 -->
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

    <a-card :body-style="{ padding: 0 }">
      <LbFilterBar
        :active-count="activeFilterCount"
        :filtered="visible.length"
        :total="nodes.length"
        unit="台"
        @clear="clearFilters"
      >
        <a-input-search
          v-model:value="filters.keyword"
          placeholder="名称 / 展示名称 / IP"
          allow-clear
          style="width: 220px"
        />
        <a-select v-model:value="filters.run" placeholder="运行状态" allow-clear style="width: 130px">
          <a-select-option value="ONLINE">运行中</a-select-option>
          <a-select-option value="OFFLINE">离线</a-select-option>
          <a-select-option value="DEPLOY_FAILED">部署失败</a-select-option>
          <a-select-option value="PENDING">待初始化</a-select-option>
          <a-select-option value="DISABLED">已禁用</a-select-option>
        </a-select>
        <a-select v-model:value="filters.config" placeholder="配置状态" allow-clear style="width: 130px">
          <a-select-option value="IN_SYNC">已同步</a-select-option>
          <a-select-option value="PENDING">待部署</a-select-option>
          <a-select-option value="DEPLOY_FAILED">部署失败</a-select-option>
          <a-select-option value="NEVER_DEPLOYED">未部署</a-select-option>
          <a-select-option value="UNKNOWN">未知</a-select-option>
        </a-select>
        <a-select v-model:value="filters.tierID" placeholder="访问等级" allow-clear style="width: 120px">
          <a-select-option v-for="t in tiers" :key="t.id" :value="t.id">{{ t.name }}</a-select-option>
        </a-select>
        <a-checkbox v-model:checked="filters.subOff">仅停发订阅</a-checkbox>
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

      <!-- <768 整表换卡片:横向滚动会把「操作」列推到屏幕外。 -->
      <div v-else-if="narrow" class="nv__cards">
        <LbRowCard v-for="n in pager.slice(visible)" :key="n.id">
          <template #head>
            <span class="nv__sort lb-mono">#{{ n.sort_order }}</span>
            <a class="nv__card-name" @click="openDetail(n.id)">{{ n.display_name || n.name }}</a>
            <span v-if="n.role === 'RELAY'" class="nv__role">中转</span>
            <LbStatusTag kind="node" :status="n.status" />
          </template>

          <div class="nv__host lb-mono">
            <template v-if="n.role !== 'RELAY'">
              <span class="nv__proto" :title="protocolTitle(n)">{{ protocolShort(n) }}</span>
              {{ n.host }} · 端口 {{ portSummary(n) }} · {{ tierSummary(n) }}
              <!-- 只在两者不同时才写出来:相同的话再列一遍只是噪音。
                   而不同时必须写 —— 上面那个地址是面板连的,不是用户连的,
                   两者长得一样合理,排查时照着它去测会得到与故障无关的结论。 -->
              <template v-if="n.sub_ipv4_address"> · 订阅 {{ n.sub_ipv4_address }}</template>
              <template v-if="n.ipv6_address"> · IPv6</template>
            </template>
            <!-- 中转机没有自己的协议与代理端口,渲染出来只会让人以为配漏了。 -->
            <template v-else>
              {{ n.host }} · 端口见转发规则
              <template v-if="n.sub_ipv4_address"> · 订阅 {{ n.sub_ipv4_address }}</template>
            </template>
          </div>
          <div v-if="hasChain(n)" class="nv__host">出口经中转</div>
          <div class="nv__stack nv__stack--row">
            <LbStatusTag
              :meta="configStatusMeta[configState(n)]"
              :suffix="`rev ${n.config_revision}`"
            />
            <LbStatusTag
              v-for="(b, i) in nodeBadges(n, metrics[n.id]?.collected_at)"
              :key="i"
              :meta="b"
            />
          </div>
          <div v-if="n.maintenance_message" class="nv__card-maint">{{ n.maintenance_message }}</div>
          <div v-if="n.cloud" class="nv__stack nv__stack--row">
            <span class="nv__reset">云实例</span>
            <LbStatusTag :meta="cloudStatusMeta(n.cloud)" />
          </div>
          <!-- 中转机上跑的是 nginx,它不接统计接口,面板在那台机器上
               拿不到任何计数。这里写明「不计流量」而不是画一条 0 的进度条 ——
               0 与「真的没用过」长得一模一样,那是最容易骗到管理员的一种失败。 -->
          <div v-if="n.role === 'RELAY'" class="nv__reset">中转主机,面板不计流量</div>
          <LbQuotaBar
            v-else
            :used-bytes="cycles[n.id]?.used_bytes ?? null"
            :quota-bytes="cycles[n.id]?.quota_bytes ?? n.traffic_quota_bytes"
            :warning-level="cycles[n.id]?.warning_level"
          />
          <div v-if="billingNote(n.id)" class="nv__reset">{{ billingNote(n.id) }}</div>
          <div v-if="cycleResetText(n.id)" class="nv__reset">{{ cycleResetText(n.id) }}</div>
          <div class="nv__host">
            最后同步
            <LbTimeText
              :value="n.last_heartbeat_at"
              :warn-after-ms="threshold.metricsStaleMs"
              :danger-after-ms="threshold.metricsStaleMs * 6"
            />
          </div>

          <template #foot>
            <a-button
              :type="primaryAction(n) === 'detail' ? 'default' : 'primary'"
              :loading="!!busy[n.id]"
              @click="runPrimary(n)"
            >
              {{ actionLabel[primaryAction(n)] }}
            </a-button>
            <a-dropdown v-if="primaryAction(n) !== 'detail'" placement="topRight">
              <a-button
                class="lb-touch-target"
                :aria-label="`${n.display_name || n.name} 的更多操作`"
              >
                ⋯
              </a-button>
              <template #overlay>
                <a-menu>
                  <a-menu-item @click="openDetail(n.id)">详情</a-menu-item>
                  <a-menu-item @click="openEdit(n)">编辑节点</a-menu-item>
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
        :scroll="{ x: 1125 }"
      >
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'node'">
            <!-- 排序值直接写出来。它决定订阅与门户里的先后,不显示的话
                 管理员改了值也看不出改到了第几位。 -->
            <span
              class="nv__sort lb-mono"
              :title="`排序值 ${record.sort_order} —— 数值小的排在订阅与门户前面`"
            >
              #{{ record.sort_order }}
            </span>
            <a @click="openDetail(record.id)">{{ record.display_name || record.name }}</a>
            <!-- 内部名称与展示名称都列:管理员按内部名称找机器,用户报的是展示名称。 -->
            <span v-if="record.display_name !== record.name" class="nv__inner">{{ record.name }}</span>
            <!-- 中转主机与落地节点在同一份列表里混着,而它们几乎没有共同点:
                 中转上没有 sing-box、没有协议与端口、也没有任何流量数字。
                 不标出来的话,管理员会对着一台中转机找它的协议为什么是空的。 -->
            <span v-if="record.role === 'RELAY'" class="nv__role">中转</span>
            <span
              v-if="hasChain(record)"
              class="nv__chain"
              title="这台机器的出口指向别处(链式中转)。订阅内容不受影响。"
              >经中转出网</span
            >
            <div class="nv__host lb-mono">
              <!-- 协议放在地址前面。同一份列表里两种协议混着,不标的话
                   管理员分不出哪台机器的订阅条目是 ss:// —— 而排查
                   「某个客户端连不上」时那正是第一个要知道的事。 -->
              <template v-if="record.role !== 'RELAY'">
                <span class="nv__proto" :title="protocolTitle(record)">{{ protocolShort(record) }}</span>
                {{ record.host }} · 端口 {{ portSummary(record) }}
              </template>
              <!-- 中转机没有自己的协议与代理端口:那些列在库里是 0 /
                   保持默认值,渲染出来只会让人以为配漏了。
                   客户端连的端口在「转发」面板里,一条规则一个。 -->
              <template v-else>{{ record.host }} · 端口见转发规则</template>
              <!-- 订阅地址与管理地址不同时才写出来,理由同上。 -->
              <template v-if="record.sub_ipv4_address"> · 订阅 {{ record.sub_ipv4_address }}</template>
              <!-- 端口与 IPv4 不同时才写出来:相同的话再列一遍只是噪音。 -->
              <template v-if="record.ipv6_address"> · IPv6</template>
            </div>
          </template>

          <!-- 云实例(V17):只有绑了阿里云实例的机器有;其余显示短横,不留空白格。 -->
          <template v-else-if="column.key === 'cloud'">
            <LbStatusTag v-if="record.cloud" :meta="cloudStatusMeta(record.cloud)" small />
            <span v-else class="nv__reset">—</span>
          </template>

          <template v-else-if="column.key === 'health'">
            <div v-if="health[record.id]" class="nv__stack">
              <span v-if="record.role !== 'RELAY'" class="nv__hitem">
                <span class="nv__hname">sing-box</span>
                <LbStatusTag kind="service" :status="health[record.id].singbox" small />
              </span>
              <span
                v-if="health[record.id].nginx !== 'NOT_APPLICABLE'"
                class="nv__hitem"
              >
                <span class="nv__hname">nginx</span>
                <LbStatusTag kind="service" :status="health[record.id].nginx" small />
              </span>
              <span
                v-if="health[record.id].realm && health[record.id].realm !== 'NOT_APPLICABLE'"
                class="nv__hitem"
              >
                <span class="nv__hname">realm</span>
                <LbStatusTag kind="service" :status="health[record.id].realm" small />
              </span>
              <!-- **逐实例列出来并点名。** 一台机器上可以有好几个 Mieru 入口,
                   合成一行的话,挂了哪一个看不出来 —— 而它们是各自独立的进程,
                   要去救的也只是其中一个。 -->
              <span
                v-for="m in health[record.id].mieru ?? []"
                :key="m.inbound_id"
                class="nv__hitem"
              >
                <span class="nv__hname lb-ellipsis" :title="m.display_name">{{ m.display_name }}</span>
                <LbStatusTag kind="service" :status="m.state" small />
              </span>
              <span v-if="health[record.id].recover_error" class="nv__hfail">
                自动恢复失败
              </span>
              <span v-else-if="health[record.id].recovered" class="nv__hok">已自动拉起</span>
            </div>
            <!-- 读不到时显示「—」,不显示 0 也不显示「正常」 ——
                 读不到和真的正常长得一模一样,那是最容易骗到人的一种失败。 -->
            <span v-else class="nv__dash">—</span>
          </template>

          <template v-else-if="column.key === 'run'">
            <div class="nv__stack">
              <LbStatusTag kind="node" :status="record.status" />
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

          <template v-else-if="column.key === 'config'">
            <LbStatusTag
              :meta="configStatusMeta[configState(record)]"
              :suffix="`rev ${record.config_revision}`"
            />
            <div class="nv__tier">{{ tierSummary(record) }}</div>
          </template>

          <template v-else-if="column.key === 'sync'">
            <LbTimeText
              :value="record.last_heartbeat_at"
              :warn-after-ms="threshold.metricsStaleMs"
              :danger-after-ms="threshold.metricsStaleMs * 6"
            />
          </template>

          <template v-else-if="column.key === 'cycle'">
            <!-- 理由同上:中转主机没有任何计数,写明比画一条 0 更诚实。 -->
            <div v-if="record.role === 'RELAY'" class="nv__reset">中转主机,面板不计流量</div>
            <LbQuotaBar
              v-else
              :used-bytes="cycles[record.id]?.used_bytes ?? null"
              :quota-bytes="cycles[record.id]?.quota_bytes ?? record.traffic_quota_bytes"
              :warning-level="cycles[record.id]?.warning_level"
            />
            <div v-if="billingNote(record.id)" class="nv__reset">{{ billingNote(record.id) }}</div>
            <div v-if="cycleResetText(record.id)" class="nv__reset">
              {{ cycleResetText(record.id) }}
            </div>
          </template>

          <template v-else-if="column.key === 'actions'">
            <div class="nv__actions">
              <a-button
                size="small"
                :type="primaryAction(record) === 'detail' ? 'default' : 'primary'"
                :loading="!!busy[record.id]"
                @click="runPrimary(record)"
              >
                {{ actionLabel[primaryAction(record)] }}
              </a-button>
              <a-button v-if="primaryAction(record) !== 'detail'" size="small" @click="openDetail(record.id)">
                详情
              </a-button>
              <a-dropdown placement="bottomRight">
                <a-button
                  size="small"
                  :aria-label="`${record.display_name || record.name} 的更多操作`"
                  :title="`${record.display_name || record.name} 的更多操作`"
                >
                  ⋯
                </a-button>
                <template #overlay>
                  <a-menu>
                    <a-menu-item @click="openEdit(record)">编辑节点</a-menu-item>
                    <a-menu-item @click="run(record.id, '探测', () => api.probeNode(record.id), '探测完成')">
                      探测
                    </a-menu-item>
                    <a-menu-item
                      @click="run(record.id, '同步流量', () => api.syncNodeTraffic(record.id), '流量已同步')"
                    >
                      同步流量
                    </a-menu-item>
                    <a-menu-item @click="confirmDeploy(record)">部署</a-menu-item>
                    <a-menu-divider />
                    <a-menu-item :danger="record.status !== 'DISABLED'" @click="confirmToggle(record)">
                      {{ record.status === 'DISABLED' ? '启用节点' : '禁用节点' }}
                    </a-menu-item>
                    <!-- 删除、卸载、重置主机密钥这三个不可逆的在详情页的危险区,
                         那里才有空间把「删记录不动机器」和「动机器不删记录」说清楚。 -->
                    <a-menu-item @click="openDetail(record.id)">更多操作(详情页)</a-menu-item>
                  </a-menu>
                </template>
              </a-dropdown>
            </div>
          </template>
        </template>
      </a-table>
    </a-card>

    <NodeFormModal
      v-model:open="formOpen"
      :node="editing"
      :tiers="tiers"
      :next-reset-at="editing ? (cycles[editing.id]?.next_reset_at ?? null) : null"
      @saved="
        (id) => {
          load()
          // 新建之后直接进详情页:接下来要做的探测、安装、部署全在那里,
          // 留在列表上等于让人再点一次。
          if (!editing) openDetail(id)
        }
      "
      @deploy="(id) => run(id, '部署', () => api.deployNode(id), '部署已执行,详情见部署记录')"
    />

    <a-modal v-model:open="keyOpen" title="面板 SSH 公钥" :width="620" :footer="null">
      <p class="nv__key-note">
        新增节点时装进节点 <code>authorized_keys</code> 的就是这一行。面板对节点的所有操作都用它,
        轮换或吊销时不必动你自己的日常密钥。这一行是公钥,贴到哪里都不构成泄露。
      </p>
      <LbCopyField :value="panelKey || '(尚未生成 —— 首次连接节点时自动生成)'" />
    </a-modal>

    <!-- 执行中不可关闭:部署一旦发出就在节点上跑,关掉弹窗不会取消它。 -->
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
.nv {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.nv__head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
}

.nv__title {
  margin: 0;
  font-size: 19px;
  font-weight: 600;
}

.nv__sub {
  margin-top: 3px;
  font-size: 12.5px;
  color: #6b7480;
}

.nv__metrics {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 16px;
}

.nv__inner {
  margin-left: 6px;
  font-family: 'IBM Plex Mono', ui-monospace, monospace;
  font-size: 10.5px;
  color: #6b7480;
}

.nv__host {
  font-size: 11px;
  color: #6b7480;
}

/* 协议标记。用中性底色而不是彩色 —— 协议不是状态,没有好坏之分,
   给它上色会跟旁边真正表达状态的那几个 LbStatusTag 抢注意力。
   两个值都取自 tokens.ts(bgSubtle / text2)。 */
.nv__proto {
  display: inline-block;
  margin-right: 6px;
  padding: 0 4px;
  border-radius: 3px;
  background: #f1f3f5;
  color: #576070;
  font-size: 10px;
  letter-spacing: 0.02em;
}

/* 角色与链式标记。同样用中性底色 —— 它们描述的是「这台机器是什么」,
   不是状态,上色会跟旁边真正表达状态的 LbStatusTag 抢注意力。
   取值同样来自 tokens.ts(bgSubtle / text2)。 */
.nv__role,
.nv__chain {
  display: inline-block;
  margin-left: 6px;
  padding: 0 4px;
  border-radius: 3px;
  background: #f1f3f5;
  color: #576070;
  font-size: 10px;
  letter-spacing: 0.02em;
}

.nv__hitem {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.nv__hname {
  font-size: 12px;
  color: #6B7480;
}
.nv__hfail {
  font-size: 12px;
  color: #B4291D;
}
.nv__hok {
  font-size: 12px;
  color: #1B7A4B;
}
.nv__dash {
  color: #8A93A0;
}

.nv__stack {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 3px;
}

.nv__stack--row {
  flex-direction: row;
  flex-wrap: wrap;
  gap: 6px;
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
}

.nv__card-maint {
  font-size: 11.5px;
  color: #5f52a0;
}

.nv__pager {
  align-self: center;
  padding: 4px 0 2px;
}

.nv__maint {
  max-width: 150px;
  font-size: 10.5px;
  color: #5f52a0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.nv__busy {
  font-size: 10.5px;
  color: #2563b8;
}

.nv__tier {
  margin-top: 3px;
  font-size: 10.5px;
  color: #6b7480;
}

/* 排序值。跟内部名称一样是运维视角的信息,弱化处理,不跟展示名称抢视线。 */
.nv__sort {
  margin-right: 6px;
  font-size: 10.5px;
  color: #6b7480;
}

.nv__reset {
  margin-top: 3px;
  font-size: 10.5px;
  color: #6b7480;
}

.nv__actions {
  display: flex;
  align-items: center;
  gap: 6px;
}

.nv__key-note {
  margin: 0 0 12px;
  font-size: 12.5px;
  line-height: 1.75;
  color: #576070;
}

.nv__key-note code {
  padding: 1px 5px;
  background: #f1f3f5;
  border-radius: 3px;
  font-family: 'IBM Plex Mono', ui-monospace, monospace;
  font-size: 12px;
}

.nv__batch-foot {
  display: flex;
  justify-content: flex-end;
  margin-top: 16px;
}

@media (max-width: 1279px) {
  .nv__metrics {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}

@media (max-width: 767px) {
  .nv__metrics {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
