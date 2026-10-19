<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type BoceEstimate,
  type BoceScheduleSpec,
  type BoceTaskInput,
  type BoceTaskView,
  type BoceVantage,
  type Node,
  type ProbeCandidate,
} from '@/api/client'
import { LbInfoTip } from '@/components/lb'
import BoceVantagePicker from './BoceVantagePicker.vue'
import BoceScheduleEditor from './BoceScheduleEditor.vue'
import BoceEstimatePanel from './BoceEstimatePanel.vue'
import { emptyTaskInput, specOf, targetText, TEST_TYPES } from './boceText'

/**
 * 把同一份 Boce 任务模板批量应用到几台节点:每台各建一个独立任务,之后可以在节点详情里单独调整。
 * 每台的被测入口默认取它第一个能交给 Boce 的 IPv4 订阅入口,可以逐台改。建好的任务默认不开自动检测。
 */
const props = defineProps<{ open: boolean; nodes: Node[] }>()
const emit = defineEmits<{ (e: 'close'): void; (e: 'saved'): void }>()

const vantages = ref<BoceVantage[]>([])
const tasks = ref<BoceTaskView[]>([])
const cands = ref<Record<number, ProbeCandidate[]>>({})
const refs = ref<Record<number, string | undefined>>({})
const pointsPerCheck = ref(1)
const form = reactive<BoceTaskInput>(emptyTaskInput({
  schedule_mode: 'INTERVAL', interval_min: 60, times: [], window_start: '08:00', window_end: '24:00', weekdays: 127,
  timezone: 'Asia/Shanghai',
}))
const templateID = ref<number | 0>(0)
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const results = ref<{ node_id: number; task_id: number; error: string }[] | null>(null)

const spec = computed<BoceScheduleSpec>({
  get: () => specOf(form),
  set: (v) => Object.assign(form, v),
})

watch(
  () => props.open,
  async (open) => {
    if (!open) return
    error.value = ''
    results.value = null
    templateID.value = 0
    loading.value = true
    try {
      const [v, t, ov] = await Promise.all([api.boceVantages(), api.boceTasks(), api.boceOverview()])
      vantages.value = v.vantages
      tasks.value = t.tasks
      pointsPerCheck.value = ov.settings.points_per_check
      Object.assign(form, emptyTaskInput(ov.settings.default_schedule))
      const entries = await Promise.all(
        props.nodes.map(async (n) => {
          try {
            return [n.id, (await api.nodeBoce(n.id)).candidates] as const
          } catch {
            return [n.id, [] as ProbeCandidate[]] as const
          }
        }),
      )
      cands.value = Object.fromEntries(entries)
      refs.value = Object.fromEntries(entries.map(([id, list]) => [id, list.find((c) => !c.note)?.ref]))
    } catch (err) {
      error.value = err instanceof ApiError ? err.message : '读取失败'
    } finally {
      loading.value = false
    }
  },
)

function applyTemplate(id: unknown) {
  templateID.value = Number(id) || 0
  const t = tasks.value.find((x) => x.id === templateID.value)
  if (!t) return
  Object.assign(form, {
    ...specOf(t),
    test_type: t.test_type,
    vantage_ids: [...t.vantage_ids],
    month_points_limit: t.month_points_limit,
  })
}

const ready = computed(() => props.nodes.filter((n) => refs.value[n.id]))

const estimate = ref<BoceEstimate | null>(null)
const estimateError = ref('')
let timer: ReturnType<typeof setTimeout> | null = null
watch(
  () => [props.open, JSON.stringify(spec.value), form.vantage_ids.length, ready.value.length] as const,
  () => {
    if (!props.open) return
    if (timer) clearTimeout(timer)
    timer = setTimeout(async () => {
      try {
        estimate.value = await api.boceEstimate({ ...spec.value, vantages: form.vantage_ids.length, targets: Math.max(ready.value.length, 1) })
        estimateError.value = ''
      } catch (err) {
        estimateError.value = err instanceof ApiError ? err.message : '估算失败'
      }
    }, 350)
  },
)

async function submit() {
  error.value = ''
  if (!form.vantage_ids.length) {
    error.value = '至少选一个 Boce 拨测点'
    return
  }
  if (!ready.value.length) {
    error.value = '选中的节点里没有能交给 Boce 测的 IPv4 订阅入口'
    return
  }
  saving.value = true
  try {
    const r = await api.boceBatch({
      template: { ...form, times: [...form.times], ref: undefined },
      nodes: ready.value.map((n) => ({ node_id: n.id, ref: refs.value[n.id]! })),
    })
    results.value = r.results
    const ok = r.results.filter((x) => !x.error).length
    if (ok) message.success(`已为 ${ok} 台节点各建一个任务${form.enabled ? '并开启自动检测' : '(自动检测还没开)'}`)
    if (ok === r.results.length) emit('saved')
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '批量应用失败'
  } finally {
    saving.value = false
  }
}

function nodeName(id: number): string {
  const n = props.nodes.find((x) => x.id === id)
  return n?.display_name || n?.name || `#${id}`
}
</script>

