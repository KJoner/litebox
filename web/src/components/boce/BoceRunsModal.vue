<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { api, ApiError, type BoceRun, type BoceTaskView, type BoceVantage, type ProbeStatus } from '@/api/client'
import { LbIcon, LbStatusTag } from '@/components/lb'
import { formatTime } from '@/utils/format'
import { rttText } from '@/components/asset/probeView'
import { runStateMeta, vantageLabel } from './boceText'

/**
 * 一个任务的执行记录(每一轮 = 一次 Boce 任务):状态、估算消耗、每个拨测点的原始结果。
 * 任务定义与执行记录分开:改任务、暂停任务都不删这里的东西。只读库,不调 Boce。
 */
const props = defineProps<{ open: boolean; task: BoceTaskView | null; vantages: BoceVantage[] }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const runs = ref<BoceRun[]>([])
const loading = ref(false)
const error = ref('')

const vname = computed(() => {
  const m = new Map<number, string>()
  for (const v of props.vantages) m.set(v.id, vantageLabel(v))
  return m
})

async function load() {
  if (!props.task) return
  loading.value = true
  error.value = ''
  try {
    runs.value = (await api.boceRuns(props.task.id, 50)).runs
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '读取执行记录失败'
  } finally {
    loading.value = false
  }
}

watch(
  () => [props.open, props.task?.id],
  () => {
    if (props.open) void load()
  },
  { immediate: true },
)

function tone(s: ProbeStatus): string {
  switch (s) {
    case 'OK':
      return 'brr__ok'
    case 'LOSS':
      return 'brr__warn'
    case 'TIMEOUT':
      return 'brr__bad'
  }
  return 'brr__muted'
}
</script>

<template>
  <a-modal
    :open="open"
    :title="task ? `执行记录 · ${task.name}` : '执行记录'"
    :width="820"
    :footer="null"
    @cancel="emit('close')"
  >
    <div class="brr" data-testid="boce-runs">
      <div class="brr__bar">
        <span class="brr__muted">每一轮是一次 Boce 任务;「估算」是面板按单价算的,实际扣费以 Boce 账单为准。保留 90 天。</span>
        <a-button size="small" :loading="loading" @click="load">
          <template #icon><LbIcon name="refresh" :size="13" /></template>
          刷新
        </a-button>
      </div>
      <div v-if="error" class="lb-error-strip">{{ error }}</div>
      <a-skeleton v-else-if="loading && !runs.length" active :paragraph="{ rows: 4 }" />
      <div v-else-if="!runs.length" class="brr__muted brr__empty">还没有执行过</div>
      <ol v-else class="brr__list">
        <li v-for="r in runs" :key="r.id" class="brr__run">
          <div class="brr__run-head lb-tabular">
            <span>{{ formatTime(r.created_at) }}</span>
            <span class="lb-chip">{{ r.trigger === 'MANUAL' ? '手动' : `定时 ${r.slot ? formatTime(r.slot).slice(11, 16) : ''}` }}</span>
            <LbStatusTag :meta="runStateMeta(r.state)" />
            <span class="brr__muted">估算 {{ r.points }} 波点</span>
            <span v-if="r.host" class="brr__muted lb-mono">{{ r.host }} · 口径 {{ r.generation }}</span>
            <span v-if="r.vendor_task_id" class="brr__muted lb-mono" title="Boce 任务 ID">#{{ r.vendor_task_id }}</span>
          </div>
          <div v-if="r.error" class="brr__err">{{ r.error }}</div>
          <div v-if="r.items.length" class="brr__items">
            <div v-for="it in r.items" :key="it.vantage_id" class="brr__item lb-tabular">
              <span class="brr__name lb-ellipsis">{{ vname.get(it.vantage_id) ?? it.node_name ?? `#${it.vantage_id}` }}</span>
              <span :class="tone(it.status)">{{ it.label }}</span>
              <span>{{ it.status === 'OK' || it.status === 'LOSS' ? rttText(it.rtt_avg_ms) : '' }}</span>
              <span class="brr__muted">
                <template v-if="it.sent">{{ it.received }}/{{ it.sent }}</template>
                <template v-if="it.ip"> · {{ it.ip }}</template>
                <template v-if="!it.charged"> · 未计费</template>
                <template v-if="it.error"> · {{ it.error }}</template>
              </span>
            </div>
          </div>
        </li>
      </ol>
    </div>
  </a-modal>
</template>

<style scoped>
.brr {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.brr__bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
}
.brr__list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  max-height: 520px;
  overflow: auto;
}
.brr__run {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: var(--sp-2) var(--sp-3);
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
}
.brr__run-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
  font-size: var(--fs-sm);
}
.brr__items {
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: var(--fs-xs);
}
.brr__item {
  display: grid;
  grid-template-columns: minmax(0, 140px) 120px 70px minmax(0, 1fr);
  gap: var(--sp-2);
}
.brr__name {
  font-weight: 600;
}
.brr__err {
  font-size: var(--fs-xs);
  color: var(--warn);
}
.brr__ok {
  color: var(--ok);
}
.brr__warn {
  color: var(--warn);
}
.brr__bad {
  color: var(--bad);
}
.brr__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.brr__empty {
  padding: var(--sp-3) 0;
}
@media (max-width: 767px) {
  .brr__item {
    grid-template-columns: minmax(0, 1fr) auto;
  }
}
</style>
