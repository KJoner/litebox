<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type ExternalProxy,
  type Node,
  type ProxyUser,
  type QuickChainPreview,
} from '@/api/client'
import { LbInfoTip, lbDangerConfirm } from '@/components/lb'

/**
 * 快速组链(VNext CHAIN02–05):在入口机上新建一个入口,出口指向已有的落地。
 *
 * 两个入口进来:从某台机器的入口菜单「作为入口组链」(预填入口机),从某个自建入口或外部代理的菜单
 * 「作为出口组链」(预填出口)。外部代理只能当出口 —— 选中它不会让它变成可部署的服务端入口。
 *
 * 路径、新建与改动清单、问题全部来自后端的预览(只读库),前端不推导;有问题就不让保存 ——
 * 不能保存一条注定不可用的链。保存只写库;「保存并应用」再走已有的两阶段下发(先落地、后入口机、真实拨测)。
 * 这是**服务端链路**:跳点都跑在面板管的机器上,与客户端配置里的 detour / dialer-proxy 不是一回事。
 */
const props = defineProps<{
  open: boolean
  nodes: Node[]
  externals: ExternalProxy[]
  preset: { entryNodeId?: number; targetKind?: 'INBOUND' | 'EXTERNAL'; targetId?: number } | null
}>()
const emit = defineEmits<{ 'update:open': [boolean]; saved: [] }>()

const form = ref({
  entry_node_id: undefined as number | undefined,
  display_name: '',
  protocol: 'SHADOWSOCKS' as 'SHADOWSOCKS' | 'VLESS_REALITY',
  listen_port: undefined as number | undefined,
  target_kind: 'INBOUND' as 'INBOUND' | 'EXTERNAL',
  target_inbound_id: undefined as number | undefined,
  target_external_id: undefined as number | undefined,
  user_ids: [] as number[],
})
const users = ref<ProxyUser[]>([])
const preview = ref<QuickChainPreview | null>(null)
const previewError = ref('')
const saving = ref('')

const entryNodes = computed(() => props.nodes.filter((n) => n.role !== 'RELAY'))
const landingOptions = computed(() =>
  props.nodes
    .filter((n) => n.role !== 'RELAY' && n.id !== form.value.entry_node_id)
    .flatMap((n) =>
      (n.inbounds ?? []).map((i) => ({
        value: i.id,
        label: `${n.display_name || n.name} / ${i.display_name}${i.deployed_protocol ? '' : '(未部署过)'}${i.enabled ? '' : '(已停用)'}`,
      })),
    ),
)
const externalOptions = computed(() =>
  props.externals.map((p) => ({
    value: p.id,
    label: p.dialable_by_node ? p.display_name : `${p.display_name}(不能当出口:${p.dialable_reason})`,
    disabled: !p.dialable_by_node,
  })),
)
const userOptions = computed(() => users.value.map((u) => ({ value: u.id, label: `${u.display_name}(${u.user_code})` })))

function body() {
  return {
    entry_node_id: form.value.entry_node_id ?? 0,
    display_name: form.value.display_name.trim(),
    protocol: form.value.protocol,
    listen_port: form.value.listen_port ?? 0,
    target_kind: form.value.target_kind,
    target_id: (form.value.target_kind === 'INBOUND' ? form.value.target_inbound_id : form.value.target_external_id) ?? 0,
    user_ids: form.value.user_ids,
  }
}

watch(
  () => props.open,
  async (open) => {
    if (!open) return
    const p = props.preset ?? {}
    form.value = {
      entry_node_id: p.entryNodeId,
      display_name: '',
      protocol: 'SHADOWSOCKS',
      listen_port: undefined,
      target_kind: p.targetKind ?? 'INBOUND',
      target_inbound_id: p.targetKind !== 'EXTERNAL' ? p.targetId : undefined,
      target_external_id: p.targetKind === 'EXTERNAL' ? p.targetId : undefined,
      user_ids: [],
    }
    preview.value = null
    try {
      users.value = (await api.users()).items
    } catch {
      users.value = []
    }
  },
  { immediate: true },
)

const ready = computed(
  () =>
    !!form.value.entry_node_id &&
    !!form.value.listen_port &&
    !!(form.value.target_kind === 'INBOUND' ? form.value.target_inbound_id : form.value.target_external_id),
)

