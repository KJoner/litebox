<script setup lang="ts">
import { computed } from 'vue'
import LbShapeIcon from './LbShapeIcon.vue'
import { statusMeta, type LbStatusKind, type LbStatusMeta } from './statusMeta'

/**
 * 状态永远同时带形状、文案与颜色。不做纯图标的状态列。
 *
 * 不裸用 a-tag:AntD Tag 只有色 + 文,给不了第三重编码。
 * meta 可直接传入,用于「停发订阅」「数据过期」这类不属于任何枚举的派生态。
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
    :style="{ color: m.fg, background: m.bg, borderColor: m.bd }"
  >
    <LbShapeIcon :shape="m.shape" :color="m.fg" :size="small ? 7 : 8" />
    <span>{{ m.text }}</span>
    <span v-if="props.suffix" class="lb-status__suffix">{{ props.suffix }}</span>
  </span>
</template>

<style scoped>
.lb-status {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 2px 7px;
  border: 1px solid;
  border-radius: 4px;
  font-size: 11.5px;
  font-weight: 500;
  line-height: 1.5;
  white-space: nowrap;
}

.lb-status--md {
  padding: 2px 8px;
  font-size: 12px;
}

.lb-status__suffix {
  font-family: 'IBM Plex Mono', ui-monospace, monospace;
  font-weight: 400;
  opacity: 0.75;
}
</style>
