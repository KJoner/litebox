<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type ExternalProxy,
  type MieruInbound,
  type Node,
} from '@/api/client'
import { lbDangerConfirm } from '@/components/lb'

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
const landingNodes = ref<Node[]>([])
const externalProxies = ref<ExternalProxy[]>([])

const form = ref({
  target_kind: 'INBOUND' as 'INBOUND' | 'EXTERNAL',
  // undefined 而不是 0:0 会被 a-select 当成一个已选中的值渲染出来,
  // 而占位提示永远不出现 —— 管理员看到一个像是选好了的框,
  // 点确定却被拦下来说没选落地。
  target_inbound_id: undefined as number | undefined,
  target_external_id: undefined as number | undefined,
  socks_port: 11081,
})

watch(
  () => props.open,
  async (open) => {
    if (!open) return
    const cur = props.inbound
    form.value = {
      target_kind: (cur?.chain_target_kind as 'INBOUND' | 'EXTERNAL') || 'INBOUND',
      target_inbound_id: cur?.chain_target_inbound_id || undefined,
      target_external_id: cur?.chain_target_external_id || undefined,
      // 已经配过就沿用原来那个端口:换一个只会让这次下发多断一次连接,
      // 而端口本身没有任何需要变的理由。
      socks_port: cur?.egress_socks_port || 11081,
    }
    try {
      const [ns, ps] = await Promise.all([api.nodes(), api.externalProxies()])
      landingNodes.value = (ns.items ?? []).filter(
        (n) => n.role !== 'RELAY' && n.id !== props.node.id,
      )
      externalProxies.value = (ps.items ?? []).filter((p) => p.subscription_enabled)
    } catch {
      // 拉不到候选不影响"解除出口"那条路 —— 它不需要任何候选。
      landingNodes.value = []
      externalProxies.value = []
    }
  },
)

/**
 * 可选的落地入站。
 *
 * **同机的入站不在列表里**(上面按 node.id 过滤掉了):流量确实多走了
 * mita → sing-box 这一跳,但出口 IP 一个字节都没变 —— 后端也会拒绝,
 * 在这里就不列出来,省得管理员选完才被告知。
 *
 * 没部署过的落地也标出来:它上面还没有这条链路的凭据,选了会被拦下。
 */
const inboundOptions = computed(() =>
  landingNodes.value.flatMap((n) =>
    (n.inbounds ?? [])
      .filter((i) => i.enabled)
      .map((i) => ({
        value: i.id,
        label:
          `${n.display_name || n.name} / ${i.display_name}` +
          (i.deployed_protocol ? '' : '(还没上过节点)'),
      })),
  ),
)

const externalOptions = computed(() =>
  externalProxies.value.map((p) => ({ value: p.id, label: p.display_name })),
)

const hasChain = computed(() => !!props.inbound?.chain_target_kind)

/** 落地是外部代理时不需要下发落地那一台 —— 凭据是机场给的。 */
const landingIsExternal = computed(() => form.value.target_kind === 'EXTERNAL')

function submit() {
  const f = form.value
  if (f.target_kind === 'INBOUND' && !f.target_inbound_id) {
    message.warning('请选择落地入口')
    return
  }
  if (f.target_kind === 'EXTERNAL' && !f.target_external_id) {
    message.warning('请选择外部代理')
    return
  }
  if (!f.socks_port) {
    message.warning('请填写回环端口')
    return
  }

  const steps = [
    ...(landingIsExternal.value
      ? []
      : ['先下发**落地那台机器** —— 把这条链路的凭据写进它的用户列表。'
        + '不做这一步,后面两步都会失败,而报错会落在这台机器上,看起来像是链路不通。']),
    `再下发 **${props.node.display_name || props.node.name} 的 sing-box** —— ` +
      '加上那个只监听回环的 socks 入站。这一下会重启 sing-box,' +
      '踢掉这台机器上**全部 sing-box 入口**的在线连接(Mieru 入口不受影响)。',
    '最后下发**这个 Mieru 入口** —— 让 mita 的出口指过去。' +
      '这一下只断这一个入口的连接,同机其他入口与 sing-box 都不受影响。',
  ]
  lbDangerConfirm({
    title: `确认把「${props.inbound?.display_name}」的出口改到落地?`,
    okType: 'primary',
    okText: '保存出口设置',
    impacts: [
      '**现在只写数据库,节点上一个字节都不动。** 生效要按下面的顺序下发:',
      ...steps.map((s, i) => `${i + 1}. ${s}`),
    ],
    footer:
      'mita 的出口代理只认 SOCKS5,拨不出 VLESS 或 Shadowsocks —— ' +
      '中间借道本机 sing-box 那一跳是协议限制,不是面板绕的路。',
    onOk: () => void doSave(),
  })
}

async function doSave() {
  running.value = '正在保存出口设置'
  emit('busy', running.value)
  try {
    await api.setMieruChain(props.inbound!.id, {
      kind: form.value.target_kind,
      target_id:
        form.value.target_kind === 'INBOUND'
          ? form.value.target_inbound_id
          : form.value.target_external_id,
      socks_port: form.value.socks_port,
    })
    emit('update:open', false)
    emit('applied')
    message.success('已保存。按上面的顺序下发之后才会生效')
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '保存失败')
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
      <a-form-item label="落地种类">
        <a-radio-group v-model:value="form.target_kind" size="small" button-style="solid">
          <a-radio-button value="INBOUND">自建节点的入口</a-radio-button>
          <a-radio-button value="EXTERNAL">外部代理</a-radio-button>
        </a-radio-group>
      </a-form-item>

      <a-form-item v-if="form.target_kind === 'INBOUND'" label="落地入口">
        <a-select
          v-model:value="form.target_inbound_id"
          :options="inboundOptions"
          placeholder="选择落地入口"
          show-search
          option-filter-prop="label"
        />
        <div class="mcm__hint">
          <b>同一台机器上的入口不在列表里</b>:流量确实多走了一跳,
          但出口 IP 一个字节都没变。
          <br />
          标着「还没上过节点」的落地选了会被拦下 —— 它上面还没有这条链路的凭据。
        </div>
      </a-form-item>

      <a-form-item v-else label="外部代理">
        <a-select
          v-model:value="form.target_external_id"
          :options="externalOptions"
          placeholder="选择外部代理"
          show-search
          option-filter-prop="label"
        />
        <div class="mcm__hint">
          走 QUIC 的线路(Hysteria2 / TUIC)选了会被拦下:节点上的 sing-box
          是精简构建,拨不动它们。
        </div>
      </a-form-item>

      <a-form-item label="回环端口(mita 与 sing-box 之间那一跳)">
        <a-input-number v-model:value="form.socks_port" :min="1" :max="65535" />
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
        <a-button size="small" type="primary" :loading="!!running" @click="submit">
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
  font-size: 12px;
  line-height: 1.6;
  color: #6B7480;
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
