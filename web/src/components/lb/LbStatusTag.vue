<script setup lang="ts">
import { computed } from 'vue'
import LbShapeIcon from './LbShapeIcon.vue'
import { statusMeta, type LbStatusKind, type LbStatusMeta } from './statusMeta'

/**
 * 状态永远同时带形状、文案与颜色。不做纯图标的状态列。
 *
 * 不裸用 a-tag:AntD Tag 只有色 + 文,给不了第三重编码。
 * meta 可直接传入,用于「停发订阅」「数据过期」这类不属于任何枚举的派生态。
 *
 * UI 2.0:胶囊、语义浅底(10%)+ 一圈同色细描边,都从 meta 里来。
 * 胶囊只留给状态 —— 中性元数据用矩形的 .lb-chip,两者在形状上分开。
 */
const props = defineProps<{
  kind?: LbStatusKind
  status?: string
  meta?: LbStatusMeta
  /** 附在文案后的小字,例如 rev 号:已同步 rev 41 */
  suffix?: string
  size?: 'sm' | 'md'
}>()

const m = computed<LbStatusMeta>(() =>
  props.meta ?? statusMeta(props.kind ?? 'node', props.status ?? ''),
)
const small = computed(() => props.size !== 'md')
</script>

<template>
  <span
    class="lb-status"
    :class="{ 'lb-status--md': !small }"
    :style="{ color: m.fg, background: m.bg }"
  >
    <LbShapeIcon :shape="m.shape" :color="m.fg" :size="7" />
    <span>{{ m.text }}</span>
    <span v-if="props.suffix" class="lb-status__suffix">{{ props.suffix }}</span>
  </span>
</template>

<style scoped>
.lb-status {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 1px 8px;
  border-radius: var(--r-pill);
  font-size: var(--fs-xs);
  font-weight: 500;
  line-height: 20px;
  white-space: nowrap;
  /* 1px 同色描边(透明度由浅底决定):浅底在白卡片上偏淡,加一圈边形状才立得住。 */
  box-shadow: inset 0 0 0 1px color-mix(in srgb, currentColor 16%, transparent);
}

.lb-status--md {
  padding: 2px 10px;
  font-size: var(--fs-sm);
}

.lb-status__suffix {
  font-family: var(--mono);
  font-size: 0.92em;
  font-weight: 400;
  font-variant-numeric: tabular-nums;
  opacity: 0.8;
}
</style>
