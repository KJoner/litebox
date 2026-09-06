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
 * V18:胶囊、无边框,底色是语义色 12% alpha(深色 18%),都从 meta 里来。
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
  padding: 3px 9px;
  border-radius: var(--r-pill);
  font-size: 12px;
  font-weight: 600;
  line-height: 1.4;
  white-space: nowrap;
}

.lb-status--md {
  padding: 4px 11px;
  font-size: 12.5px;
}

.lb-status__suffix {
  font-family: var(--mono);
  font-weight: 400;
  font-variant-numeric: tabular-nums;
  opacity: 0.75;
}
</style>
