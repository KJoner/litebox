<script setup lang="ts">
import { computed } from 'vue'
import { threshold } from '@/theme/tokens'

/**
 * 资源使用率细条(CPU / 内存 / 磁盘),Beszel 式:一条 4px 的条 + 右侧百分比。
 *
 * 颜色按 threshold.usageWarn / usageDanger 三档(与节点详情的资源卡同一份阈值)。
 * 读不到(从未采样、采集失败)传 null:显示「—」与一条空槽,**不画成 0%** ——
 * 0% 的 CPU 与「不知道」在条上一个贴底一个空着,看反了就是把一台没数据的机器当成闲着。
 * stale 为真时整条降成中性色:数字仍给出来,但颜色不再暗示「此刻」的健康程度。
 */
const props = withDefaults(
  defineProps<{
    percent: number | null | undefined
    /** 悬停说明,例如「312 MB / 1.0 GB」。 */
    detail?: string
    /** 采样过期 / 变更前数据:降成中性色。 */
    stale?: boolean
    /** 条的最小宽度,表格里给 56,卡片里给更宽。 */
    minWidth?: number
  }>(),
  { stale: false, minWidth: 56 },
)

const value = computed(() =>
  props.percent === null || props.percent === undefined || Number.isNaN(props.percent)
    ? null
    : Math.max(0, Math.min(100, props.percent)),
)

const tone = computed(() => {
  if (value.value === null || props.stale) return 'muted'
  if (value.value >= threshold.usageDanger) return 'bad'
  if (value.value >= threshold.usageWarn) return 'warn'
  return 'ok'
})

const text = computed(() => {
  if (value.value === null) return '—'
  return value.value < 10 && value.value > 0 ? `${value.value.toFixed(1)}%` : `${Math.round(value.value)}%`
})
</script>

<template>
  <div class="lb-usage" :class="`lb-usage--${tone}`" :title="detail">
    <div class="lb-usage__track" :style="{ minWidth: `${props.minWidth}px` }">
      <div v-if="value !== null" class="lb-usage__fill" :style="{ width: `${value}%` }" />
    </div>
    <span class="lb-usage__value lb-tabular">{{ text }}</span>
  </div>
</template>

<style scoped>
.lb-usage {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  min-width: 0;
}

.lb-usage__track {
  flex: 1;
  height: 4px;
  border-radius: var(--r-pill);
  background: var(--track);
  overflow: hidden;
}

.lb-usage__fill {
  height: 100%;
  border-radius: inherit;
  background: var(--ok);
  transform-origin: left;
  animation: lb-grow 0.6s var(--ease);
}
.lb-usage--warn .lb-usage__fill {
  background: var(--warn);
}
.lb-usage--bad .lb-usage__fill {
  background: var(--bad);
}
.lb-usage--muted .lb-usage__fill {
  background: var(--text3);
  opacity: 0.6;
}

.lb-usage__value {
  flex: none;
  min-width: 38px;
  text-align: right;
  font-size: var(--fs-xs);
  font-weight: 500;
  color: var(--text2);
}
.lb-usage--warn .lb-usage__value {
  color: var(--warn);
}
.lb-usage--bad .lb-usage__value {
  color: var(--bad);
}
.lb-usage--muted .lb-usage__value {
  color: var(--text3);
}
</style>
