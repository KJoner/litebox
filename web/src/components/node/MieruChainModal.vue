<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type MieruInbound,
  type NextHopPreview,
  type Node,
} from '@/api/client'
import { lbDangerConfirm } from '@/components/lb'
import NextHopPicker, { type NextHopValue } from '@/components/topology/NextHopPicker.vue'

/**
 * 改一个 Mieru 入口的出口去向。
 *
 * **链路是三跳,中间那一跳是上游定死的:**
 *
 *   用户 ──mieru──► mita 实例 ──socks5──► 本机 sing-box 的一个 socks 入站
 *                                          └─(按入站分流)──► 落地
 *
 * mita 的出口代理只认 SOCKS5(上游枚举里只有这一个值),拨不出 VLESS 或
 * Shadowsocks —— 所以必须借道本机的 sing-box。这不是面板的设计选择,
 * 而是协议限制,界面上要说出来:不然管理员会问"为什么还要重启 sing-box"。
 *
 * **改出口要下发两台机器、两个服务**,而且顺序不能反:
 *
 *   1. 落地那台 —— 把这条链路的凭据(chain_xxxxxx)写进它的用户列表。
 *      不做的话,入口机连过去会被拒,而报错会在十几秒后落在【入口机】的
 *      下发记录里,写着一句 SSH 握手 EOF,看起来完全像是链路不通;
 *   2. 入口机的 sing-box —— 加上那个只监听回环的 socks 入站与链式出站;
 *   3. 这个 Mieru 入口 —— 让 mita 的出口指过去。
 *
 * 落地是**外部代理**时第 1 步不需要:凭据是机场给的,那台机器不归我们管。
 */
const props = defineProps<{
  open: boolean
  inbound: MieruInbound | null
  /** 入口所属的机器,用于文案里点名"哪台会重启"。 */
  node: Node
}>()

const emit = defineEmits<{
  'update:open': [boolean]
  applied: []
  busy: [label: string]
}>()

const running = ref('')
const socksPort = ref(11081)
const choice = ref<NextHopValue | null>(null)
const preview = ref<NextHopPreview | null>(null)
const picker = ref<InstanceType<typeof NextHopPicker> | null>(null)
const fromKey = computed(() => (props.inbound ? `MIERU:${props.inbound.id}` : ''))

// 候选、禁用原因与完整路径来自统一的选择器(v23.3):与 sing-box 入口的出口、转发规则的目标同一套,
// 而且与后端保存时的校验是同一个函数 —— 同机的入口、不能当链式落地的协议、拼不出出站的外部代理、
// 终点是指定地址的转发都在列表里禁用并写明原因。订阅开关关着的线路照样能当出口:那个开关只管它进不进订阅。
watch(
  () => [props.open, props.inbound?.id],
  () => {
    if (!props.open) return
    const cur = props.inbound
    preview.value = null
    // 已经配过就沿用原来那个端口:换一个只会让这次下发多断一次连接。
    socksPort.value = cur?.egress_socks_port || 11081
    choice.value = !cur?.chain_target_kind
      ? null
      : cur.chain_target_kind === 'EXTERNAL'
        ? { kind: 'EXTERNAL', id: cur.chain_target_external_id, endpoint_id: 0 }
        : cur.chain_target_kind === 'RELAY'
          ? { kind: 'RELAY', id: cur.chain_target_relay_id, endpoint_id: cur.chain_target_addr?.endpoint_id ?? 0 }
          : { kind: 'INBOUND', id: cur.chain_target_inbound_id, endpoint_id: cur.chain_target_addr?.endpoint_id ?? 0 }
  },
  { immediate: true },
)

const blocked = computed(() => {
  if (!choice.value) return '请选择出口与连接地址'
  if (!preview.value) return '正在预览完整路径'
  if (preview.value.problems.length) return preview.value.problems[0]
  if (!socksPort.value) return '请填写回环端口'
  return ''
})

const hasChain = computed(() => !!props.inbound?.chain_target_kind)

function submit() {
  if (blocked.value) {
    message.warning(blocked.value)
    return
  }
  const pv = preview.value!
  lbDangerConfirm({
    title: `确认把「${props.inbound?.display_name}」的出口改到 ${pv.dial || '所选目标'}?`,
    okType: 'primary',
    okText: '保存出口设置',
    impacts: [
      '**现在只写数据库,节点上一个字节都不动。** 生效要按下面的顺序下发:',
      `实际连接 ${pv.dial},在「${pv.auth}」认证` +
        (pv.transparent.length ? `(经过透明转发:${pv.transparent.join(' → ')})` : ''),
      ...pv.deploys.map((d, i) => `${i + 1}. 下发 ${d.node_name} 的 ${d.component} —— ${d.why}`),
      `${pv.deploys.length + 1}. 最后下发**这个 Mieru 入口** —— 让 mita 的出口指过去(只断这一个入口的连接)。`,
      ...pv.warnings,
    ],
    footer:
      'mita 的出口代理只认 SOCKS5,拨不出 VLESS 或 Shadowsocks —— ' +
      '中间借道本机 sing-box 那一跳是协议限制,不是面板绕的路。',
    onOk: () => void doSave(pv.revision),
  })
}