let seq = 0
let timer: ReturnType<typeof setTimeout> | undefined
watch(
  form,
  () => {
    if (!props.open || !ready.value) {
      preview.value = null
      return
    }
    clearTimeout(timer)
    timer = setTimeout(async () => {
      const mine = ++seq
      try {
        const pv = await api.quickChainPreview(body())
        if (mine === seq) {
          preview.value = pv
          previewError.value = ''
        }
      } catch (e) {
        if (mine === seq) previewError.value = e instanceof ApiError ? e.message : '预览失败'
      }
    }, 300)
  },
  { deep: true },
)

const canSave = computed(
  () => ready.value && !!form.value.display_name.trim() && !!preview.value && preview.value.problems.length === 0 && !saving.value,
)

function close() {
  emit('update:open', false)
}

function save(apply: boolean) {
  const pv = preview.value
  if (!pv) return
  if (!apply) {
    void doSave(false)
    return
  }
  close()
  lbDangerConfirm({
    title: '保存并应用这条链?',
    okText: '保存并应用',
    impacts: [...pv.creates, ...pv.modifies],
    footer: '多台机器按顺序分阶段下发,不是一个能原子回滚的事务:失败时已下发的那台保持新配置,入口机自动回滚。',
    onOk: () => {
      void doSave(true)
    },
  })
}

async function doSave(apply: boolean) {
  saving.value = apply ? '正在保存并下发(先落地、后入口机)' : '正在保存'
  try {
    const b = body()
    const res = await api.quickChain(b)
    if (res.access_error) message.warning(`入口已建好,但「仅指定用户」没写成:${res.access_error}`)
    if (!apply) {
      message.success('已保存草稿:入口与出口已写进库,还没有下发到任何机器')
      emit('saved')
      close()
      return
    }
    const r = await api.applyChain(res.inbound.id, {
      target_kind: b.target_kind,
      target_inbound_id: b.target_kind === 'INBOUND' ? b.target_id : 0,
      target_external_id: b.target_kind === 'EXTERNAL' ? b.target_id : 0,
    })
    if (r.error) message.error(`入口已保存,但下发没完成:${r.error}`)
    else message.success('链路已生效')
    emit('saved')
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '保存失败')
  } finally {
    saving.value = ''
  }
}
</script>

