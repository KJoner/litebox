<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
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
 * 节点的「转发」面板:nginx 透传规则 + sing-box 链式出站。
 *
 * 两块刻意分开显示,因为它们对用户的影响完全不同:
 *
 *   nginx 转发规则  改了只 reload,在途连接一条不断 → 普通确认;
 *   链式出站        改了要重启 sing-box,踢掉这台机器上全部在线连接,
 *                   而且连带部署另一台机器 → lbDangerConfirm,逐条列影响。
 *
 * 合成一块之后,管理员会对「点这一下要不要挑时机」失去判断。
 */
const props = defineProps<{
  node: Node
  /** 可作为落地的自建节点(已排除中转角色与自己) */
  landingNodes: Node[]
  externalProxies: ExternalProxy[]
}>()
const emit = defineEmits<{
  close: []
  /** 有动作在跑时抬起,抽屉据此屏蔽遮罩点击与 ESC */
  busy: [label: string]
  /** 链式变更后节点本身变了,让抽屉重新拉一次 */
  changed: []
}>()

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

const isRelayHost = computed(() => props.node.role === 'RELAY')
const chainEnabled = computed(() => props.node.chain_target_kind !== '')

const chainTargetName = computed(() => {
  if (props.node.chain_target_kind === 'NODE') {
    const n = props.landingNodes.find((x) => x.id === props.node.chain_target_node_id)
    return n ? n.display_name || n.name : `节点 #${props.node.chain_target_node_id}`
  }
  if (props.node.chain_target_kind === 'EXTERNAL') {
    const p = props.externalProxies.find((x) => x.id === props.node.chain_target_external_id)
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
    target_node_id: props.landingNodes[0]?.id ?? 0,
    target_external_id: props.externalProxies[0]?.id ?? 0,
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
      ? props.landingNodes.find((x) => x.id === chainForm.value.target_node_id)?.display_name
      : props.externalProxies.find((x) => x.id === chainForm.value.target_external_id)
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

onMounted(() => {
  load()
  chainForm.value.target_kind = props.node.chain_target_kind === 'EXTERNAL' ? 'EXTERNAL' : 'NODE'
  chainForm.value.target_node_id =
    props.node.chain_target_node_id || props.landingNodes[0]?.id || 0
  chainForm.value.target_external_id =
    props.node.chain_target_external_id || props.externalProxies[0]?.id || 0
})
</script>

<template>
  <section class="nr">
    <div class="nr__head">
      <span class="nr__title">转发</span>
      <span v-if="running" class="nr__running">{{ running }}…</span>
      <span class="nr__spacer" />
      <a-button size="small" :loading="!!running" @click="checkNginx">检查 nginx</a-button>
      <a-button size="small" :loading="!!running" @click="deployNow">立刻下发</a-button>
      <a-button size="small" type="primary" :disabled="!!running" @click="openCreate">
        新增线路
      </a-button>
      <a-button size="small" type="text" @click="emit('close')">收起</a-button>
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

    <!-- nginx 透传规则 -->
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
    </a-table>

    <p class="nr__note">
      转发规则的增删改只会 <b>reload nginx</b>,在途连接一条不断 ——
      所以这里没有需要挑时机的操作。落地未就绪的线路不会出现在任何人的订阅里。
    </p>

    <!-- 链式出站。中转角色的机器没有自己的入站,这一块不出现。 -->
    <div v-if="!isRelayHost" class="nr__chain">
      <div class="nr__chain-head">
        <span class="nr__title">出口去向</span>
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
          :options="landingNodes.map((n) => ({ value: n.id, label: n.display_name || n.name }))"
        />
        <a-select
          v-else
          v-model:value="chainForm.target_external_id"
          size="small"
          class="nr__select"
          :options="externalProxies.map((p) => ({ value: p.id, label: p.display_name }))"
        />
        <a-button size="small" type="primary" :disabled="!!running" @click="applyChain">
          {{ chainEnabled ? '改出口' : '启用链式出口' }}
        </a-button>
      </div>
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
