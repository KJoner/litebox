<script setup lang="ts">
import LbIcon, { type LbIconName } from './LbIcon.vue'
import LbInfoTip from './LbInfoTip.vue'

/**
 * 指标卡(UI 2.0)。每格是一张独立卡片,父容器用 `.lb-metrics`(base.css)排成网格。
 *
 * 四种状态各有各的写法,重点是 error:曾经的写法是 summary?.traffic_month ?? 0 ——
 * 接口失败时页面稳稳显示「本月流量 0 B」,读不到和真的是零长得一模一样。
 * 这里 error 时显示「—」,绝不回退成 0。
 */
withDefaults(
  defineProps<{
    label: string
    /** 标签旁的 ⓘ 文案。 */
    tip?: string
    /** 右上角的图标。只是帮助扫读的记号,不承载状态。 */
    icon?: LbIconName
    value?: string | number | null
    unit?: string
    /** 分母,例如 8 / 10 */
    total?: string | number | null
    state?: 'ready' | 'loading' | 'empty' | 'error'
    tone?: 'default' | 'warning' | 'danger' | 'success'
    /** 底部一行说明,或用 #foot 插槽放标签 / 「查看 ›」链接 */
    hint?: string
    emptyHint?: string
  }>(),
  { state: 'ready', tone: 'default', emptyHint: '尚无数据' },
)
</script>

<template>
  <div class="lb-metric" :class="`lb-metric--${tone}`">
    <div class="lb-metric__label">
      <span class="lb-metric__label-text">{{ label }}</span>
      <LbInfoTip v-if="tip" :text="tip" />
      <slot name="tip" />
      <span v-if="icon" class="lb-metric__icon"><LbIcon :name="icon" :size="16" /></span>
    </div>

    <template v-if="state === 'loading'">
      <div class="lb-metric__skel lb-metric__skel--value" />
      <div class="lb-metric__skel lb-metric__skel--hint" />
    </template>

    <template v-else-if="state === 'error'">
      <div class="lb-metric__row">
        <span class="lb-metric__value lb-metric__value--muted">—</span>
        <span class="lb-metric__failed">加载失败</span>
      </div>
      <div class="lb-metric__foot">读取失败不会把数值写成 0,显示的仍是未知</div>
    </template>

    <template v-else-if="state === 'empty'">
      <div class="lb-metric__row">
        <span class="lb-metric__value lb-metric__value--muted">—</span>
      </div>
      <div class="lb-metric__foot">{{ emptyHint }}</div>
    </template>

    <template v-else>
      <div class="lb-metric__row">
        <span class="lb-metric__value">{{ value }}</span>
        <span v-if="unit" class="lb-metric__unit">{{ unit }}</span>
        <span v-if="total !== undefined && total !== null" class="lb-metric__unit">/ {{ total }}</span>
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
  gap: var(--sp-2);
  padding: var(--sp-4) var(--sp-5) var(--sp-5);
  min-width: 0;
  background: var(--surface);
  border: 1px solid var(--sep);
  border-radius: var(--r-card);
  box-shadow: var(--shadow);
}

.lb-metric__label {
  display: flex;
  align-items: center;
  gap: 6px;
  min-height: 24px;
  font-size: var(--fs-sm);
  font-weight: 500;
  color: var(--text2);
}
.lb-metric__label-text {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.lb-metric__icon {
  margin-left: auto;
  flex: none;
  width: 28px;
  height: 28px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: var(--r-tile);
  background: var(--surface2);
  border: 1px solid var(--sep2);
  color: var(--text3);
}
.lb-metric--warning .lb-metric__icon {
  background: var(--warn-bg);
  border-color: transparent;
  color: var(--warn);
}
.lb-metric--danger .lb-metric__icon {
  background: var(--bad-bg);
  border-color: transparent;
  color: var(--bad);
}
.lb-metric--success .lb-metric__icon {
  background: var(--ok-bg);
  border-color: transparent;
  color: var(--ok);
}

.lb-metric__row {
  display: flex;
  align-items: baseline;
  gap: 6px;
  min-width: 0;
  margin-top: 2px;
}

.lb-metric__value {
  font-size: var(--fs-metric);
  font-weight: 600;
  letter-spacing: -0.02em;
  line-height: 1.15;
  font-variant-numeric: tabular-nums;
  color: var(--text);
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.lb-metric--warning .lb-metric__value {
  color: var(--warn);
}
.lb-metric--danger .lb-metric__value {
  color: var(--bad);
}
.lb-metric__value--muted {
  color: var(--text3);
}

.lb-metric__unit {
  font-size: var(--fs-base);
  color: var(--text3);
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.lb-metric__foot {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  font-size: var(--fs-xs);
  color: var(--text3);
  min-height: 20px;
}

.lb-metric__failed {
  font-size: var(--fs-sm);
  font-weight: 500;
  color: var(--bad);
}

.lb-metric__skel {
  background: var(--surface3);
  border-radius: var(--r-sm);
  animation: lb-pulse 1.6s ease-in-out infinite;
}

.lb-metric__skel--value {
  width: 96px;
  height: 30px;
}

.lb-metric__skel--hint {
  width: 140px;
  height: 12px;
}

@media (max-width: 767px) {
  .lb-metric {
    padding: var(--sp-3) var(--sp-4) var(--sp-4);
  }
  .lb-metric__value {
    font-size: 24px;
  }
  .lb-metric__icon {
    display: none;
  }
}
</style>