<template>
  <a-modal :open="open" title="快速组链" width="680px" :mask-closable="!saving" :closable="!saving" @cancel="close">
    <div class="qc">
      <div class="qc__mode">
        服务端链路
        <LbInfoTip text="每一跳都跑在面板管理的机器上:入口机的 sing-box 把流量交给下一跳。与客户端配置里的 detour / dialer-proxy(客户端链路)不是一回事,也不会改用户的客户端配置。" />
      </div>
      <a-form layout="vertical" class="qc__form">
        <div class="qc__row">
          <a-form-item label="入口机" class="qc__grow">
            <a-select
              v-model:value="form.entry_node_id"
              placeholder="选择要新建入口的机器"
              :options="entryNodes.map((n) => ({ value: n.id, label: n.display_name || n.name }))"
            />
          </a-form-item>
          <a-form-item label="入口名称" class="qc__grow">
            <a-input v-model:value="form.display_name" placeholder="出现在订阅里的名字" :maxlength="64" />
          </a-form-item>
        </div>
        <div class="qc__row">
          <a-form-item label="接入协议">
            <a-radio-group v-model:value="form.protocol" button-style="solid" size="small">
              <a-radio-button value="SHADOWSOCKS">Shadowsocks 2022</a-radio-button>
              <a-radio-button value="VLESS_REALITY">VLESS + REALITY</a-radio-button>
            </a-radio-group>
          </a-form-item>
          <a-form-item label="监听端口">
            <a-input-number v-model:value="form.listen_port" :min="1" :max="65535" placeholder="如 31000" />
          </a-form-item>
        </div>
        <a-form-item label="出口">
          <div class="qc__row">
            <a-radio-group v-model:value="form.target_kind" size="small">
              <a-radio value="INBOUND">自建入口</a-radio>
              <a-radio value="EXTERNAL">外部代理</a-radio>
            </a-radio-group>
            <a-select
              v-if="form.target_kind === 'INBOUND'"
              v-model:value="form.target_inbound_id"
              placeholder="选择落地入口"
              :options="landingOptions"
              show-search
              option-filter-prop="label"
              class="qc__grow"
            />
            <a-select
              v-else
              v-model:value="form.target_external_id"
              placeholder="选择外部代理"
              :options="externalOptions"
              show-search
              option-filter-prop="label"
              class="qc__grow"
            />
          </div>
        </a-form-item>
        <a-form-item label="使用用户(可选)">
          <a-select
            v-model:value="form.user_ids"
            mode="multiple"
            :options="userOptions"
            placeholder="不选 = 按访问等级(普通组)开放;选了 = 新入口仅供这些用户"
            show-search
            option-filter-prop="label"
          />
        </a-form-item>
      </a-form>

      <div v-if="previewError" class="qc__bad">{{ previewError }}</div>
      <template v-else-if="preview">
        <div class="qc__path" aria-label="链路路径">
          <template v-for="(h, i) in preview.path.hops" :key="i">
            <div class="qc__hop" :class="{ 'qc__hop--bad': !!h.problem, 'qc__hop--new': h.kind === 'NEW' }">
              <div class="qc__hop-no">第 {{ i + 1 }} 跳</div>
              <div class="qc__hop-name">{{ h.node_name ? `${h.node_name} / ` : '' }}{{ h.name }}</div>
              <div class="qc__hop-meta">{{ h.protocol || '—' }} · TCP 支持 · UDP {{ h.udp }}</div>
              <div class="qc__hop-runs">{{ h.runs }}</div>
              <div v-if="h.problem" class="qc__bad">{{ h.problem }}</div>
            </div>
            <div v-if="i < preview.path.hops.length - 1" class="qc__arrow" aria-hidden="true">→</div>
          </template>
        </div>
        <div class="qc__facts">
          <div><b>最终出口</b>:{{ preview.path.egress || '—' }}</div>
          <div><b>DNS</b>:{{ preview.path.dns }}</div>
        </div>
        <div class="qc__lists">
          <div>
            <b>将新建</b>
            <ul><li v-for="c in preview.creates" :key="c">{{ c }}</li></ul>
          </div>
          <div>
            <b>会改动</b>
            <ul><li v-for="m in preview.modifies" :key="m">{{ m }}</li></ul>
          </div>
        </div>
        <div v-if="preview.problems.length" class="qc__problems">
          <b>这条链现在建不起来</b>
          <ul><li v-for="p in preview.problems" :key="p">{{ p }}</li></ul>
        </div>
      </template>
      <div v-else class="qc__note">选好入口机、端口与出口之后,这里显示完整路径。</div>
      <div v-if="saving" class="qc__note">{{ saving }}…</div>
    </div>
    <template #footer>
      <a-button :disabled="!!saving" @click="close">取消</a-button>
      <a-button :disabled="!canSave" @click="save(false)">只保存(不下发)</a-button>
      <a-button type="primary" :disabled="!canSave" :loading="!!saving" @click="save(true)">保存并应用</a-button>
    </template>
  </a-modal>
</template>

<style scoped>
.qc {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.qc__mode {
  display: inline-flex;
  align-items: center;
  gap: var(--sp-1);
  font-size: var(--fs-xs);
  color: var(--text2);
}
.qc__form :deep(.ant-form-item) {
  margin-bottom: var(--sp-2);
}
.qc__row {
  display: flex;
  gap: var(--sp-3);
  align-items: center;
  flex-wrap: wrap;
}
.qc__grow {
  flex: 1;
  min-width: 180px;
}
.qc__path {
  display: flex;
  align-items: stretch;
  gap: var(--sp-2);
  overflow-x: auto;
  padding-bottom: var(--sp-1);
}
.qc__hop {
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
  background: var(--surface2);
  padding: var(--sp-2) var(--sp-3);
  min-width: 170px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.qc__hop--new {
  border-color: var(--brand-border);
}
.qc__hop--bad {
  border-color: var(--bad);
}
.qc__hop-no,
.qc__hop-runs,
.qc__hop-meta {
  font-size: var(--fs-xs);
  color: var(--text2);
}
.qc__hop-name {
  font-weight: 600;
}
.qc__arrow {
  align-self: center;
  color: var(--text3);
}
.qc__facts,
.qc__lists,
.qc__note {
  font-size: var(--fs-xs);
  color: var(--text2);
}
.qc__lists {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--sp-3);
}
.qc__lists ul,
.qc__problems ul {
  margin: 4px 0 0;
  padding-left: 18px;
}
.qc__problems {
  font-size: var(--fs-xs);
  color: var(--bad);
  background: var(--bad-bg);
  border-radius: var(--r-group);
  padding: var(--sp-2) var(--sp-3);
}
.qc__bad {
  color: var(--bad);
  font-size: var(--fs-xs);
}
@media (max-width: 767px) {
  .qc__lists {
    grid-template-columns: 1fr;
  }
}
</style>
