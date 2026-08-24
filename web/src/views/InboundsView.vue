<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  PROTOCOL_LABEL,
  type AccessTier,
  type DeployResult,
  type DeploymentRecord,
  type MieruInbound,
  type Node,
  type NodeInbound,
} from '@/api/client'
import DeployStepList from '@/components/DeployStepList.vue'
import { LbEmptyState, LbRowCard, LbStatusTag, configStatusMeta, lbDangerConfirm } from '@/components/lb'
import { configState, needsDeploy } from '@/components/lb/derive'
import InboundChainModal from '@/components/node/InboundChainModal.vue'
import InboundDestModal from '@/components/node/InboundDestModal.vue'
import InboundFormModal from '@/components/node/InboundFormModal.vue'
import MieruChainModal from '@/components/node/MieruChainModal.vue'
import MieruInboundFormModal from '@/components/node/MieruInboundFormModal.vue'
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
 * 入口管理:把全部机器上的 sing-box 入口摊平成一张表。
 *
 * 为什么要有这一页 —— 机器多起来之后,「这套系统一共对外开了哪些口子」
 * 这个问题只能靠一台台点进节点详情去拼,而它恰恰是最常被问到的:
 * 排查一条线路、核对某个等级放出去了几个入口、找出哪些还没部署,
 * 全都是**横着看**的问题,而节点详情是竖着切的。
 *
 * **数据只有一个来源**:GET /api/nodes 已经把每台机器的入口一并带出来了
 * (Node.inbounds)。不新开一个"列出全部入口"的接口 —— 两个接口迟早
 * 分叉,而分叉的表现是这一页显示的入口在节点详情里没有,或者反过来,
 * 全链路不报任何错。
 *
 * **操作复用同一批弹窗与同一份确认文案**(InboundFormModal /
 * InboundChainModal / InboundDestModal / inboundOps / nodeOps)。
 * 影响清单是管理员判断"这一下要不要挑时机"的全部依据,两处各写一份的话,
 * 他会按上次看到的那一版做决定。
 *
 * 中转角色的机器不出现在这里:它上面没有任何 sing-box 入站,
 * 它的 nginx 转发是另一种东西(改了只 reload,在途连接一条不断),
 * 混进来会让这一页的操作摩擦变成两种。
 */

const router = useRouter()
const narrow = useNarrow()

const nodes = ref<Node[]>([])
const tiers = ref<AccessTier[]>([])
const loading = ref(false)
const loadError = ref('')
const running = ref('')

/**
 * 一行 = 一个入口 + 它所在的机器。机器要一起带着 —— 操作都落在机器上。
 *
 * **两类入口在同一张表里**,因为这一页要回答的是同一个问题:
 * 「这套系统一共对外开了哪些口子」。分成两张表要在两处之间来回看才拼得出
 * 一台机器的全貌,而那正是这一页存在的理由。
 *
 * 但它们的下发方式差得很远(sing-box 是整台机器一次、Mieru 是逐入口
 * 各下各的),所以操作按 kind 分支,确认文案也各走各的。
 */
type SingBoxRow = { key: string; kind: 'singbox'; inbound: NodeInbound; node: Node }
type MieruRow = { key: string; kind: 'mieru'; mieru: MieruInbound; node: Node }
type Row = SingBoxRow | MieruRow

const rows = computed<Row[]>(() =>
  nodes.value
    .filter((n) => n.role !== 'RELAY')
    .flatMap((n) => [
      ...(n.inbounds ?? []).map(
        (i): Row => ({ key: `${n.id}-i${i.id}`, kind: 'singbox', inbound: i, node: n }),
      ),
      ...(n.mieru_inbounds ?? []).map(
        (m): Row => ({ key: `${n.id}-m${m.id}`, kind: 'mieru', mieru: m, node: n }),
      ),
    ]),
)

// ---------- 两类入口的取值差异都收在这里 ----------
//
// 模板里逐处 `r.kind === 'singbox' ? ... : ...` 的话,加一列就要在模板里
// 再写一次分支,而漏掉一处的表现是那一列对 Mieru 行显示空白 ——
// 看起来像"这个入口没配这一项"。

const rowName = (r: Row) => (r.kind === 'singbox' ? r.inbound.display_name : r.mieru.display_name)
/** sing-box 的 tag 是配置里的标识;Mieru 没有对应物,给一句类型说明。 */
const rowSub = (r: Row) => (r.kind === 'singbox' ? r.inbound.tag : 'Mieru · mita 实例')
const rowTier = (r: Row) =>
  r.kind === 'singbox' ? r.inbound.access_tier_name : r.mieru.access_tier_name
const rowEnabled = (r: Row) => (r.kind === 'singbox' ? r.inbound.enabled : r.mieru.enabled)
const rowInSub = (r: Row) =>
  r.kind === 'singbox' ? r.inbound.subscription_enabled : r.mieru.subscription_enabled
