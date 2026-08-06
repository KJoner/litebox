<script setup lang="ts">
import { computed } from 'vue'
import { color } from '@/theme/tokens'

/**
 * 轻量内联 SVG 趋势图。不引图表库。
 *
 * 关键规则:**缺失的点传 null,不补 0、不插值。**
 * 补 0 会让当天看起来「没人用」,插值会凭空造出一个从未存在的数字。
 * 折线用灰虚线跨过缺口,柱状用空心柱。
 */
export interface LbPoint {
  /** ISO 日期(UTC 日) */
  at: string
  /** null = 当天没有数据,不是 0 */
  value: number | null
}

const props = withDefaults(
  defineProps<{
    points: LbPoint[]
    type?: 'line' | 'bar'
    height?: number
    /** 标出区间最高点 */
    markMax?: boolean
  }>(),
  { type: 'line', height: 120, markMax: true },
)

const W = 800

const max = computed(() => {
  const vals = props.points.map((p) => p.value).filter((v): v is number => v !== null)
  return vals.length ? Math.max(...vals) : 0
})

const scaled = computed(() => {
  const h = props.height
  const n = Math.max(props.points.length - 1, 1)
  const top = 8
  const bottom = h - 10
  return props.points.map((p, i) => ({
    ...p,
    x: (i / n) * W,
    y:
      p.value === null || max.value === 0
        ? null
        : bottom - (p.value / max.value) * (bottom - top),
  }))
})

/** 把折线切成若干连续段,缺口不参与 polyline。 */
const segments = computed(() => {
  const segs: string[] = []
  let cur: string[] = []
  for (const p of scaled.value) {
    if (p.y === null) {
      if (cur.length > 1) segs.push(cur.join(' '))
      cur = []
    } else {
      cur.push(`${p.x},${p.y}`)
    }
  }
  if (cur.length > 1) segs.push(cur.join(' '))
  return segs
})

/** 缺口两端连一条灰虚线,让人看出「这里断了」而不是「这里是低谷」。 */
const gapLines = computed(() => {
  const out: { x1: number; y1: number; x2: number; y2: number }[] = []
  const pts = scaled.value
  for (let i = 0; i < pts.length; i++) {
    if (pts[i].y !== null) continue
    let l = i - 1
    while (l >= 0 && pts[l].y === null) l--
    let r = i + 1
    while (r < pts.length && pts[r].y === null) r++
    if (l >= 0 && r < pts.length) {
      out.push({ x1: pts[l].x, y1: pts[l].y as number, x2: pts[r].x, y2: pts[r].y as number })
    }
  }
  return out
})

const barW = computed(() => Math.max(2, (W / Math.max(props.points.length, 1)) * 0.62))

const maxPoint = computed(() => {
  if (!props.markMax || max.value === 0) return null
  return scaled.value.find((p) => p.value === max.value) ?? null
})

const hasGap = computed(() => props.points.some((p) => p.value === null))
defineExpose({ hasGap })
</script>

<template>
  <div class="lb-spark">
    <svg
      :viewBox="`0 0 ${W} ${props.height}`"
      :height="props.height"
      width="100%"
      preserveAspectRatio="none"
      role="img"
      :aria-label="`趋势图,峰值 ${max}`"
    >
      <line
        v-for="f in [0.25, 0.5, 0.75]"
        :key="f"
        x1="0"
        :y1="props.height * f"
        :x2="W"
        :y2="props.height * f"
        :stroke="color.borderSubtle"
      />
      <line x1="0" :y1="props.height - 10" :x2="W" :y2="props.height - 10" :stroke="color.border" />

      <template v-if="props.type === 'bar'">
        <rect
          v-for="(p, i) in scaled"
          :key="i"
          :x="p.x - barW / 2"
          :y="p.y === null ? props.height - 18 : p.y"
          :width="barW"
          :height="p.y === null ? 8 : props.height - 10 - p.y"
          :fill="p.y === null ? color.borderSubtle : p.value === max ? '#9EC3EC' : color.brandBorder"
        />
      </template>

      <template v-else>
        <line
          v-for="(g, i) in gapLines"
          :key="'g' + i"
          :x1="g.x1"
          :y1="g.y1"
          :x2="g.x2"
          :y2="g.y2"
          :stroke="color.divider"
          stroke-width="1.5"
          stroke-dasharray="3 3"
        />
        <polyline
          v-for="(s, i) in segments"
          :key="'s' + i"
          :points="s"
          fill="none"
          :stroke="color.brand"
          stroke-width="1.8"
          stroke-linejoin="round"
        />
      </template>

      <circle
        v-if="maxPoint && maxPoint.y !== null"
        :cx="maxPoint.x"
        :cy="maxPoint.y"
        r="3.5"
        :fill="color.brand"
      />
    </svg>
    <slot name="axis" />
  </div>
</template>

<style scoped>
.lb-spark {
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-width: 0;
}

.lb-spark svg {
  display: block;
}
</style>
