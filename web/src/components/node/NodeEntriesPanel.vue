<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  PROTOCOL_SHORT,
  type ChainApplyResult,
  type DeployResult,
  type ExternalProxy,
  type NginxFacts,
  type Node,
  type NodeRelay,
} from '@/api/client'
import { LbStatusTag, lbDangerConfirm, type LbStatusMeta } from '@/components/lb'
import { color } from '@/theme/tokens'

/**
 * 节点的「入口」面板:这台机器对外提供的全部入口。
 *
 * 两类入口在同一个列表里,因为对管理员来说它们回答的是同一个问题
 * ——「用户连这台机器的哪个端口、连上之后去哪」:
 *
 *   sing-box 入口   这台机器自己的入站。**至多一条** —— 见下方说明。
 *                   出口可以是本机直连,也可以链到另一个落地。
 *   nginx 转发入口  把字节原样搬到落地,可以有多条,各占一个端口。
 *
 * 叫「入口」而不是「订阅」:面板里「订阅」已经是用户手上那条
 * /sub/{token} 链接,同一个词两个意思会在后台与门户之间来回打架。
 * 而「入口」与下面那一栏「出口」正好成对,一眼看得出方向。
 *
 * **两类入口的操作摩擦不同,必须让它们看起来不同**:
 *   nginx 转发   改了只 reload,在途连接一条不断 → 普通确认;
 *   sing-box     改了要重启,踢掉这台机器上全部在线连接,改出口还连带
 *                部署另一台机器 → lbDangerConfirm,逐条列影响。
 * 合成一种确认之后,管理员会对「点这一下要不要挑时机」失去判断。
 */
const props = defineProps<{
  node: Node
  /** 订阅展开(IPv4 + 可选 IPv6),由页面算好传进来 */
  subEntries: { name: string; addr: string }[]
}>()
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
 * 落地候选**排除中转角色的机器与节点自己**:前者上面没有 sing-box,
 * 转发过去只会得到一条连不上的线路;后者会让流量绕回自己。
 */
const landingNodes = ref<Node[]>([])
const externalProxies = ref<ExternalProxy[]>([])

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

const relays = ref<NodeRelay[]>([])
const loading = ref(false)
const loadError = ref('')
const running = ref('')
const nginx = ref<NginxFacts | null>(null)
const nginxError = ref('')
const lastDeploy = ref<DeployResult | null>(null)
const lastChain = ref<ChainApplyResult | null>(null)

// 表单
const editing = ref<NodeRelay | null>(null)
const formOpen = ref(false)
const form = ref({
  display_name: '',
  listen_port: 0,
  public_port: 0,
  target_kind: 'NODE' as 'NODE' | 'EXTERNAL',
  target_node_id: 0,
  target_external_id: 0,
  access_tier_id: 0,
  sort_order: 0,
  subscription_enabled: true,
  enabled: true,
  public_remark: '',
})

/**
 * 中转角色的机器上没有 sing-box 入站 —— 那一块整个不出现。
 *
 * 显示一个"未配置"的占位会让人以为可以去配一个,而角色一经创建不可更改:
 * 要在这台机器上跑 sing-box,只能删了重建。
 */
const isRelayHost = computed(() => props.node.role === 'RELAY')

/**
 * sing-box 入口至多一条 —— 一个节点一个入站。
 *
 * 这不是界面上的限制,是数据模型的:协议、监听端口、公网端口、REALITY
 * 参数、SS 密钥这十几个字段全挂在 nodes 那一行上,一行只能描述一个入站。
 * 要多条得把它们搬进一张独立的表,那是一次架构级改动。
 *
 * 界面上把这一条明说出来,而不是让「新增入口」里的那一项默默变灰 ——
 * 变灰的按钮只告诉人"不能点",不告诉人"为什么"和"要不要等以后"。
 */
const singboxEntryExists = computed(() => !isRelayHost.value)

/**
 * 当前选择的落地种类下有没有候选。
 *
 * 没有候选时下拉里一个选项都没有,而 v-model 还绑着 0 —— antd 会把它
 * 原样渲染成一个 "0"。那既不是地址也不是名字,看起来像个 bug,
 * 而真实情况是「这个面板上还没有第二台机器可以当落地」。
 */
