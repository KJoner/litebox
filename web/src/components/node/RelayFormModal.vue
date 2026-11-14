<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AccessTier,
  type InboundEndpointInput,
  type NextHopPreview,
  type NodeAddress,
  type NodeRelay,
} from '@/api/client'
import NextHopPicker, { type NextHopValue } from '@/components/topology/NextHopPicker.vue'
import EndpointsEditor from './EndpointsEditor.vue'

/**
 * nginx / realm 转发规则的新增与编辑(v23.3 从 NodeEntriesPanel 里抽出来,节点详情与链路画布共用)。
 *
 * 一条规则 = 一个入口:用户(或上游入口)连这台机器的某个端口,字节被原样搬到「转发目标」。
 * 转发器不认证 —— 协议与凭据是沿转发走到的第一个代理入口(认证终点)的;目标可以是自建入口的某一条地址、
 * 另一条转发规则(级联)、外部代理,或者手填的地址(纯端口转发,不进订阅、不拨测)。
 * 目标的候选与禁用原因来自统一的选择器(与保存时的校验是同一个函数)。
 */
const props = defineProps<{
  open: boolean
  nodeId: number
  /** 这台机器的管理地址(地址条目编辑器里「管理 IP」那一行用) */
  nodeHost: string
  /** 编辑时传入;新增时为 null */
  relay: NodeRelay | null
  /** 新增时的引擎 */
  engine?: 'NGINX' | 'REALM'
  tiers: AccessTier[]
}>()

const emit = defineEmits<{
  'update:open': [boolean]
  saved: [NodeRelay]
  busy: [label: string]
}>()

const saving = ref(false)
const form = ref({
  engine: 'NGINX' as 'NGINX' | 'REALM',
  display_name: '',
  listen_port: 0,
  public_port: 0,
  access_tier_id: 0,
  sort_order: 0,
  subscription_enabled: true,
  enabled: true,
  public_remark: '',
})
const target = ref<NextHopValue | null>(null)
const preview = ref<NextHopPreview | null>(null)
const nodeAddresses = ref<NodeAddress[]>([])
const endpoints = ref<InboundEndpointInput[]>([])
const picker = ref<InstanceType<typeof NextHopPicker> | null>(null)

const editing = computed(() => props.relay)
const isAddress = computed(() => target.value?.kind === 'ADDRESS')
const fromKey = computed(() => (props.relay ? `${props.relay.engine}:${props.relay.id}` : ''))
/** 编辑一条指定地址的规则时只能改主机与端口:那一种不能与别的落地种类互换。 */
const addressOnly = computed(() => props.relay?.target_kind === 'ADDRESS')

watch(
  () => [props.open, props.relay?.id],
  async () => {
    if (!props.open) return
    const r = props.relay
    preview.value = null
    endpoints.value = []
    form.value = {
      engine: r?.engine ?? props.engine ?? 'NGINX',
      display_name: r?.display_name ?? '',
      listen_port: r?.listen_port ?? 0,
      public_port: r?.public_port ?? 0,
      access_tier_id: r?.access_tier_id ?? props.tiers[0]?.id ?? 1,
      sort_order: r?.sort_order ?? 0,
      subscription_enabled: r?.subscription_enabled ?? true,
      enabled: r?.enabled ?? true,
      public_remark: r?.public_remark ?? '',
    }
    target.value = !r
      ? null
      : r.target_kind === 'ADDRESS'
        ? { kind: 'ADDRESS', id: 0, endpoint_id: 0, host: r.target_host, port: r.target_port }
        : {
            kind: r.target_kind,
            id: r.target_kind === 'INBOUND' ? r.target_inbound_id : r.target_kind === 'RELAY' ? r.target_relay_id : r.target_external_id,
            endpoint_id: r.target_addr?.endpoint_id ?? 0,
          }
    try {
      nodeAddresses.value = (await api.nodeAddresses(props.nodeId)).items
    } catch {
      nodeAddresses.value = []
    }
    if (r && r.target_kind !== 'ADDRESS') {
      try {
        endpoints.value = (await api.inboundEndpoints(r.engine, r.id)).items.map((e) => ({
          // 带上稳定 id:上游的链路按 id 选这条规则的地址,重建会让它们指向一个不存在的条目。
          id: e.id,
          address_id: e.address_id,
          public_port: e.public_port,
          public_port_end: e.public_port_end,
          display_name: e.display_name,
        }))
      } catch {
        endpoints.value = []
      }
    }
  },
  { immediate: true },
)

