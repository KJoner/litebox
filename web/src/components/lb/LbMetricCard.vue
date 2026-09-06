<script setup lang="ts">
import LbInfoTip from './LbInfoTip.vue'

/**
 * 指标条里的一格。V18 起四个指标不再是四张卡,而是**一张卡片内的分隔栅格**:
 * 父容器用 `.lb-metrics`(base.css),每格靠 box-shadow 画 1px 分隔线,
 * 折行时仍然正确。
 *
 * 四种状态各有各的写法,重点是 error:现有代码写的是
 * summary?.traffic_month ?? 0 —— 接口失败时页面稳稳显示「本月流量 0 B」,
 * 读不到和真的是零长得一模一样。这里 error 时显示「—」,绝不回退成 0。
 */
withDefaults(
  defineProps<{
    label: string
    /** 标签旁的 ⓘ 文案。 */
    tip?: string
    value?: string | number | null
    unit?: string
    /** 分母,例如 8 / 10 */
    total?: string | number | null
    state?: 'ready' | 'loading' | 'empty' | 'error'
    tone?: 'default' | 'warning' | 'danger'
    /** 底部一行说明,或用 #foot 插槽放标签 / 「筛选 ›」链接 */
    hint?: string
    emptyHint?: string
  }>(),
  { state: 'ready', tone: 'default', emptyHint: '尚无数据' },
)
</script>

<template>
  <div class="lb-metric" :class="`lb-metric--${tone}`">
    <div class="lb-metric__label">
      <span>{{ label }}</span>
      <LbInfoTip v-if="tip" :text="tip" />
      <slot name="tip" />
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
  gap: 14px;
  padding: 26px 28px;
  min-width: 0;
  /* 分隔线用 box-shadow 而不是 border:折行之后左右两端的边线仍然对得上。 */
  box-shadow: 0 0 0 1px var(--sep2);
}

.lb-metric__label {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 13px;
  font-weight: 500;
  color: var(--text3);
}

.lb-metric__row {
  display: flex;
  align-items: baseline;
  gap: 8px;
  min-width: 0;
}

.lb-metric__value {
  font-size: 44px;
  font-weight: 700;
  letter-spacing: -0.04em;
  line-height: 1;
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
  font-size: 16px;
  color: var(--text3);
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.lb-metric__foot {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  font-size: 12.5px;
  color: var(--text2);
  min-height: 18px;
}

.lb-metric__failed {
  font-size: 13px;
  font-weight: 500;
  color: var(--bad);
}

.lb-metric__skel {
  background: var(--fill);
  border-radius: 8px;
}

.lb-metric__skel--value {
  width: 120px;
  height: 40px;
}

.lb-metric__skel--hint {
  width: 140px;
  height: 12px;
}

@media (max-width: 767px) {
  .lb-metric {
    padding: 18px 20px;
  }
  .lb-metric__value {
    font-size: 32px;
  }
}
</style>