<template>
  <a-modal
    :open="open"
    title="批量应用 Boce 拨测任务"
    :width="820"
    :mask-closable="false"
    :confirm-loading="saving"
    ok-text="应用"
    cancel-text="关闭"
    @cancel="emit('close')"
    @ok="submit"
  >
    <div class="bbm" data-testid="boce-batch">
      <div class="bbm__muted">
        给选中的 {{ nodes.length }} 台节点各建一个独立任务(之后可以单独调整)。保存任务不会创建付费检测;新任务默认不开自动检测。
      </div>
      <div v-if="error" class="lb-error-strip">{{ error }}</div>
      <a-skeleton v-if="loading" active :paragraph="{ rows: 3 }" />
      <template v-else>
        <div class="lb-group">
          <div class="lb-group__row bbm__row">
            <span class="lb-group__label">从已有任务复制</span>
            <a-select :value="templateID || undefined" allow-clear placeholder="不复制,用默认计划" aria-label="模板" @update:value="applyTemplate">
              <a-select-option v-for="t in tasks" :key="t.id" :value="t.id">{{ t.node_name }} · {{ t.name }}</a-select-option>
            </a-select>
          </div>
          <div class="lb-group__row bbm__row">
            <span class="lb-group__label">任务名称</span>
            <a-input v-model:value="form.name" :maxlength="64" placeholder="留空 = 用各自的入口名" aria-label="任务名称" />
          </div>
          <div class="lb-group__row bbm__row">
            <span class="lb-group__label">检测类型</span>
            <a-radio-group v-model:value="form.test_type" option-type="button" button-style="solid" size="small" aria-label="检测类型">
              <a-radio-button v-for="t in TEST_TYPES" :key="t.value" :value="t.value" :title="t.hint">{{ t.label }}</a-radio-button>
            </a-radio-group>
          </div>
        </div>
        <div class="bbm__sec">
          <div class="bbm__title">拨测点</div>
          <BoceVantagePicker v-model="form.vantage_ids" :vantages="vantages" :points-per-check="pointsPerCheck" />
        </div>
        <div class="bbm__sec">
          <div class="bbm__title">时间表</div>
          <BoceScheduleEditor v-model="spec" />
        </div>
        <div class="lb-group">
          <div class="lb-group__row bbm__row">
            <span class="lb-group__label">单任务每月上限</span>
            <a-input-number v-model:value="form.month_points_limit" :min="0" :max="100000000" :precision="0" addon-after="波点 / 月" />
          </div>
          <div class="lb-group__row bbm__row bbm__row--switch">
            <span class="lb-group__label">建好后开启自动检测</span>
            <a-switch v-model:checked="form.enabled" aria-label="建好后开启自动检测" />
          </div>
        </div>
        <div class="bbm__sec">
          <div class="bbm__title">
            各节点的被测入口
            <LbInfoTip :width="300" text="只能选这台节点的订阅入口;IPv6 入口列出来但选不了(Boce 的 TCPing / Ping 都没有说明支持 IPv6)。没有可选入口的节点会被跳过。" />
          </div>
          <div v-for="n in nodes" :key="n.id" class="bbm__node">
            <span class="bbm__node-name lb-ellipsis">{{ n.display_name || n.name }}</span>
            <a-select
              v-model:value="refs[n.id]"
              class="bbm__node-sel"
              :placeholder="(cands[n.id] ?? []).some((c) => !c.note) ? '选择入口' : '没有可选的 IPv4 订阅入口(跳过)'"
              :aria-label="`${n.display_name || n.name} 的被测入口`"
            >
              <a-select-option v-for="c in cands[n.id] ?? []" :key="c.ref" :value="c.ref" :disabled="!!c.note" :title="c.note">
                {{ c.label }} · {{ targetText(form.test_type, c.host, c.port) }}
              </a-select-option>
            </a-select>
            <span v-if="results" class="bbm__res">
              <template v-for="r in results.filter((x) => x.node_id === n.id)" :key="r.node_id">
                <span v-if="r.error" class="bbm__bad">{{ r.error }}</span>
                <span v-else class="bbm__ok">已建任务 #{{ r.task_id }}</span>
              </template>
            </span>
          </div>
        </div>
        <BoceEstimatePanel :estimate="estimate" :error="estimateError" :targets="Math.max(ready.length, 1)" />
        <div v-if="results" class="bbm__muted">
          共 {{ results.length }} 台:成功 {{ results.filter((r) => !r.error).length }},失败
          {{ results.filter((r) => r.error).length }}<template v-for="r in results.filter((x) => x.error)" :key="`e${r.node_id}`">;{{ nodeName(r.node_id) }}</template>
        </div>
      </template>
    </div>
  </a-modal>
</template>

<style scoped>
.bbm {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.bbm__row {
  grid-template-columns: 140px minmax(0, 1fr);
}
.bbm__row--switch {
  grid-template-columns: minmax(0, 1fr) auto;
}
.bbm__sec {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.bbm__title {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--text2);
}
.bbm__node {
  display: grid;
  grid-template-columns: minmax(0, 160px) minmax(0, 1fr) minmax(0, 180px);
  align-items: center;
  gap: var(--sp-2);
}
.bbm__node-name {
  font-weight: 600;
}
.bbm__node-sel {
  width: 100%;
}
.bbm__res {
  font-size: var(--fs-xs);
}
.bbm__ok {
  color: var(--ok);
}
.bbm__bad {
  color: var(--bad);
}
.bbm__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
}
@media (max-width: 767px) {
  .bbm__node {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
