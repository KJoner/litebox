<script setup lang="ts">
import { color } from '@/theme/tokens'

/**
 * 指标卡。四种状态各有各的写法,重点是 error:
 *
 * 现有代码写的是 summary?.traffic_month ?? 0 —— 接口失败时页面稳稳显示
 * 「本月流量 0 B」,读不到和真的是零长得一模一样。这里 error 时显示「—」,
 * 绝不回退成 0。
 */
withDefaults(
  defineProps<{
    label: string
    value?: string | number | null
    unit?: string
    /** 分母,例如 8 / 10 */
    total?: string | number | null
    state?: 'ready' | 'loading' | 'empty' | 'error'
    tone?: 'default' | 'warning' | 'danger'
    /** 底部一行说明,或用 #foot 插槽放标签 */
    hint?: string
    emptyHint?: string
  }>(),
  { state: 'ready', tone: 'default', emptyHint: '尚无数据' },
)

const toneColor = { default: undefined, warning: color.warning, danger: color.danger }
</script>

<template>
  <div class="lb-metric">
    <div class="lb-metric__label">{{ label }}</div>

    <template v-if="state === 'loading'">
      <div class="lb-metric__skel lb-metric__skel--value" />
      <div class="lb-metric__skel lb-metric__skel--hint" />
    </template>

    <template v-else-if="state === 'error'">
      <div class="lb-metric__row">
        <svg width="12" height="12" viewBox="0 0 9 9" aria-hidden="true">
          <path d="M1.2 1.2 7.8 7.8M7.8 1.2 1.2 7.8" :stroke="color.danger" stroke-width="1.8" stroke-linecap="round" />
        </svg>
        <span class="lb-metric__failed">加载失败</span>
      </div>
      <div class="lb-metric__hint">读取失败不会把数值写成 0,显示的仍是未知</div>
    </template>

    <template v-else-if="state === 'empty'">
      <!-- 30px 的占位「—」用四级灰:该尺寸下 3.1:1 达到 AA Large。 -->
      <div class="lb-metric__row">
        <span class="lb-metric__value lb-mono" :style="{ color: color.text4 }">—</span>
      </div>
      <div class="lb-metric__hint">{{ emptyHint }}</div>
    </template>

    <template v-else>
      <div class="lb-metric__row">
        <span class="lb-metric__value lb-mono" :style="{ color: toneColor[tone] }">{{ value }}</span>
        <span v-if="unit" class="lb-metric__unit">{{ unit }}</span>
        <span v-if="total !== undefined && total !== null" class="lb-metric__total">/ {{ total }}</span>
        <slot name="action" />
      </div>
      <div v-if="hint || $slots.foot" class="lb-metric__foot">
        <slot name="foot">{{ hint }}</slot>
      </div>
    </template>
  </div>
</template>

<style scoped>
.lb-metric {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 14px 16px;
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

.lb-metric__label {
  font-size: 12.5px;
  color: #576070;
}

.lb-metric__row {
  display: flex;
  align-items: baseline;
  gap: 8px;
}

.lb-metric__value {
  font-size: 30px;
  font-weight: 600;
  letter-spacing: -0.02em;
  line-height: 1;
}

.lb-metric__unit {
  font-size: 14px;
  color: #576070;
}

.lb-metric__total,
.lb-metric__hint,
.lb-metric__foot {
  font-size: 11.5px;
  color: #6b7480;
}

.lb-metric__total {
  font-size: 13px;
}

.lb-metric__foot {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.lb-metric__failed {
  font-size: 13px;
  font-weight: 500;
  color: #b4291d;
}

.lb-metric__skel {
  background: #edeff2;
  border-radius: 4px;
}

.lb-metric__skel--value {
  width: 104px;
  height: 26px;
}

.lb-metric__skel--hint {
  width: 140px;
  height: 9px;
  background: #f1f3f5;
}
</style>
