<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type ChainApplyResult,
  type ExternalProxy,
  type Node,
  type NodeInbound,
} from '@/api/client'
import { lbDangerConfirm } from '@/components/lb'

/**
 * 改一个 sing-box 入口的出口去向(链式出站)。
 *
 * 独立成组件的理由与 InboundFormModal 相同 —— 现在有两个地方要开它。
 * 而这一个比表单更不能分叉:它的确认文案里写的是**两台机器**上会发生什么,
 * 少列一条的后果是管理员按一个不完整的清单去判断要不要挑时机。
 *
 * **为什么改出口要部署两台机器**:sing-box 的入站是有用户列表的,没有匿名
 * 接入。入口机的出站要连到落地的那个入站,落地的用户列表里就必须有一份
 * 属于这条链路的凭据(chain_000001 这种)。不部署落地的话,入口机连过去
 * 会被拒,节点日志里是一行 `unknown UUID` —— 而部署会在拨测那一步失败
 * 并自动回滚,管理员看到的是「入口节点部署失败」,不会想到问题在另一台上。
 * 落地是**外部代理**时只部署一台:凭据是机场给的,那台机器不归我们管。
 */
const props = defineProps<{
  open: boolean
  inbound: NodeInbound | null
  /** 入口所属的机器,用于文案里点名"哪台会重启"。 */
  node: Node
}>()

const emit = defineEmits<{
  'update:open': [boolean]
  /** 切换完成(成功或失败都算),调用方据此重新拉数据。 */
  applied: [ChainApplyResult | null]
  busy: [label: string]
}>()

const running = ref('')
const landingNodes = ref<Node[]>([])
const externalProxies = ref<ExternalProxy[]>([])

/**
 * 两个 id 用 undefined 而不是 0 表示"还没选"。
 *
 * 0 会被 a-select 当成一个**已选中的值**渲染出来 —— 下拉框里显示一个孤零零的
 * "0",而占位提示("选择落地入口")永远不出现。管理员看到的是一个像是已经
 * 选好了的框,点「下一步」却被拦下来说没选落地。
 */
const form = ref({
  target_kind: 'INBOUND' as 'INBOUND' | 'EXTERNAL',
  target_inbound_id: undefined as number | undefined,
  target_external_id: undefined as number | undefined,
})

/**
 * 落地候选精确到【入站】而不是机器。
 *
 * 一台机器上有两个入口时,「转发到 B」是有歧义的,而歧义的表现是流量进了
 * 管理员没打算用的那个入口(协议、端口、等级都不同),没有任何一层会报错。
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

/**
 * 走 QUIC 的外部代理(Hysteria2 / TUIC)不能当出口:节点上的 sing-box 是
 * 精简构建,不含 with_quic,拨不动它们。在这里就滤掉并说明少了几条 ——
 * 等到部署才发现的话,错误是十几秒后部署记录里的一句
 * "QUIC is not included in this build",而管理员不会想到那是构建选项。
 */
const chainableProxies = computed(() => externalProxies.value.filter((p) => p.dialable_by_node))
const hiddenCount = computed(() => externalProxies.value.length - chainableProxies.value.length)

const chainReason = computed(() => {
  if (form.value.target_kind === 'INBOUND') {
    if (landingInbounds.value.length) return ''
    return '还没有别的落地入口可选 —— 中转角色的机器与这台机器自己都不能当落地。'
  }
  if (chainableProxies.value.length) return ''
  return hiddenCount.value > 0
    ? `${hiddenCount.value} 条外部代理都走 QUIC(Hysteria2 / TUIC),节点上的 sing-box 拨不了它们。它们照常发给用户直连,只是不能当出口。`
    : '还没有可用的外部代理。'
})

const nodeLabel = computed(() => props.node.display_name || props.node.name)

