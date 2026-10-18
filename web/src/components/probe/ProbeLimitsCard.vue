<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type ProbeLimitRange, type ProbeLimits, type OutboundRoundStatus } from '@/api/client'
import { LbInfoTip } from '@/components/lb'

/**
 * 出站探测的调度限额(全站):第三方目标每分钟的实际探测尝试上限、单台节点最多几个第三方目标、
 * 同时执行的机器数、单台节点单次执行的目标上限。超出的延后执行,不记任何结果、不算失败或丢包。
 */
const props = defineProps<{
  limits: ProbeLimits
  ranges: ProbeLimitRange[]
  lastRun: OutboundRoundStatus | null
}>()
const emit = defineEmits<{ (e: 'saved'): void }>()

const keyOf: Record<string, keyof ProbeLimits> = {
  probe_third_party_budget_per_min: 'third_party_budget_per_min',
  probe_third_party_per_node: 'third_party_per_node',
  probe_outbound_concurrency: 'node_concurrency',
  probe_outbound_round_max: 'round_max',
}
const tips: Record<string, string> = {
  probe_third_party_budget_per_min:
    '全部节点合计,每分钟对第三方来源(Zstatic、TcpQuality 等)的实际探测尝试:ICMP 按包、TCP 握手近似按次、TCP 建连按次。按计划先占、跑完按实际结算;节点正忙、采集失败、DNS 没解析出来都不算。',
  probe_third_party_per_node: '一台机器最多对多少个第三方目标启用。超出时启用操作直接拒绝(返回原因),不会悄悄少测几个。',
  probe_outbound_concurrency: '同时经 SSH 执行出站探测的机器数。每台机器一次执行跑完它这一拍到期的目标。',
  probe_outbound_round_max: '一台机器一次执行最多带几个目标(占 SSH 锁的时间与之成正比),多出来的下一拍(15 秒后)再测。',
}

const form = reactive<Record<string, number | null>>({})
const saving = ref(false)
const error = ref('')

watch(
  () => props.limits,
  (l) => {
    for (const r of props.ranges) form[r.key] = l[keyOf[r.key]]
  },
  { immediate: true },
)

async function save() {
  saving.value = true
  error.value = ''
  const values: Record<string, number> = {}
  for (const r of props.ranges) {
    const v = form[r.key]
    values[r.key] = v == null || v === r.default ? 0 : v
  }
  try {
    await api.updateProbeLimits(values)
    message.success('限额已保存,下一拍起生效')
    emit('saved')
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <section class="lb-card plc" data-testid="probe-limits">
    <div class="plc__head">
      <h3 class="plc__title">调度与限额</h3>
      <span v-if="lastRun?.last_run_at" class="plc__muted lb-tabular">
        最近一分钟第三方探测 {{ lastRun.third_party_used }} / {{ lastRun.third_party_budget }} 次
        <template v-if="lastRun.deferred"> · 上一拍延后 {{ lastRun.deferred }} 条(不记结果、不算失败)</template>
      </span>
    </div>
    <div v-if="error" class="lb-error-strip">{{ error }}</div>
    <div class="plc__grid">
      <label v-for="r in ranges" :key="r.key" class="plc__item">
        <span class="plc__label">
          {{ r.label }}
          <LbInfoTip :width="300" :text="`${tips[r.key] ?? ''} 范围 ${r.min}~${r.max},默认 ${r.default}。`" />
        </span>
        <a-input-number v-model:value="form[r.key]" :min="r.min" :max="r.max" :precision="0" size="small" class="plc__num" />
      </label>
    </div>
    <div class="plc__foot">
      <span class="plc__muted">第三方目标还受各自来源的最小间隔(默认 300 秒)与单目标限流约束;各机器的首次探测在 5 分钟内错峰,之后每轮带 ±5% 的抖动。</span>
      <a-button size="small" type="primary" :loading="saving" @click="save">保存限额</a-button>
    </div>
  </section>
</template>

<style scoped>
.plc {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.plc__head {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  justify-content: space-between;
  gap: var(--sp-2);
}
.plc__title {
  margin: 0;
  font-size: var(--fs-md);
  font-weight: 600;
}
.plc__grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
  gap: var(--sp-3);
}
.plc__item {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.plc__label {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-xs);
  color: var(--text2);
}
.plc__num {
  width: 140px;
}
.plc__foot {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
}
.plc__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
}
</style>
