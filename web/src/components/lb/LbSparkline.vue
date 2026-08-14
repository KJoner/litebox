<script setup lang="ts">
import { computed, ref } from 'vue'
import { color } from '@/theme/tokens'
import { formatBytes, formatUTCDay } from '@/utils/format'

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
    /** 数值格式化。默认按字节 —— 目前五处调用画的都是流量。 */
    format?: (value: number) => string
    /** 悬停时这一格的标题。默认把 ISO 日按 UTC 渲染成「8 月 14 日」。 */
    labelFormat?: (at: string) => string
  }>(),
  {
    type: 'line',
    height: 120,
    markMax: true,
    format: formatBytes,
    labelFormat: formatUTCDay,
  },
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

// ---------- 悬停读数 ----------
//
// 图上只有形状,没有数字。管理员看到「这天比昨天高一截」之后必然要问高多少,
// 没有读数就只能去别处翻。缺失的日子同样要能悬停 —— 「当天没有记录」和
// 「当天是 0」在图上一个是空心柱一个是贴底的实柱,但那点差别在 140px 高的
// 图里很容易看反,悬停是唯一能把两者说死的地方。

const hover = ref<number | null>(null)

function onMove(event: MouseEvent) {
  const n = props.points.length
  if (n === 0) return
  const rect = (event.currentTarget as SVGSVGElement).getBoundingClientRect()
  if (rect.width === 0) return
  // viewBox 固定 0..W 且 preserveAspectRatio="none",所以按比例换算即可。
  const x = ((event.clientX - rect.left) / rect.width) * W
  const step = W / Math.max(n - 1, 1)
  hover.value = Math.max(0, Math.min(n - 1, Math.round(x / step)))
}

const hoverPoint = computed(() => (hover.value === null ? null : scaled.value[hover.value] ?? null))

/** 左侧 62% 之内向右展开,之后翻到左边 —— 否则最后几天的读数会被裁在图外。 */
const tipStyle = computed(() => {
  const p = hoverPoint.value
  if (!p) return {}
  const leftPercent = (p.x / W) * 100
  const flip = leftPercent > 62
  return {
    left: `${leftPercent}%`,
    transform: flip ? 'translateX(-100%)' : 'translateX(10px)',
    marginLeft: flip ? '-10px' : '0',
  }
})
</script>

<template>
  <div class="lb-spark">
    <svg
      :viewBox="`0 0 ${W} ${props.height}`"
      :height="props.height"
      width="100%"
      preserveAspectRatio="none"
      role="img"
      :aria-label="`趋势图,峰值 ${props.format(max)}`"
      @mousemove="onMove"
      @mouseleave="hover = null"
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

      <!-- 悬停标记画在最后,盖在柱子和折线之上。 -->
      <template v-if="hoverPoint">
        <line
          :x1="hoverPoint.x"
          y1="4"
          :x2="hoverPoint.x"
          :y2="props.height - 10"
          :stroke="color.text3"
          stroke-width="1"
          stroke-dasharray="2 2"
        />
        <circle
          v-if="hoverPoint.y !== null"
          :cx="hoverPoint.x"
          :cy="hoverPoint.y"
          r="3.5"
          :fill="color.brand"
          stroke="#fff"
          stroke-width="1.5"
        />
      </template>
    </svg>

    <!-- 读数框跟着鼠标横向走,纵向固定在顶部:柱高会变,跟着纵向跳会读不稳。 -->
    <div v-if="hoverPoint" class="lb-spark__tip" :style="tipStyle">
      <div class="lb-spark__tip-day">{{ props.labelFormat(hoverPoint.at) }}</div>
      <div v-if="hoverPoint.value === null" class="lb-spark__tip-none">
        当天没有记录(不是 0)
      </div>
      <b v-else class="lb-mono">{{ props.format(hoverPoint.value) }}</b>
    </div>

    <slot name="axis" />
  </div>
</template>

<style scoped>
.lb-spark {
  position: relative;
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-width: 0;
}

.lb-spark svg {
  display: block;
}

/* 色值取自 tokens.ts:bgSurface / border / text3 / shadowOverlay。 */
.lb-spark__tip {
  position: absolute;
  top: 2px;
  z-index: 2;
  padding: 6px 9px;
  background: rgb(255 255 255 / 97%);
  border: 1px solid #e3e6ea;
  border-radius: 6px;
  box-shadow: 0 4px 12px rgb(20 24 28 / 8%);
  font-size: 12px;
  line-height: 1.5;
  white-space: nowrap;
  /* 读数框自己不能吃鼠标事件,否则鼠标移到它上面就触发 mouseleave,框会闪。 */
  pointer-events: none;
}

.lb-spark__tip-day {
  font-size: 10.5px;
  color: #6b7480;
}

.lb-spark__tip-none {
  color: #6b7480;
}
</style>
