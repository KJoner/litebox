<script setup lang="ts">
import { computed } from 'vue'
import { formatBytes, formatQuota } from '@/utils/format'

/**
 * 额度条。三条规则来自现有代码,原样保留:
 *
 * 1. 不限量(quota <= 0)不画进度条。没有分母,画出来只能是 0% 或 100%,两种都是错的。
 * 2. 颜色取后端给的 warningLevel,前端不重算阈值 —— 边界(80/95/100)只能有一份定义,
 *    两边各算一次迟早会在临界点上对不齐。
 * 3. usedBytes 读取失败时传 null,显示「—」而不是 0。读不到和真的是零不是一回事。
 *
 * V18:轨道 5px(详情大版本 8px)、填充首次渲染 lb-grow 0.8s。
 */
export type LbWarningLevel = 'UNLIMITED' | 'NORMAL' | 'WARNING' | 'DANGER' | 'EXCEEDED'

const props = withDefaults(
  defineProps<{
    usedBytes: number | null
    quotaBytes: number
    warningLevel?: LbWarningLevel
    /** sm 用于表格行内(5px),md 用于详情卡片(8px) */
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

const level = computed(() => {
  switch (props.warningLevel) {
    case 'WARNING':
      return 'warn'
    case 'DANGER':
    case 'EXCEEDED':
      return 'bad'
    default:
      // 没拿到 warningLevel 时保持中性,不猜一个等级出来。
      return 'normal'
  }
})
</script>

<template>
  <div class="lb-quota" :class="[`lb-quota--${level}`, `lb-quota--${props.size}`]">
    <div v-if="props.showValue" class="lb-quota__value lb-tabular">
      <span v-if="unknown" class="lb-quota__muted">—</span>
      <span v-else class="lb-quota__used">{{ formatBytes(props.usedBytes as number) }}</span>
      <span class="lb-quota__muted"> / {{ formatQuota(props.quotaBytes) }}</span>
    </div>

    <!-- 不限量:留一条静止的浅色槽,占住同样的高度,表格行不因此错位。 -->
    <div v-if="unlimited || unknown" class="lb-quota__track lb-quota__track--empty" />
    <div v-else class="lb-quota__track">
      <div class="lb-quota__fill" :style="{ width: percent + '%' }" />
    </div>

    <div v-if="unknown && props.showValue" class="lb-quota__note">流量读取失败</div>
  </div>
</template>

<style scoped>
.lb-quota {
  display: flex;
  flex-direction: column;
  gap: 5px;
  min-width: 0;
}

.lb-quota__value {
  font-size: 12.5px;
  font-weight: 600;
  color: var(--text);
}
.lb-quota--md .lb-quota__value {
  font-size: 13px;
}
.lb-quota--warn .lb-quota__used {
  color: var(--warn);
}
.lb-quota--bad .lb-quota__used {
  color: var(--bad);
}

.lb-quota__muted {
  color: var(--text3);
  font-weight: 400;
}

.lb-quota__track {
  height: 5px;
  background: var(--track);
  border-radius: var(--r-pill);
  overflow: hidden;
}
.lb-quota--md .lb-quota__track {
  height: 8px;
  border-radius: var(--r-input);
}

.lb-quota__track--empty {
  background: var(--fill);
}

.lb-quota__fill {
  height: 100%;
  border-radius: inherit;
  background: var(--brand);
  transform-origin: left;
  animation: lb-grow 0.8s var(--ease);
}
.lb-quota--warn .lb-quota__fill {
  background: var(--warn);
}
.lb-quota--bad .lb-quota__fill {
  background: var(--bad);
}

.lb-quota__note {
  font-size: 11px;
  color: var(--text3);
}
</style>