function targetName(i: NodeInbound): string {
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

// 候选只在打开时拉。节点详情每次进来都带着的话,会白拉两个接口 ——
// 而这两个列表只在配置出口的那几秒里用得上。
watch(
  () => props.open,
  async (open) => {
    if (!open || !props.inbound) return
    const i = props.inbound
    form.value = {
      target_kind: i.chain_target_kind === 'EXTERNAL' ? 'EXTERNAL' : 'INBOUND',
      target_inbound_id: i.chain_target_inbound_id || undefined,
      target_external_id: i.chain_target_external_id || undefined,
    }
    try {
      const [ns, ps] = await Promise.all([api.nodes(), api.externalProxies()])
      // 落地候选排除中转角色的机器与这个入口自己所在的机器:前者上面没有
      // sing-box,后者会让流量绕回自己 —— 出口 IP 一个字节都没变,
      // 而管理员以为配了一条链路。
      landingNodes.value = (ns.items ?? []).filter(
        (n) => n.role !== 'RELAY' && n.id !== props.node.id,
      )
      externalProxies.value = ps.items ?? []
    } catch {
      // 拉不到候选不该把弹窗变成一片空白,chainReason 会说明为什么选不了。
      landingNodes.value = []
      externalProxies.value = []
    }
    if (!form.value.target_inbound_id) {
      form.value.target_inbound_id = landingInbounds.value[0]?.value
    }
    if (!form.value.target_external_id) {
      form.value.target_external_id = chainableProxies.value[0]?.id
    }
  },
  // **必须 immediate**。调用方(入口管理页)只在选中某一行之后才创建这个组件,
  // 那时 open 已经是 true 了 —— 没有这一句的话 watch 一次都不会触发:
  // 候选拉不到、表单不初始化,而界面上显示的是"还没有别的落地入口可选",
  // 一句完全正确的解释配上一个完全错误的前提。
  { immediate: true },
)

function confirmApply() {
  const inbound = props.inbound
  if (!inbound) return
  const name =
    form.value.target_kind === 'INBOUND'
      ? landingInbounds.value.find((x) => x.value === form.value.target_inbound_id)?.label
      : chainableProxies.value.find((x) => x.id === form.value.target_external_id)?.display_name
  if (!name) {
    message.warning('请选择落地')
    return
  }
  emit('update:open', false)
  lbDangerConfirm({
    title: `把入口「${inbound.display_name}」的出口改到「${name}」?`,
    impacts: [
      `${nodeLabel.value} 上的 sing-box 会重启,这台机器上【全部入口】的在线连接都会断开`,
      ...(form.value.target_kind === 'INBOUND'
        ? [
            '落地那台机器也会重新部署一次 —— 它的入站用户列表里要加一份这条链路的凭据,' +
              '不加的话入口机连过去会被拒;它上面的在线连接同样会断开',
          ]
        : []),
      '两次部署有先后:先落地后本机,顺序反了本机会连不上落地',
      '订阅内容不变 —— 用户不会察觉这个入口后面多了一跳',
      '拨测只验证链路可用,不验证出口真的落在那台机器上',
    ],
    okText: '改出口',
    onOk: () => {
      // 不返回这个 Promise:AntD 只要拿到 Promise 就把确认框留在屏幕上
      // 转圈等它 resolve,而两台机器的部署要几十秒。
      void runApply(inbound)
    },
  })
}

async function runApply(inbound: NodeInbound) {
  running.value = '正在切换出口(两台机器)'
  emit('busy', running.value)
  try {
    const r = await api.applyChain(inbound.id, {
      target_kind: form.value.target_kind,
      target_inbound_id: form.value.target_inbound_id ?? 0,
      target_external_id: form.value.target_external_id ?? 0,
    })
    if (r.error) message.error(r.error)
    else message.success('出口已切换')
    emit('applied', r.result)
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '切换失败')
    emit('applied', null)
  } finally {
    running.value = ''
    emit('busy', '')
  }
}

function confirmClear() {
  const i = props.inbound
  if (!i) return
  emit('update:open', false)
  lbDangerConfirm({
    title: `把入口「${i.display_name}」的出口改回本机直连?`,
    impacts: [
      `${nodeLabel.value} 上的 sing-box 会重启,全部入口的在线连接都会断开`,
      '落地那台也会重新部署一次,以撤掉这条链路的凭据',
      '之后这个入口的流量从这台机器自己的 IP 出去',
    ],
    okText: '改回直连',
    onOk: () => {
      void runClear(i)
    },
  })
}

async function runClear(i: NodeInbound) {
  running.value = '正在改回直连(两台机器)'
  emit('busy', running.value)
  try {
    const r = await api.clearChain(i.id)
    if (r.error) message.error(r.error)
    else message.success('已改回本机直连')
    emit('applied', r.result)
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '操作失败')
    emit('applied', null)
  } finally {
    running.value = ''
    emit('busy', '')
  }
}
</script>

<template>
  <a-modal
    :open="open"
    :title="`入口「${inbound?.display_name ?? ''}」的出口`"
    ok-text="下一步"
    @update:open="(v: boolean) => emit('update:open', v)"
    @ok="confirmApply"
  >
    <p class="icm__hint">
      所属机器 <b>{{ nodeLabel }}</b>。链式出口只改这一个入口的去向,这台机器上别的入口不受影响。
      订阅内容不变 —— 用户拿到的还是这台机器的地址与协议,不知道后面还有一跳。
    </p>
    <a-form layout="vertical" size="small">
      <a-form-item label="落地种类">
        <a-radio-group v-model:value="form.target_kind" size="small" button-style="solid">
          <a-radio-button value="INBOUND">自建节点的入口</a-radio-button>
          <a-radio-button value="EXTERNAL">外部代理</a-radio-button>
        </a-radio-group>
      </a-form-item>
      <a-form-item label="落地">
        <a-select
          v-if="form.target_kind === 'INBOUND'"
          v-model:value="form.target_inbound_id"
          placeholder="选择落地入口"
          :disabled="!landingInbounds.length"
          :options="landingInbounds"
        />
        <a-select
          v-else
          v-model:value="form.target_external_id"
          placeholder="选择外部代理"
          :disabled="!chainableProxies.length"
          :options="chainableProxies.map((p) => ({ value: p.id, label: p.display_name }))"
        />
      </a-form-item>
    </a-form>
    <!-- 按钮变灰时必须说出为什么。只变灰的话,管理员会以为是权限问题
         或者页面坏了,而真实原因是「面板上还没有第二台机器」。 -->
    <p v-if="chainReason" class="icm__warn">{{ chainReason }}</p>
    <div v-if="inbound?.chain_target_kind" class="icm__now">
      当前经 <b>{{ targetName(inbound) }}</b> 出网,链路凭据
      <span class="lb-mono">{{ inbound.chain_code || '—' }}</span>
      <a-button size="small" type="link" danger :disabled="!!running" @click="confirmClear">
        改回本机直连
      </a-button>
    </div>
  </a-modal>
</template>

<style scoped>
/* 颜色只允许用 tokens.ts 里已有的值:text3 / danger。 */
.icm__hint {
  margin: 0 0 12px;
  font-size: 12px;
  line-height: 1.6;
  color: #6b7480;
}
.icm__warn {
  margin: 8px 0 0;
  font-size: 12px;
  line-height: 1.6;
  color: #b4291d;
}
.icm__now {
  margin-top: 12px;
  font-size: 12px;
  color: #6b7480;
}
</style>