const chainCandidates = computed(() =>
  chainForm.value.target_kind === 'NODE' ? landingNodes.value : externalProxies.value,
)
const chainReason = computed(() => {
  if (isRelayHost.value) return ''
  if (chainCandidates.value.length > 0) return ''
  return chainForm.value.target_kind === 'NODE'
    ? '还没有别的落地节点可选 —— 中转角色的机器与这台机器自己都不能当落地。'
    : '还没有可用的外部代理。链式落地本版本只支持 Shadowsocks 的外部代理。'
})

const singboxAddHint =
  '一个节点一个 sing-box 入站:协议、端口、REALITY 参数这十几个字段都挂在节点自己那一行上,' +
  '一行只能描述一个入站。要在同一台机器上开第二条(比如再加一个 Shadowsocks 入口),' +
  '需要把这些字段拆到独立的表里,那是一次架构改动。'

/**
 * 协议标记读 deployed_protocol —— 节点上【已经生效】的那个。
 *
 * 读期望值的话,改协议到部署成功之间的窗口里这里会显示新协议,
 * 而订阅下发的、用户实际连的还是旧的 —— 界面与事实分叉,
 * 而分叉的那一头恰好是管理员会相信的那一头。
 */
const protocolMeta = computed<LbStatusMeta>(() => {
  const p = props.node.deployed_protocol
  if (!p) {
    return {
      text: '未部署',
      shape: 'ring',
      fg: color.neutral,
      bg: color.neutralBg,
      bd: color.neutralBorder,
    }
  }
  const pending = p !== props.node.protocol
  return {
    text: pending ? `${PROTOCOL_SHORT[p]} → ${PROTOCOL_SHORT[props.node.protocol]} 待部署` : PROTOCOL_SHORT[p],
    shape: pending ? 'triangle' : 'check',
    fg: pending ? color.warning : color.success,
    bg: pending ? color.warningBg : color.successBg,
    bd: pending ? color.warningBorder : color.successBorder,
  }
})
const chainEnabled = computed(() => props.node.chain_target_kind !== '')

const chainTargetName = computed(() => {
  if (props.node.chain_target_kind === 'NODE') {
    const n = landingNodes.value.find((x) => x.id === props.node.chain_target_node_id)
    return n ? n.display_name || n.name : `节点 #${props.node.chain_target_node_id}`
  }
  if (props.node.chain_target_kind === 'EXTERNAL') {
    const p = externalProxies.value.find((x) => x.id === props.node.chain_target_external_id)
    return p ? p.display_name : `外部代理 #${props.node.chain_target_external_id}`
  }
  return ''
})

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
    target_kind: 'NODE',
    target_node_id: landingNodes.value[0]?.id ?? 0,
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
    target_node_id: r.target_node_id,
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
      `${props.node.display_name || props.node.name} 上监听 ${r.listen_port} 的转发会被撤掉`,
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

// ---------- 链式出站 ----------

const chainForm = ref({
  target_kind: 'NODE' as 'NODE' | 'EXTERNAL',
  target_node_id: 0,
  target_external_id: 0,
})

function applyChain() {
  const targetName =
    chainForm.value.target_kind === 'NODE'
      ? landingNodes.value.find((x) => x.id === chainForm.value.target_node_id)?.display_name
      : externalProxies.value.find((x) => x.id === chainForm.value.target_external_id)
          ?.display_name
  if (!targetName) {
    message.warning('请选择落地')
    return
  }
  lbDangerConfirm({
    title: `把 ${props.node.display_name || props.node.name} 的出口改到「${targetName}」?`,
    impacts: [
      `${props.node.display_name || props.node.name} 上的 sing-box 会重启,这台机器上全部在线连接会断开`,
      ...(chainForm.value.target_kind === 'NODE'
        ? [`落地「${targetName}」也会重新部署一次,它上面的在线连接同样会断开`]
        : []),
      '两次部署有先后:先落地后中转,顺序反了中转会连不上落地',
      '订阅内容不变 —— 用户不会察觉这台机器后面多了一跳',
    ],
    okText: '改出口',
    onOk: () => {
      // 不返回这个 Promise:AntD 只要拿到 Promise 就把确认框留在屏幕上
      // 转圈等它 resolve,而两台机器的部署要几十秒。
      void runApplyChain()
    },
  })
}

