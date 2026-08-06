<script setup lang="ts">
import { computed } from 'vue'
import { formatBytes, formatQuota } from '@/utils/format'
import { color } from '@/theme/tokens'

/**
 * 额度条。三条规则来自现有代码,原样保留:
 *
 * 1. 不限量(quota <= 0)不画进度条。没有分母,画出来只能是 0% 或 100%,两种都是错的。
 * 2. 颜色取后端给的 warningLevel,前端不重算阈值 —— 边界(80/95/100)只能有一份定义,
 *    两边各算一次迟早会在临界点上对不齐。
 * 3. usedBytes 读取失败时传 null,显示「—」而不是 0。读不到和真的是零不是一回事。
 */
export type LbWarningLevel = 'UNLIMITED' | 'NORMAL' | 'WARNING' | 'DANGER' | 'EXCEEDED'

const props = withDefaults(
  defineProps<{
    usedBytes: number | null
    quotaBytes: number
    warningLevel?: LbWarningLevel
    /** sm 用于表格行内(3px),md 用于卡片(6px) */
    size?: 'sm' | 'md'
    /** 显示「已用 / 总量」文字行 */
    showValue?: boolean
  }>(),
  { size: 'sm', showValue: true },
)

const unlimited = computed(() => props.quotaBytes <= 0)
const unknown = computed(() => props.usedBytes === null)

const percent = computed(() => {
  if (unlimited.value || unknown.value) return null
  return Math.min(100, Math.round(((props.usedBytes as number) / props.quotaBytes) * 100))
})

const barColor = computed(() => {
  if (props.warningLevel) {
    return {
      UNLIMITED: color.text3,
      NORMAL: color.brand,
      WARNING: color.warning,
      DANGER: color.danger,
      EXCEEDED: color.danger,
    }[props.warningLevel]
  }
  // 没拿到 warningLevel 时保持中性,不猜一个等级出来。
  return color.brand
})

const valueColor = computed(() =>
  props.warningLevel === 'DANGER' || props.warningLevel === 'EXCEEDED'
    ? color.danger
    : props.warningLevel === 'WARNING'
      ? color.warning
      : color.text1,
)

const height = computed(() => (props.size === 'md' ? 6 : 3))
</script>

<template>
  <div class="lb-quota">
    <div v-if="props.showValue" class="lb-quota__value lb-mono">
      <span v-if="unknown" :style="{ color: color.text3 }">—</span>
      <span v-else :style="{ color: valueColor }">{{ formatBytes(props.usedBytes as number) }}</span>
      <span :style="{ color: color.text3 }"> / {{ formatQuota(props.quotaBytes) }}</span>
    </div>

    <!-- 不限量:留一条静止的浅色槽,占住同样的高度,表格行不因此错位。 -->
    <div
      v-if="unlimited || unknown"
      class="lb-quota__track lb-quota__track--empty"
      :style="{ height: height + 'px' }"
    />
    <div v-else class="lb-quota__track" :style="{ height: height + 'px' }">
      <div
        class="lb-quota__fill"
        :style="{ width: percent + '%', height: height + 'px', background: barColor }"
      />
    </div>

    <div v-if="unknown && props.showValue" class="lb-quota__note">流量读取失败</div>
  </div>
</template>

<style scoped>
.lb-quota {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}

.lb-quota__value {
  font-size: 12px;
}

.lb-quota__track {
  background: #edeff2;
  border-radius: 2px;
  overflow: hidden;
}

.lb-quota__track--empty {
  background: #f1f3f5;
}

.lb-quota__fill {
  border-radius: 2px;
}

.lb-quota__note {
  font-size: 10.5px;
  color: #6b7480;
}
</style>