async function doSave(revision: string) {
  const v = choice.value
  if (!v) return
  running.value = '正在保存出口设置'
  emit('busy', running.value)
  try {
    await api.setMieruChain(props.inbound!.id, {
      kind: v.kind,
      target_id: v.id,
      target_endpoint_id: v.endpoint_id,
      socks_port: socksPort.value,
      revision,
    })
    emit('update:open', false)
    emit('applied')
    message.success('已保存。按上面的顺序下发之后才会生效')
  } catch (e) {
    if (e instanceof ApiError && e.code === 'STALE_PREVIEW') {
      message.warning('预览之后链路数据变过了,请重新确认出口')
      void picker.value?.reload()
    } else {
      message.error(e instanceof ApiError ? e.message : '保存失败')
    }
  } finally {
    running.value = ''
    emit('busy', '')
  }
}

function clearChain() {
  lbDangerConfirm({
    title: `确认把「${props.inbound?.display_name}」的出口改回直连?`,
    okText: '改回直连',
    impacts: [
      '**现在只写数据库。** 生效要下发这个 Mieru 入口(只断这一个入口的连接)。',
      '之后这个入口的流量从**这台机器自己的 IP** 出去 —— 用户看到的出口会变。',
      '链路凭据不会被删掉:改回去时还是同一个,落地那边的历史流量对得上。',
      '落地上那份凭据要等落地重新下发才会真的消失。',
    ],
    onOk: () =>
      void (async () => {
        running.value = '正在解除出口'
        emit('busy', running.value)
        try {
          await api.clearMieruChain(props.inbound!.id)
          emit('update:open', false)
          emit('applied')
          message.success('已改回直连。下发这个入口之后生效')
        } catch (e) {
          message.error(e instanceof ApiError ? e.message : '解除失败')
        } finally {
          running.value = ''
          emit('busy', '')
        }
      })(),
  })
}
</script>

<template>
  <a-modal
    :open="open"
    width="640px"
    :title="`Mieru 入口「${inbound?.display_name ?? ''}」的出口`"
    :confirm-loading="!!running"
    @update:open="(v: boolean) => emit('update:open', v)"
    @ok="submit"
  >
    <a-alert type="info" show-icon banner class="mcm__intro">
      <template #message>出口要经本机 sing-box 转一跳</template>
      <template #description>
        mita 的出口代理<b>只认 SOCKS5</b>,拨不出 VLESS 或 Shadowsocks。
        所以流量是:mita → 本机 sing-box 的一个回环 socks 入站 → 落地。
        这是协议限制,不是面板绕的路 —— 也因此改出口要连带下发一次 sing-box。
      </template>
    </a-alert>

    <a-form layout="vertical" size="small">
      <a-form-item label="出口(下一跳与连接地址)">
        <NextHopPicker
          v-if="open && inbound"
          ref="picker"
          v-model="choice"
          :from="fromKey"
          @preview="(p) => (preview = p)"
        />
        <p v-if="blocked && choice" class="mcm__warn">{{ blocked }}</p>
      </a-form-item>

      <a-form-item label="回环端口(mita 与 sing-box 之间那一跳)">
        <a-input-number v-model:value="socksPort" :min="1" :max="65535" />
        <div class="mcm__hint">
          它只监听 127.0.0.1,不对外。要避开这台机器上已有的端口 ——
          撞上了保存时就会拒绝,不会等到下发失败。
          <template v-if="inbound?.egress_socks_port">
            <br />
            这个入口原来用的是 {{ inbound.egress_socks_port }},没有理由就别改 ——
            换一个只会让下发多断一次连接。
          </template>
        </div>
      </a-form-item>
    </a-form>

    <template #footer>
      <div class="mcm__footer">
        <a-button v-if="hasChain" danger size="small" :disabled="!!running" @click="clearChain">
          改回直连
        </a-button>
        <span class="mcm__spacer" />
        <a-button size="small" @click="emit('update:open', false)">取消</a-button>
        <a-button size="small" type="primary" :loading="!!running" :disabled="!!blocked" @click="submit">
          保存出口设置
        </a-button>
      </div>
    </template>
  </a-modal>
</template>

<style scoped>
/* 颜色只允许用 tokens.ts 里已有的那几个值 —— 这里是 text3。 */
.mcm__intro {
  margin-bottom: 12px;
}
.mcm__hint {
  margin-top: 4px;
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--text3);
}
/* danger —— 与 InboundChainModal 的 icm__warn 同一档。 */
.mcm__warn {
  margin: 8px 0 0;
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--bad);
}
.mcm__footer {
  display: flex;
  align-items: center;
  gap: 8px;
}
.mcm__spacer {
  flex: 1;
}
</style>
