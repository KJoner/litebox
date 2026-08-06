<script setup lang="ts">
import { computed } from 'vue'
import { formatRelative, formatTime, formatUTCTime } from '@/utils/format'
import { color } from '@/theme/tokens'

/**
 * 时间有两种语义,不能统一处理:
 *
 *   cycle —— 流量周期边界。一律 UTC 并且页面上必须标注 UTC,
 *            因为后端就是按 UTC 00:00 切的,显示成本地时间会对不上账。
 *   ops   —— 运维时间(最后心跳、部署于)。相对时间 + 本地时区更好读,
 *            绝对值放 title,需要时悬停可见。
 */
const props = withDefaults(
  defineProps<{
    value?: string | null
    mode?: 'ops' | 'cycle' | 'both'
    /** 超过这个秒数就标黄(用于心跳、采样这类新鲜度敏感的时间) */
    warnAfterMs?: number
    dangerAfterMs?: number
    empty?: string
  }>(),
  { mode: 'ops', empty: '—' },
)

const ms = computed(() => {
  if (!props.value) return null
  const t = new Date(props.value).getTime()
  return Number.isNaN(t) ? null : Date.now() - t
})

const tone = computed(() => {
  if (ms.value === null) return color.text3
  if (props.dangerAfterMs && ms.value > props.dangerAfterMs) return color.danger
  if (props.warnAfterMs && ms.value > props.warnAfterMs) return color.warning
  return color.text2
})
</script>

<template>
  <span v-if="!props.value" class="lb-time lb-time--empty">{{ props.empty }}</span>

  <!-- 周期边界:UTC 后缀常驻,不藏进 tooltip。 -->
  <span v-else-if="props.mode === 'cycle'" class="lb-time lb-mono">
    {{ formatUTCTime(props.value) }}
  </span>

  <span v-else-if="props.mode === 'both'" class="lb-time lb-time--stack">
    <span class="lb-mono">{{ formatTime(props.value) }}</span>
    <span class="lb-time__rel lb-mono">{{ formatRelative(props.value) }}</span>
  </span>

  <span v-else class="lb-time lb-mono" :style="{ color: tone }" :title="formatUTCTime(props.value)">
    {{ formatRelative(props.value) }}
  </span>
</template>

<style scoped>
.lb-time {
  font-size: 12px;
}

.lb-time--empty {
  color: #6b7480;
}

.lb-time--stack {
  display: inline-flex;
  flex-direction: column;
  gap: 1px;
}

.lb-time__rel {
  font-size: 10.5px;
  color: #6b7480;
}
</style>
