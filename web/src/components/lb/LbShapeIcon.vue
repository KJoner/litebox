<script setup lang="ts">
import type { LbShape } from './statusMeta'

// 形状是状态的第三重编码,不是装饰。每个形状只对应一种语义。
const props = withDefaults(defineProps<{ shape: LbShape; color: string; size?: number }>(), {
  size: 8,
})
</script>

<template>
  <svg
    v-if="props.shape === 'dot'"
    :width="props.size" :height="props.size" viewBox="0 0 8 8" aria-hidden="true"
  >
    <circle cx="4" cy="4" r="4" :fill="props.color" />
  </svg>

  <svg
    v-else-if="props.shape === 'cross'"
    :width="props.size + 1" :height="props.size + 1" viewBox="0 0 9 9" aria-hidden="true"
  >
    <path d="M1.2 1.2 7.8 7.8M7.8 1.2 1.2 7.8" :stroke="props.color" stroke-width="1.8" stroke-linecap="round" />
  </svg>

  <svg
    v-else-if="props.shape === 'triangle'"
    :width="props.size + 2" :height="props.size + 1" viewBox="0 0 10 9" aria-hidden="true"
  >
    <path d="M5 0.5 9.5 8.5H0.5Z" :fill="props.color" />
  </svg>

  <!-- reduced-motion 下不旋转:进度信息由旁边的文字承载,不靠动画。 -->
  <svg
    v-else-if="props.shape === 'spinner'"
    :width="props.size + 1" :height="props.size + 1" viewBox="0 0 9 9" class="lb-spin" aria-hidden="true"
  >
    <circle cx="4.5" cy="4.5" r="3.6" fill="none" :stroke="props.color" stroke-width="1.8" stroke-dasharray="4 2.6" />
  </svg>

  <svg
    v-else-if="props.shape === 'pause'"
    :width="props.size" :height="props.size" viewBox="0 0 8 8" aria-hidden="true"
  >
    <rect x="0" y="0" width="3" height="8" :fill="props.color" />
    <rect x="5" y="0" width="3" height="8" :fill="props.color" />
  </svg>

  <svg
    v-else-if="props.shape === 'square'"
    :width="props.size + 1" :height="props.size + 1" viewBox="0 0 9 9" aria-hidden="true"
  >
    <rect x="1" y="1" width="7" height="7" rx="1" fill="none" :stroke="props.color" stroke-width="1.6" />
  </svg>

  <svg
    v-else-if="props.shape === 'ring'"
    :width="props.size" :height="props.size" viewBox="0 0 8 8" aria-hidden="true"
  >
    <circle cx="4" cy="4" r="3" fill="none" :stroke="props.color" stroke-width="2" />
  </svg>

  <svg
    v-else-if="props.shape === 'dashRing'"
    :width="props.size" :height="props.size" viewBox="0 0 8 8" aria-hidden="true"
  >
    <circle cx="4" cy="4" r="3" fill="none" :stroke="props.color" stroke-width="1.6" stroke-dasharray="2 2" />
  </svg>

  <svg
    v-else-if="props.shape === 'check'"
    :width="props.size + 2" :height="props.size" viewBox="0 0 10 8" aria-hidden="true"
  >
    <path d="M1 4.2 3.7 6.8 9 1.4" fill="none" :stroke="props.color" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" />
  </svg>

  <svg
    v-else :width="props.size + 1" :height="props.size + 1" viewBox="0 0 9 9" aria-hidden="true"
  >
    <path d="M1.6 4.5h5.8" :stroke="props.color" stroke-width="1.8" stroke-linecap="round" />
  </svg>
</template>

<style scoped>
.lb-spin {
  animation: lb-spin 1.1s linear infinite;
}

@keyframes lb-spin {
  to {
    transform: rotate(360deg);
  }
}
</style>