/** 已经上过节点没有。sing-box 看 deployed_protocol,Mieru 看 deployed_transport。 */
/** 这一行在订阅里会不会多出一条 IPv6 条目。 */
const rowHasIPv6 = (r: Row) =>
  r.kind === 'singbox'
    ? inboundHasIPv6Entry(r.inbound, r.node)
    : !!r.node.ipv6_address && r.mieru.ipv6_enabled

/** Mieru 行的「待下发」标记。颜色取 tokens 里的 warning 那一组。 */
const mieruPendingMeta = {
  text: '待下发',
  shape: 'dot' as const,
  fg: '#8C6D1F',
  bg: '#FCF3E3',
  bd: '#EFDCB4',
}

const rowDeployed = (r: Row) =>
  r.kind === 'singbox' ? !!r.inbound.deployed_protocol : !!r.mieru.deployed_transport

function rowPortText(r: Row): string {
  if (r.kind === 'singbox') return portText(r.inbound.listen_port, r.inbound.public_port)
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

/** Mieru 的传输层标记,与 inboundProtocolMeta 同构。 */
function rowProtocolMeta(r: Row) {
  if (r.kind === 'singbox') return inboundProtocolMeta(r.inbound)
  const m = r.mieru
  const pending = m.deployed_transport && m.deployed_transport !== m.transport
  return {
    text: pending ? `${m.deployed_transport} → ${m.transport} 待下发` : `Mieru ${m.transport}`,
    shape: 'dot' as const,
    fg: pending ? '#8C6D1F' : '#2B5CA8',
    bg: pending ? '#FCF3E3' : '#EAF1FB',
    bd: pending ? '#EFDCB4' : '#C7DAF3',
  }
}

// ---------------------------------------------------------------- 筛选

const kw = ref('')
const filterNode = ref<number | undefined>(undefined)
const filterProtocol = ref<string | undefined>(undefined)
const filterTier = ref<number | undefined>(undefined)
const filterExit = ref<'ALL' | 'DIRECT' | 'CHAIN'>('ALL')
const onlyPending = ref(false)

/** 协议筛选:Mieru 用 'MIERU' 这个取值,与 sing-box 的两种并列。 */
const rowProtocol = (r: Row) => (r.kind === 'singbox' ? r.inbound.protocol : 'MIERU')
const rowTierID = (r: Row) =>
  r.kind === 'singbox' ? r.inbound.access_tier_id : r.mieru.access_tier_id
const rowChained = (r: Row) =>
  r.kind === 'singbox' ? !!r.inbound.chain_target_kind : !!r.mieru.chain_target_kind

const filtered = computed(() =>
  rows.value.filter((r) => {
    if (filterNode.value && r.node.id !== filterNode.value) return false
    if (filterProtocol.value && rowProtocol(r) !== filterProtocol.value) return false
    if (filterTier.value && rowTierID(r) !== filterTier.value) return false
    if (filterExit.value === 'DIRECT' && rowChained(r)) return false
    if (filterExit.value === 'CHAIN' && !rowChained(r)) return false
    // 「只看待部署」= 这个入口自己还没上过节点,或者机器整体有未下发的变更。
    // 两者都算:一个刚加的入口和一台改了协议还没部署的机器,
    // 管理员要做的下一件事是同一个 —— 去下发。
    if (onlyPending.value && rowDeployed(r) && !needsDeploy(r.node)) return false
    const q = kw.value.trim().toLowerCase()
    if (!q) return true
    return [rowName(r), rowSub(r), r.node.name, r.node.display_name, r.node.host, rowPortText(r)]
      .join(' ')
      .toLowerCase()
      .includes(q)
  }),
)

/**
 * 协议筛选的选项。**Mieru 要在里面** —— 它是第三类入口,
 * 漏掉的话这一页会出现"筛选之后总数对不上"而看不出为什么。
 */
const protocolOptions = computed(() => [
  ...Object.entries(PROTOCOL_LABEL).map(([v, l]) => ({ value: v, label: l })),
  { value: 'MIERU', label: 'Mieru' },
])

const nodeOptions = computed(() =>
  nodes.value
    .filter((n) => n.role !== 'RELAY')
    .map((n) => ({ value: n.id, label: nodeLabel(n) })),
)

/** 摘要放在表格上方:这一页存在的理由就是一眼看清全局。 */
const summary = computed(() => {
  const all = rows.value
  return {
    total: all.length,
    nodes: new Set(all.map((r) => r.node.id)).size,
    chained: all.filter(rowChained).length,
    pending: all.filter((r) => !rowDeployed(r) || needsDeploy(r.node)).length,
    disabled: all.filter((r) => !rowEnabled(r)).length,
  }
})

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const [ns, ts] = await Promise.all([api.nodes(), api.accessTiers()])
    nodes.value = ns.items ?? []
    tiers.value = ts.items ?? []
  } catch (e) {
    // 读不到时表格保持空白,不显示「暂无数据」—— 那会被读成「一个入口都没有」,
    // 而那正是管理员打开这一页最不该被骗到的地方。
    nodes.value = []
    loadError.value = e instanceof ApiError ? e.message : '读取入口失败'
  } finally {
    loading.value = false
  }
}