const blocked = computed(() => {
  if (!form.value.display_name.trim()) return '请填写线路名称'
  if (!form.value.listen_port) return '请填写监听端口'
  if (!target.value) return '请选择转发目标(多条地址时选定连哪一条)'
  if (preview.value?.problems.length) return preview.value.problems[0]
  return ''
})

async function submit() {
  if (blocked.value) {
    message.warning(blocked.value)
    return
  }
  const t = target.value!
  saving.value = true
  emit('busy', editing.value ? '正在保存' : '正在新增')
  const body = {
    ...form.value,
    target_kind: t.kind,
    target_inbound_id: t.kind === 'INBOUND' ? t.id : 0,
    target_external_id: t.kind === 'EXTERNAL' ? t.id : 0,
    target_relay_id: t.kind === 'RELAY' ? t.id : 0,
    target_endpoint_id: t.endpoint_id,
    target_host: t.kind === 'ADDRESS' ? (t.host ?? '') : '',
    target_port: t.kind === 'ADDRESS' ? (t.port ?? 0) : 0,
    revision: preview.value?.revision ?? '',
  }
  try {
    let saved: NodeRelay
    if (editing.value) {
      saved = (await api.updateRelay(editing.value.id, body)).relay
    } else {
      saved = (await api.createRelay(props.nodeId, body)).relay
    }
    // 「指定地址」不进订阅,没有地址条目要存。其余按引擎(NGINX / REALM)存。
    if (t.kind !== 'ADDRESS') {
      await api.saveInboundEndpoints(saved.engine, saved.id, endpoints.value)
    }
    emit('update:open', false)
    emit('saved', saved)
    // 说明「已排队」而不是「已生效」:标脏之后由协调器合并下发,中间有几秒的静默期。
    message.success(
      saved.engine === 'REALM'
        ? '已保存,realm 配置将在数秒内下发(restart,这台机器上全部 realm 线路的在途连接会断开)'
        : '已保存,中转配置将在数秒内下发(只 reload,不断开在途连接)',
    )
  } catch (e) {
    if (e instanceof ApiError && e.code === 'STALE_PREVIEW') {
      message.warning('预览之后链路数据变过了,请重新确认转发目标')
      void picker.value?.reload()
    } else {
      message.error(e instanceof ApiError ? e.message : '保存失败')
    }
  } finally {
    saving.value = false
    emit('busy', '')
  }
}
</script>

