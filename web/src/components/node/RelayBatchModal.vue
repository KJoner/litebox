<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type RelayBatchPlan } from '@/api/client'

/**
 * 一个中转配多个落地(复核批次三)。
 *
 * 选好几个落地入口,每个一行(名字、监听端口可以改),先「预览」看每一条会生成什么、哪一条有什么问题;
 * 「提交」是整批的:任何一条通不过就一条都不写 —— 不会出现写进去一半、界面却说整批成功。
 * 写入之后这台中转机按引擎下发一次(nginx 只 reload,在途连接不断;realm 要重启)。
 */
const props = defineProps<{
  open: boolean
  nodeId: number
  nodeLabel: string
  landings: { value: number; label: string }[]
}>()
const emit = defineEmits<{ 'update:open': [boolean]; changed: [] }>()

interface Row {
  inbound: number
  label: string
  name: string
  port: number
}
const engine = ref<'NGINX' | 'REALM'>('NGINX')
const startPort = ref(40000)
const picked = ref<number[]>([])
const rows = ref<Row[]>([])
const plan = ref<RelayBatchPlan | null>(null)
const busy = ref(false)

watch(
  () => props.open,
  (o) => {
    if (!o) return
    picked.value = []
    rows.value = []
    plan.value = null
  },
)

// 选中的落地变了:保留已经改过的行,新加的按起始端口往后排。
watch(picked, (ids) => {
  const keep = new Map(rows.value.map((r) => [r.inbound, r]))
  const used = new Set(rows.value.filter((r) => ids.includes(r.inbound)).map((r) => r.port))
  let next = startPort.value
  rows.value = ids.map((id) => {
    const old = keep.get(id)
    if (old) return old
    while (used.has(next)) next++
    used.add(next)
    const label = props.landings.find((l) => l.value === id)?.label ?? String(id)
    return { inbound: id, label, name: `${props.nodeLabel} → ${label}`.slice(0, 64), port: next++ }
  })
  plan.value = null
})

const items = computed(() =>
  rows.value.map((r) => ({
    engine: engine.value,
    display_name: r.name,
    listen_port: r.port,
    target_kind: 'INBOUND',
    target_inbound_id: r.inbound,
  })),
)

function problemOf(i: number) {
  return plan.value?.items.find((it) => it.index === i)?.problem ?? ''
}

async function preview() {
  busy.value = true
  try {
    plan.value = await api.relayBatch(props.nodeId, { dry_run: true, items: items.value })
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '预览失败')
  } finally {
    busy.value = false
  }
}

async function submit() {
  busy.value = true
  try {
    const res = await api.relayBatch(props.nodeId, { items: items.value })
    message.success(
      `已新增 ${res.created?.length ?? 0} 条转发,这台机器会自动下发(${engine.value === 'REALM' ? 'realm 要重启,在途连接会断' : 'nginx 只 reload,在途连接不断'})`,
    )
    emit('changed')
    emit('update:open', false)
  } catch (e) {
    if (e instanceof ApiError && e.status === 409 && e.payload && typeof e.payload === 'object') {
      plan.value = e.payload as RelayBatchPlan
      message.warning('有通不过的条目,整批一条都没有写入,见下面的标注')
    } else {
      message.error(e instanceof ApiError ? e.message : '提交失败')
    }
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <a-modal
    :open="open"
    :title="`一个中转配多个落地 · ${nodeLabel}`"
    width="720px"
    :confirm-loading="busy"
    ok-text="提交整批"
    cancel-text="取消"
    :ok-button-props="{ disabled: !rows.length || !plan?.ok }"
    @ok="submit"
    @cancel="emit('update:open', false)"
  >
    <div class="rb">
      <div class="rb__row">
        <span class="rb__label">引擎</span>
        <a-radio-group v-model:value="engine" size="small" @change="plan = null">
          <a-radio-button value="NGINX">nginx(只 reload)</a-radio-button>
          <a-radio-button value="REALM">realm(要重启)</a-radio-button>
        </a-radio-group>
        <span class="rb__label">起始端口</span>
        <a-input-number v-model:value="startPort" :min="1" :max="65535" size="small" />
      </div>
      <a-select
        v-model:value="picked"
        mode="multiple"
        :options="landings"
        placeholder="选择要转发到的落地入口(可多选)"
        option-filter-prop="label"
        style="width: 100%"
      />
      <div v-if="!landings.length" class="rb__hint">没有可选的落地入口:先在落地机器上建好入口。</div>
      <table v-if="rows.length" class="rb__table">
        <thead>
          <tr><th>落地</th><th>名称</th><th>监听端口</th><th></th></tr>
        </thead>
        <tbody>
          <tr v-for="(r, i) in rows" :key="r.inbound" :class="{ 'rb__bad-row': problemOf(i) }">
            <td class="rb__muted">{{ r.label }}</td>
            <td><a-input v-model:value="r.name" size="small" :maxlength="64" @change="plan = null" /></td>
            <td><a-input-number v-model:value="r.port" :min="1" :max="65535" size="small" @change="plan = null" /></td>
            <td class="rb__bad">{{ problemOf(i) }}</td>
          </tr>
        </tbody>
      </table>
      <div class="rb__foot">
        <a-button size="small" :disabled="!rows.length" :loading="busy" @click="preview">预览</a-button>
        <span v-if="plan?.ok" class="rb__ok">预览通过:{{ plan.items.length }} 条都能写入,提交后整批生效</span>
        <span v-else-if="plan" class="rb__bad">有通不过的条目:改好再预览;提交是整批的,不会只写进去一部分</span>
        <span v-else class="rb__hint">先预览;提交按钮在预览通过之后才能点</span>
      </div>
    </div>
  </a-modal>
</template>

<style scoped>
.rb {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.rb__row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  flex-wrap: wrap;
}
.rb__label {
  font-size: var(--fs-sm);
  color: var(--text2);
}
.rb__table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--fs-sm);
}
.rb__table th,
.rb__table td {
  padding: var(--sp-1) var(--sp-2);
  border-bottom: 1px solid var(--border);
  text-align: left;
  vertical-align: middle;
}
.rb__muted,
.rb__hint {
  color: var(--text2);
  font-size: var(--fs-xs);
}
.rb__bad {
  color: var(--bad);
  font-size: var(--fs-xs);
}
.rb__ok {
  color: var(--ok);
  font-size: var(--fs-xs);
}
.rb__bad-row td {
  background: var(--bad-bg);
}
.rb__foot {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
}
</style>
