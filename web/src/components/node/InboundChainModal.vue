<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type ChainApplyResult,
  type NextHopPreview,
  type Node,
  type NodeInbound,
} from '@/api/client'
import { lbDangerConfirm } from '@/components/lb'
import NextHopPicker, { type NextHopValue } from '@/components/topology/NextHopPicker.vue'

/**
 * 改一个 sing-box 入口的出口去向(链式出站)。
 *
 * v23.3 起目标精确到「入口 + 它的哪一条地址」,也可以是一条透明转发规则(经它到达的第一个代理入口认证)。
 * 候选、禁用原因与完整路径都由统一的选择器(NextHopPicker)从后端取 —— 与保存时的校验是同一个函数。
 *
 * **为什么改出口要部署两台(经转发时三台)机器**:sing-box 的入站是有用户列表的,没有匿名接入。
 * 入口机的出站最终在认证终点那个入站认证,那里的用户列表里就必须有一份属于这条链路的凭据(chain_000001 这种)。
 * 不先部署那一台的话,入口机连过去会被拒 —— 而部署会在拨测那一步失败并自动回滚,
 * 管理员看到的是「入口节点部署失败」,不会想到问题在另一台上。认证终点是**外部代理**时不部署它:
 * 凭据是机场给的,那台机器不归我们管。原来的认证终点上的旧凭据暂时保留,等这台机器切过去之后才撤。
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
const choice = ref<NextHopValue | null>(null)
const preview = ref<NextHopPreview | null>(null)
const picker = ref<InstanceType<typeof NextHopPicker> | null>(null)

const nodeLabel = computed(() => props.node.display_name || props.node.name)
const fromKey = computed(() => (props.inbound ? `SINGBOX:${props.inbound.id}` : ''))

watch(
  () => [props.open, props.inbound?.id],
  () => {
    if (!props.open || !props.inbound) return
    const i = props.inbound
    preview.value = null
    choice.value =
      i.chain_target_kind === 'INBOUND'
        ? { kind: 'INBOUND', id: i.chain_target_inbound_id, endpoint_id: i.chain_target_addr?.endpoint_id ?? 0 }
        : i.chain_target_kind === 'EXTERNAL'
          ? { kind: 'EXTERNAL', id: i.chain_target_external_id, endpoint_id: 0 }
          : i.chain_target_kind === 'RELAY'
            ? { kind: 'RELAY', id: i.chain_target_relay_id, endpoint_id: i.chain_target_addr?.endpoint_id ?? 0 }
            : null
  },
  { immediate: true },
)

const blocked = computed(() => {
  if (!choice.value) return '请选择下一跳与连接地址'
  if (!preview.value) return '正在预览完整路径'
  if (preview.value.problems.length) return preview.value.problems[0]
  return ''
})

function confirmApply() {
  const inbound = props.inbound
  if (!inbound) return
  if (blocked.value) {
    message.warning(blocked.value)
    return
  }
  const pv = preview.value!
  emit('update:open', false)
  lbDangerConfirm({
    title: `把入口「${inbound.display_name}」的下一跳改成 ${pv.dial || '所选目标'}?`,
    impacts: [
      `实际连接 ${pv.dial},在「${pv.auth}」认证` +
        (pv.transparent.length ? `(经过透明转发:${pv.transparent.join(' → ')})` : ''),
      ...pv.deploys.map((d, i) => `第 ${i + 1} 步:下发 ${d.node_name} 的 ${d.component} —— ${d.why}`),
      ...pv.warnings,
      `${nodeLabel.value} 上的 sing-box 会重启,这台机器上【全部入口】的在线连接都会断开`,
      '订阅内容不变 —— 用户不会察觉这个入口后面多了一跳',
      '拨测只验证 TCP 链路可用,不验证 UDP,也不验证出口真的落在那台机器上',
    ],
    okText: '保存并下发',
    onOk: () => {
      // 不返回这个 Promise:AntD 只要拿到 Promise 就把确认框留在屏幕上
      // 转圈等它 resolve,而几台机器的部署要几十秒。
      void runApply(inbound, pv.revision)
    },
  })
}

async function runApply(inbound: NodeInbound, revision: string) {
  const v = choice.value
  if (!v) return
  running.value = '正在切换出口'
  emit('busy', running.value)
  try {
    const r = await api.applyChain(inbound.id, {
      target_kind: v.kind,
      target_inbound_id: v.kind === 'INBOUND' ? v.id : 0,
      target_external_id: v.kind === 'EXTERNAL' ? v.id : 0,
      target_relay_id: v.kind === 'RELAY' ? v.id : 0,
      target_endpoint_id: v.endpoint_id,
      revision,
    })
    if (r.error) message.error(r.error)
    else message.success('出口已切换')
    emit('applied', r.result)
  } catch (e) {
    if (e instanceof ApiError && e.code === 'STALE_PREVIEW') {
      message.warning('预览之后链路数据变过了,请重新确认')
      emit('update:open', true)
      void picker.value?.reload()
    } else {
      message.error(e instanceof ApiError ? e.message : '切换失败')
    }
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
      '原来的认证终点那台也会重新部署一次,以撤掉这条链路的凭据',
      '之后这个入口的流量从这台机器自己的 IP 出去',
    ],
    okText: '改回直连',
    onOk: () => {
      void runClear(i)
    },
  })
}

async function runClear(i: NodeInbound) {
  running.value = '正在改回直连'
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
    :title="`入口「${inbound?.display_name ?? ''}」的下一跳`"
    ok-text="下一步"
    width="640px"
    :ok-button-props="{ disabled: !!blocked }"
    @update:open="(v: boolean) => emit('update:open', v)"
    @ok="confirmApply"
  >
    <p class="icm__hint">
      所属机器 <b>{{ nodeLabel }}</b>。只改这一个入口的去向,这台机器上别的入口不受影响。
      目标可以是另一台机器上的入口(选定它的哪一条地址)、一条透明转发规则(协议在它后面第一个代理入口认证),
      或者外部代理。订阅内容不变。
    </p>
    <NextHopPicker
      v-if="open && inbound"
      ref="picker"
      v-model="choice"
      :from="fromKey"
      @preview="(p) => (preview = p)"
    />
    <p v-if="blocked && choice" class="icm__warn">{{ blocked }}</p>
    <div v-if="inbound?.chain_target_kind" class="icm__now">
      当前链路凭据 <span class="lb-mono">{{ inbound.chain_code || '—' }}</span>
      <a-button size="small" type="link" danger :disabled="!!running" @click="confirmClear">
        改回本机直连
      </a-button>
    </div>
  </a-modal>
</template>

<style scoped>
.icm__hint {
  margin: 0 0 12px;
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--text3);
}
.icm__warn {
  margin: 8px 0 0;
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--bad);
}
.icm__now {
  margin-top: 12px;
  font-size: var(--fs-xs);
  color: var(--text3);
}
</style>