<template>
  <a-modal
    :open="open"
    width="640px"
    :title="(editing ? '编辑' : '新增') + (form.engine === 'REALM' ? ' realm ' : ' nginx ') + '转发线路'"
    :confirm-loading="saving"
    :ok-button-props="{ disabled: !!blocked }"
    @update:open="(v: boolean) => emit('update:open', v)"
    @ok="submit"
  >
    <a-form layout="vertical" size="small">
      <a-form-item label="线路名称(会发给用户)">
        <a-input v-model:value="form.display_name" placeholder="例如:香港中转-东京" />
      </a-form-item>
      <a-form-item :label="`监听端口(${form.engine === 'REALM' ? 'realm' : 'nginx'} 在这台机器上真正监听的号码)`">
        <a-input-number v-model:value="form.listen_port" :min="1" :max="65535" />
      </a-form-item>
      <a-form-item v-if="!editing" label="转发引擎">
        <a-radio-group v-model:value="form.engine" size="small" button-style="solid">
          <a-radio-button value="NGINX">nginx</a-radio-button>
          <a-radio-button value="REALM">realm</a-radio-button>
        </a-radio-group>
        <div class="rf__hint">
          引擎建好之后不能改。nginx 改规则 reload、在途连接不断;realm 改规则会重启这台机器上整个 realm 进程、
          在途连接全断,但它由面板下发二进制、同时搬 UDP。
        </div>
      </a-form-item>
      <a-form-item label="转发目标">
        <div v-if="addressOnly" class="rf__manual">
          <a-input
            :value="target?.host"
            placeholder="例如:203.0.113.9 或 landing.example.com"
            @update:value="(v: string) => (target = { kind: 'ADDRESS', id: 0, endpoint_id: 0, host: v, port: target?.port })"
          />
          <a-input-number
            :value="target?.port"
            :min="1"
            :max="65535"
            @update:value="(v: number | null) => (target = { kind: 'ADDRESS', id: 0, endpoint_id: 0, host: target?.host, port: v ?? 0 })"
          />
        </div>
        <NextHopPicker
          v-else-if="open"
          ref="picker"
          v-model="target"
          :from="fromKey"
          :new-kind="editing ? undefined : form.engine"
          :new-node="nodeId"
          :allow-address="!editing"
          @preview="(p) => (preview = p)"
        />
        <div class="rf__hint">
          转发器只搬字节、不认证:用户或上游入口说的协议,在沿转发走到的第一个代理入口认证。
          <template v-if="isAddress">
            <b>指定地址不进订阅、不进门户,也不做拨测</b> —— 面板不知道那个地址背后跑的是什么。域名原样写进配置。
          </template>
        </div>
      </a-form-item>
      <!-- 「指定地址」不进订阅,没有订阅地址条目,只有监听端口。 -->
      <a-form-item v-if="!isAddress" label="这条线路自己的地址(用户或上游入口连哪几个地址、各自的公网端口与名字)">
        <EndpointsEditor
          v-model="endpoints"
          :addresses="nodeAddresses"
          :host="nodeHost"
          :is-mieru="false"
          :name-hint="form.display_name"
        />
        <div class="rf__hint">
          端口留空表示跟随监听端口;NAT 主机上公网 443 映射到主机 20443 时,监听端口填 20443、这里填 443。
          别的入口把这条转发当下一跳时,选的就是这里的某一条 —— 被选中的条目不能删。
        </div>
      </a-form-item>
      <a-form-item v-if="!isAddress" label="访问等级">
        <a-select v-model:value="form.access_tier_id" :options="tiers.map((t) => ({ value: t.id, label: t.name }))" />
        <div class="rf__hint">
          等级不高于它的用户才看得到这条线路;经它级联的话,沿途每一条转发与认证终点的权限都要过,取交集。
        </div>
      </a-form-item>
      <a-form-item label="排序">
        <a-input-number v-model:value="form.sort_order" />
      </a-form-item>
      <a-form-item label="对用户公开的备注">
        <a-input v-model:value="form.public_remark" />
      </a-form-item>
      <a-form-item>
        <a-checkbox v-model:checked="form.enabled">启用(关掉后不再监听这个端口)</a-checkbox>
        <a-checkbox v-if="!isAddress" v-model:checked="form.subscription_enabled">下发到订阅</a-checkbox>
      </a-form-item>
    </a-form>
    <p v-if="blocked && target" class="rf__bad">{{ blocked }}</p>
  </a-modal>
</template>

<style scoped>
.rf__hint {
  margin-top: 4px;
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--text3);
}
.rf__bad {
  margin: 0;
  font-size: var(--fs-xs);
  color: var(--bad);
}
.rf__manual {
  display: flex;
  gap: var(--sp-2);
}
</style>