onMounted(load)

// ---------------------------------------------------------------- 显示

/** 出口去向。名字只在这一页解析不了(链式落地可能在另一台机器上),
 *  所以按 id 在全量入口里找 —— 数据本来就都在手上。 */
function exitText(r: Row): string {
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
    return hit ? `${via}${nodeLabel(hit.node)} / ${rowName(hit)}` : `${via}入口 #${inboundID}`
  }
  if (kind === 'EXTERNAL') {
    return `${via}外部代理 #${externalID}`
  }
  return '本机直连'
}

function goNode(r: Row) {
  router.push(`/nodes/${r.node.id}/entries`)
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
  creating.value = false
  target.value = r
  formOpen.value = true
}
function openChain(r: Row) {
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
function remove(r: Row) {
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
    footer: '要看每一步的结果,去这台机器的「入口」Tab 下发 —— 那里有完整的进度弹窗。',
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

const columns = [
  { title: '入口', key: 'name' },
  { title: '所属机器', key: 'node' },
  { title: '协议', key: 'protocol', width: 150 },
  { title: '端口', key: 'port', width: 160 },
  { title: '访问等级', key: 'tier', width: 110 },
  { title: '出口去向', key: 'exit' },
  { title: '状态', key: 'state', width: 190 },
  { title: '操作', key: 'ops', width: 210 },
]
</script>

<template>
  <div class="iv">
    <div class="iv__head">
      <div>
        <h2 class="iv__title">入口管理</h2>
        <p class="iv__sub">
          全部机器上的 sing-box 与 Mieru 入口。中转主机不在这里 —— 它上面
          两种都没有,它的 nginx 转发在各自的节点详情里管。
          <br />
          <b>两类的下发方式不同</b>:sing-box 是整台机器一次(踢掉这台机器上
          全部 sing-box 入口的连接),Mieru 是逐入口各下各的(一个入口一个
          mita 实例,只断那一个)。
        </p>
      </div>
      <a-space>
        <!-- 新增 Mieru 入口不放在这里:它的表单要先知道这台机器上已有几个
             Mieru 入口(端口段冲突检测要用),而那要先挑机器。
             走节点详情的「入口」Tab —— 那里三类的按钮各占一行,
             而且离要改的东西最近。 -->
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
      </a-space>
    </div>

    <div v-if="!loading && !loadError" class="iv__summary">
      <span>{{ summary.total }} 个入口 · 分布在 {{ summary.nodes }} 台机器</span>
      <span v-if="summary.chained">· {{ summary.chained }} 个走链式出口</span>
      <span v-if="summary.disabled">· {{ summary.disabled }} 个已停用</span>
      <!-- 待部署单独标红:它是这一页唯一一个"需要你去做点什么"的数字 -->
      <span v-if="summary.pending" class="iv__pending">
        · {{ summary.pending }} 个待部署
      </span>
    </div>

    <div class="iv__filters">
      <a-input-search v-model:value="kw" placeholder="搜入口名 / tag / 机器 / 端口" allow-clear style="width: 240px" />
      <a-select
        v-model:value="filterNode"
        placeholder="机器"
        allow-clear
        style="width: 170px"
        :options="nodeOptions"
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
      <a-checkbox v-model:checked="onlyPending">只看待部署</a-checkbox>
    </div>

    <p v-if="loadError" class="iv__error">{{ loadError }}</p>

    <!-- 窄屏整表换卡片:AntD 的横向滚动会把最右边的「操作」列推出屏幕。 -->
    <div v-if="narrow" class="iv__cards">
      <LbRowCard v-for="r in filtered" :key="r.key">
        <template #head>
          <span class="iv__name">{{ rowName(r) }}</span>
          <LbStatusTag :meta="addressFamilyMeta(rowHasIPv6(r))" />
          <LbStatusTag :meta="rowProtocolMeta(r)" />
          <LbStatusTag :meta="rowEnabled(r) ? inboundEnabledMeta.on : inboundEnabledMeta.off" />
        </template>

        <div class="iv__dim">
          <a @click="goNode(r)">{{ nodeLabel(r.node) }}</a>
          · {{ rowPortText(r) }}
          · {{ rowTier(r) }}
        </div>
        <div class="iv__dim">
          出口 {{ exitText(r) }} ·
          {{ rowInSub(r) ? '在订阅里' : '已从订阅下架' }}
        </div>
        <div>
          <!-- 配置状态只对 sing-box 行成立:Mieru 是另一个进程、另一份配置。 -->
          <LbStatusTag
            v-if="r.kind === 'singbox'"
            :meta="configStatusMeta[configState(r.node)]"
            :suffix="`rev ${r.node.config_revision}`"
          />
          <LbStatusTag v-else-if="!rowDeployed(r)" :meta="mieruPendingMeta" />
        </div>

        <template #foot>
          <a-button size="small" :disabled="!!running" @click="openEdit(r)">编辑</a-button>
          <a-button size="small" :disabled="!!running" @click="openChain(r)">出口</a-button>
          <a-button
            v-if="r.kind === 'mieru'"
            size="small"
            :disabled="!!running"
            @click="deployMieru(r)"
          >
            下发
          </a-button>
          <a-button
            v-else
            size="small"
            :disabled="!!running"
            @click="doDeploy(r.node)"
          >
            下发整台
          </a-button>
          <a-button size="small" danger :disabled="!!running" @click="remove(r)">删除</a-button>
        </template>
      </LbRowCard>
      <LbEmptyState v-if="!filtered.length && !loading && !loadError" variant="filtered" title="没有匹配的入口" />
    </div>

    <a-table
      v-else
      :columns="columns"
      :data-source="filtered"
      :loading="loading"
      row-key="key"
      size="small"
      :pagination="{ pageSize: 30, hideOnSinglePage: true }"
    >
      <template #emptyText>
        <LbEmptyState v-if="!loadError" variant="filtered" title="没有匹配的入口" />
        <span v-else />
      </template>
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'name'">
          <div class="iv__name">
            {{ rowName(record as Row) }}
            <LbStatusTag :meta="addressFamilyMeta(rowHasIPv6(record as Row))" />
          </div>
          <div class="iv__dim lb-mono">{{ rowSub(record as Row) }}</div>
        </template>

        <template v-else-if="column.key === 'node'">
          <a @click="goNode(record as Row)">{{ nodeLabel((record as Row).node) }}</a>
          <div class="iv__dim">{{ (record as Row).node.host }}</div>
        </template>

        <template v-else-if="column.key === 'protocol'">
          <LbStatusTag :meta="rowProtocolMeta(record as Row)" />
        </template>

        <template v-else-if="column.key === 'port'">
          {{ rowPortText(record as Row) }}
        </template>

        <template v-else-if="column.key === 'tier'">
          {{ rowTier(record as Row) }}
        </template>

        <template v-else-if="column.key === 'exit'">
          {{ exitText(record as Row) }}
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
            :meta="configStatusMeta[configState((record as Row).node)]"
          />
          <LbStatusTag
            v-else-if="!rowDeployed(record as Row)"
            :meta="mieruPendingMeta"
          />
          <div class="iv__dim">
            {{ rowInSub(record as Row) ? '在订阅里' : '已从订阅下架' }}
          </div>
        </template>

        <template v-else-if="column.key === 'ops'">
          <a-space :size="4" wrap>
            <a-button size="small" :disabled="!!running" @click="openEdit(record as Row)">编辑</a-button>
            <a-button size="small" :disabled="!!running" @click="openChain(record as Row)">出口</a-button>
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
                  <a-menu-item @click="doDeploy((record as Row).node)">
                    下发整台机器的 sing-box
                  </a-menu-item>
                  <a-menu-item @click="doRestart((record as Row).node)">
                    重启整台机器的 sing-box
                  </a-menu-item>
                  <a-menu-divider />
                  <a-menu-item danger @click="remove(record as Row)">删除这个入口</a-menu-item>
                </a-menu>
              </template>
            </a-dropdown>
          </a-space>
        </template>
      </template>
    </a-table>

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
/* 颜色只用 tokens.ts 里已有的值:text1 / text3 / warning。 */
.iv__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 8px;
}
.iv__title {
  margin: 0;
  font-size: 18px;
  color: #15181c;
}
.iv__sub {
  margin: 4px 0 0;
  font-size: 12px;
  line-height: 1.6;
  color: #6b7480;
}
.iv__summary {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-bottom: 12px;
  font-size: 12px;
  color: #6b7480;
}
.iv__pending {
  color: #8a5300;
}
.iv__filters {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
}
.iv__error {
  margin: 0 0 12px;
  font-size: 12px;
  color: #b4291d;
}
.iv__name {
  font-weight: 500;
  color: #15181c;
}
.iv__dim {
  font-size: 12px;
  color: #6b7480;
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
  color: #6b7480;
}
.iv__deploy-foot {
  margin-top: 16px;
  text-align: right;
}
</style>