async function runApplyChain() {
  running.value = '正在切换出口(两台机器)'
  emit('busy', running.value)
  lastChain.value = null
  try {
    const r = await api.applyChain(props.node.id, { ...chainForm.value })
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

function clearChain() {
  lbDangerConfirm({
    title: `把 ${props.node.display_name || props.node.name} 的出口改回本机直连?`,
    impacts: [
      '这台机器上的 sing-box 会重启,全部在线连接会断开',
      '落地那台也会重新部署一次,以撤掉这条链路的凭据',
      '之后用户的流量从这台机器自己的 IP 出去',
    ],
    okText: '改回直连',
    onOk: () => {
      void runClearChain()
    },
  })
}

async function runClearChain() {
  running.value = '正在改回直连(两台机器)'
  emit('busy', running.value)
  lastChain.value = null
  try {
    const r = await api.clearChain(props.node.id)
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

onMounted(async () => {
  load()
  await loadTargets()
  chainForm.value.target_kind = props.node.chain_target_kind === 'EXTERNAL' ? 'EXTERNAL' : 'NODE'
  chainForm.value.target_node_id =
    props.node.chain_target_node_id || landingNodes.value[0]?.id || 0
  chainForm.value.target_external_id =
    props.node.chain_target_external_id || externalProxies.value[0]?.id || 0
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
            <a-menu-item @click="openCreate">Nginx 转发入口</a-menu-item>
            <!-- sing-box 那一项不做成灰按钮:灰按钮只说"不能点",
                 不说为什么、也不说要不要等以后。这里直接把原因写出来。 -->
            <a-menu-item disabled>
              <span :title="singboxAddHint">sing-box 入口(每台机器一条)</span>
            </a-menu-item>
          </a-menu>
        </template>
      </a-dropdown>
    </div>

    <!-- sing-box 入口。中转角色的机器上没有,这一整块不出现 ——
         显示一个"未配置"的占位会让人以为可以去配一个,而角色不可更改。 -->
    <div v-if="singboxEntryExists" class="nr__sb">
      <div class="nr__sb-head">
        <span class="nr__kind">sing-box</span>
        <span class="nr__sb-name">{{ node.display_name || node.name }}</span>
        <LbStatusTag :meta="protocolMeta" />
        <span class="nr__spacer" />
      </div>
      <div class="nr__sb-body lb-mono">
        公网 {{ node.proxy_port }} → 主机 {{ node.listen_port }}
        <template v-if="node.ipv6_address">
          · IPv6 [{{ node.ipv6_address }}]:{{ node.ipv6_proxy_port || node.proxy_port }}
        </template>
      </div>
      <div class="nr__sb-sub">
        <div>
          订阅里展开成:
          <span v-for="e in subEntries" :key="e.name" class="nr__sb-entry">
            {{ e.name }} <span class="lb-mono">{{ e.addr }}</span>
          </span>
        </div>
        <!-- IPv6 不是第二条节点记录,而是同一条记录在订阅生成时的逻辑展开 ——
             两条指向同一个入站,改 IPv6 保存即生效,不需要重新部署。 -->
        <div v-if="node.ipv6_address">
          两条指向<b>同一个入站</b>,UUID、REALITY 公钥、short ID 完全相同。
        </div>
        <div v-else>
          填上 IPv6 会额外多一条「{{ node.display_name || node.name }}-IPV6」,
          指向同一个入站,同样不需要重新部署。
        </div>
      </div>
      <div v-if="!node.subscription_enabled" class="nr__warn">
        已关闭「下发到用户订阅」,这一条不会进入新生成的订阅。
        节点仍在运行,已导入旧订阅的客户端还能用。
      </div>
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

    <!-- 链式出站。中转角色的机器没有自己的入站,这一块不出现。 -->
    <div v-if="!isRelayHost" class="nr__chain">
      <div class="nr__chain-head">
        <span class="nr__kind">出口</span>
        <span class="nr__note">这台机器上的 sing-box 入口收到的流量从哪里出网。</span>
        <span class="nr__spacer" />
        <a-button v-if="chainEnabled" size="small" danger :disabled="!!running" @click="clearChain">
          改回本机直连
        </a-button>
      </div>
      <div v-if="chainEnabled" class="nr__chain-now">
        当前经 <b>{{ chainTargetName }}</b> 出网,链路凭据
        <span class="lb-mono">{{ node.chain_code || '—' }}</span>
        <div class="nr__note">
          订阅内容不变:用户拿到的还是这台机器的地址与协议,不知道后面还有一跳。
        </div>
      </div>
      <div v-else class="nr__chain-now">当前从这台机器自己的出口直连。</div>

      <div class="nr__chain-form">
        <a-radio-group v-model:value="chainForm.target_kind" size="small" button-style="solid">
          <a-radio-button value="NODE">自建节点</a-radio-button>
          <a-radio-button value="EXTERNAL">外部代理</a-radio-button>
        </a-radio-group>
        <a-select
          v-if="chainForm.target_kind === 'NODE'"
          v-model:value="chainForm.target_node_id"
          size="small"
          class="nr__select"
          placeholder="选择落地节点"
          :disabled="!landingNodes.length"
          :options="landingNodes.map((n) => ({ value: n.id, label: n.display_name || n.name }))"
        />
        <a-select
          v-else
          v-model:value="chainForm.target_external_id"
          size="small"
          class="nr__select"
          placeholder="选择外部代理"
          :disabled="!externalProxies.length"
          :options="externalProxies.map((p) => ({ value: p.id, label: p.display_name }))"
        />
        <a-button
          size="small"
          type="primary"
          :disabled="!!running || !!chainReason"
          @click="applyChain"
        >
          {{ chainEnabled ? '改出口' : '启用链式出口' }}
        </a-button>
      </div>
      <!-- 按钮变灰时必须说出为什么。只变灰的话,管理员会以为是权限问题
           或者页面坏了,而真实原因是「面板上还没有第二台机器」。 -->
      <p v-if="chainReason" class="nr__note">{{ chainReason }}</p>
      <p class="nr__note">
        链式出口会 <b>重启这台机器的 sing-box</b>,全部在线连接会断开;落地是自建节点时,
        它也会重新部署一次。外部代理落地本版本只支持 Shadowsocks。
      </p>
    </div>

    <!-- 两次部署的结果都要显示:失败时管理员必须知道卡在哪台机器上 -->
    <div v-if="lastChain" class="nr__result">
      <div>停在:<b>{{ lastChain.stage }}</b></div>
      <div v-if="lastChain.target_deploy">
        落地部署:{{ lastChain.target_deploy.status }}
      </div>
      <div v-if="lastChain.host_deploy">
        中转主机部署:{{ lastChain.host_deploy.status }}
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
            <a-radio-button value="NODE">自建节点</a-radio-button>
            <a-radio-button value="EXTERNAL">外部代理</a-radio-button>
          </a-radio-group>
          <div class="nr__hint">落地种类建好之后不能改 —— 换种类等于换成另一条线路。</div>
        </a-form-item>
        <a-form-item label="落地">
          <a-select
            v-if="form.target_kind === 'NODE'"
            v-model:value="form.target_node_id"
            :options="landingNodes.map((n) => ({ value: n.id, label: n.display_name || n.name }))"
          />
          <a-select
            v-else
            v-model:value="form.target_external_id"
            :options="externalProxies.map((p) => ({ value: p.id, label: p.display_name }))"
          />
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
.nr__head,
.nr__chain-head {
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
.nr__chain {
  border-top: 1px dashed var(--lb-border);
  padding-top: 12px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.nr__chain-form {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.nr__select {
  min-width: 200px;
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
