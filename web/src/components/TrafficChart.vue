<script setup lang="ts">
/**
 * 每日流量趋势图。
 *
 * 形式选择:单序列面积图。数据的任务是"随时间的变化",单序列因此不需要图例 ——
 * 标题已经说明了画的是什么。刻意不做上行/下行堆叠:实测上行只占总量的
 * 万分之三(如 771 / 3,010,319 字节),堆叠后上行是一条看不见的细线,
 * 既没有信息量又占掉一个颜色槽。上下行的拆分放进悬浮提示,那里才读得到。
 *
 * 用内联 SVG 而非图表库:整个页面要嵌进 Go 二进制,再塞一个图表库
 * 会让产物大出几百 KB,而这里只需要一条折线。
 */
import { computed, ref } from 'vue'
import type { DailyPoint } from '@/api/client'
import { formatBytes } from '@/utils/format'

const props = withDefaults(
  defineProps<{
    data: DailyPoint[]
    height?: number
  }>(),
  { height: 220 },
)

const width = 800
const padding = { top: 16, right: 16, bottom: 28, left: 64 }

const plotWidth = computed(() => width - padding.left - padding.right)
const plotHeight = computed(() => props.height - padding.top - padding.bottom)

const maxValue = computed(() => {
  const max = Math.max(0, ...props.data.map((d) => d.total))
  return max === 0 ? 1 : max
})

/** Y 轴刻度取整到干净数值,承载没有被直接标注的量级。 */
const yTicks = computed(() => {
  const max = maxValue.value
  const steps = 4
  return Array.from({ length: steps + 1 }, (_, i) => {
    const value = (max / steps) * i
    return { value, y: padding.top + plotHeight.value - (value / max) * plotHeight.value }
  })
})

const points = computed(() =>
  props.data.map((d, i) => {
    const x =
      props.data.length === 1
        ? padding.left + plotWidth.value / 2
        : padding.left + (i / (props.data.length - 1)) * plotWidth.value
    const y = padding.top + plotHeight.value - (d.total / maxValue.value) * plotHeight.value
    return { ...d, x, y }
  }),
)

const linePath = computed(() =>
  points.value.map((p, i) => `${i === 0 ? 'M' : 'L'}${p.x.toFixed(1)},${p.y.toFixed(1)}`).join(' '),
)

const areaPath = computed(() => {
  if (points.value.length === 0) return ''
  const baseline = padding.top + plotHeight.value
  const first = points.value[0]
  const last = points.value[points.value.length - 1]
  return `M${first.x.toFixed(1)},${baseline} ${linePath.value.slice(1)} L${last.x.toFixed(1)},${baseline} Z`
})

/** X 轴最多标 6 个日期,避免标签互相压住。 */
const xLabels = computed(() => {
  const n = points.value.length
  if (n === 0) return []
  const stride = Math.max(1, Math.ceil(n / 6))
  return points.value.filter((_, i) => i % stride === 0 || i === n - 1)
})

const lastPoint = computed(() => points.value[points.value.length - 1] ?? null)

const hoverIndex = ref<number | null>(null)
const hovered = computed(() =>
  hoverIndex.value === null ? null : (points.value[hoverIndex.value] ?? null),
)

function onMove(event: MouseEvent) {
  if (points.value.length === 0) return
  const svg = event.currentTarget as SVGSVGElement
  const rect = svg.getBoundingClientRect()
  // SVG 用 viewBox 缩放,鼠标坐标要换算回 viewBox 坐标系。
  const x = ((event.clientX - rect.left) / rect.width) * width
  let nearest = 0
  let best = Infinity
  points.value.forEach((p, i) => {
    const d = Math.abs(p.x - x)
    if (d < best) {
      best = d
      nearest = i
    }
  })
  hoverIndex.value = nearest
}

/** 提示框贴着数据点,靠近右边界时翻到左侧,避免溢出画布。 */
const tooltipStyle = computed(() => {
  const p = hovered.value
  if (!p) return {}
  const leftPercent = (p.x / width) * 100
  const flip = leftPercent > 62
  return {
    left: `${leftPercent}%`,
    transform: flip ? 'translate(-100%, -50%)' : 'translate(12px, -50%)',
    top: `${((p.y / props.height) * 100).toFixed(1)}%`,
    marginLeft: flip ? '-12px' : '0',
  }
})
</script>

<template>
  <div class="chart-root">
    <div v-if="data.length === 0" class="chart-empty">暂无流量数据</div>
    <div v-else class="chart-wrap">
      <svg
        :viewBox="`0 0 ${width} ${height}`"
        :style="{ height: `${height}px` }"
        class="chart-svg"
        role="img"
        aria-label="每日流量趋势"
        @mousemove="onMove"
        @mouseleave="hoverIndex = null"
      >
        <!-- 网格线:一档灰,1px 实线,退到背景里 -->
        <g class="grid">
          <line
            v-for="tick in yTicks"
            :key="tick.value"
            :x1="padding.left"
            :x2="width - padding.right"
            :y1="tick.y"
            :y2="tick.y"
          />
        </g>

        <!-- 面积用序列色 10% 透明度,是一层薄雾而不是色块 -->
        <path :d="areaPath" class="area" />
        <path :d="linePath" class="line" />

        <!--
          末点始终画出来:只有一天数据时折线退化成一个点、面积退化成零宽度,
          不画端点的话整张图是空白的。同时它也是唯一的直接标注 ——
          标注要稀疏才有用,其余数值交给坐标轴与悬浮提示。
        -->
        <template v-if="lastPoint && !hovered">
          <circle :cx="lastPoint.x" :cy="lastPoint.y" r="4" class="marker" />
          <text
            :x="lastPoint.x"
            :y="lastPoint.y - 12"
            :text-anchor="points.length === 1 ? 'middle' : 'end'"
            class="end-label"
          >
            {{ formatBytes(lastPoint.total) }}
          </text>
        </template>

        <!-- 悬浮十字线与端点 -->
        <template v-if="hovered">
          <line
            :x1="hovered.x"
            :x2="hovered.x"
            :y1="padding.top"
            :y2="padding.top + plotHeight"
            class="crosshair"
          />
          <circle :cx="hovered.x" :cy="hovered.y" r="5" class="marker" />
        </template>

        <!-- 轴文字用文本色,不穿序列色 -->
        <g class="axis-text">
          <text
            v-for="tick in yTicks"
            :key="`y-${tick.value}`"
            :x="padding.left - 8"
            :y="tick.y + 4"
            text-anchor="end"
          >
            {{ formatBytes(tick.value) }}
          </text>
          <text
            v-for="p in xLabels"
            :key="`x-${p.day}`"
            :x="p.x"
            :y="height - 8"
            text-anchor="middle"
          >
            {{ p.day.slice(5) }}
          </text>
        </g>
      </svg>

      <div v-if="hovered" class="tooltip" :style="tooltipStyle">
        <div class="tooltip-day">{{ hovered.day }}</div>
        <div class="tooltip-row">
          <span>合计</span><b>{{ formatBytes(hovered.total) }}</b>
        </div>
        <div class="tooltip-row muted">
          <span>上行</span><b>{{ formatBytes(hovered.uplink) }}</b>
        </div>
        <div class="tooltip-row muted">
          <span>下行</span><b>{{ formatBytes(hovered.downlink) }}</b>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
/* 颜色按角色声明一次,浅色/深色只在这里切换。 */
.chart-root {
  --surface: #fcfcfb;
  --series-1: #2a78d6;
  --text-secondary: #52514e;
  --muted: #898781;
  --gridline: #e1e0d9;
  position: relative;
}

@media (prefers-color-scheme: dark) {
  .chart-root {
    --surface: #1a1a19;
    --series-1: #3987e5;
    --text-secondary: #c3c2b7;
    --gridline: #2c2c2a;
  }
}

.chart-svg {
  width: 100%;
  display: block;
}

.grid line {
  stroke: var(--gridline);
  stroke-width: 1;
}

.area {
  fill: var(--series-1);
  fill-opacity: 0.1;
}

.line {
  fill: none;
  stroke: var(--series-1);
  stroke-width: 2;
  stroke-linejoin: round;
  stroke-linecap: round;
}

.crosshair {
  stroke: var(--muted);
  stroke-width: 1;
}

/* 端点带 2px 表面色描边,压在折线上时仍然清晰 */
.marker {
  fill: var(--series-1);
  stroke: var(--surface);
  stroke-width: 2;
}

.axis-text text {
  fill: var(--muted);
  font-size: 11px;
  font-variant-numeric: tabular-nums;
}

/* 直接标注也用文本色,不穿序列色 —— 身份由旁边的彩色端点承担 */
.end-label {
  fill: var(--text-secondary);
  font-size: 11px;
  font-variant-numeric: tabular-nums;
}

.tooltip {
  position: absolute;
  pointer-events: none;
  background: rgb(255 255 255 / 97%);
  border: 1px solid rgb(11 11 11 / 10%);
  border-radius: 6px;
  padding: 8px 10px;
  font-size: 12px;
  box-shadow: 0 2px 8px rgb(0 0 0 / 12%);
  white-space: nowrap;
  z-index: 2;
}

.tooltip-day {
  color: var(--muted);
  margin-bottom: 4px;
}

.tooltip-row {
  display: flex;
  justify-content: space-between;
  gap: 16px;
  color: var(--text-secondary);
}

.tooltip-row.muted {
  color: var(--muted);
}

.tooltip-row b {
  font-variant-numeric: tabular-nums;
}

.chart-empty {
  height: 160px;
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--muted);
  font-size: 13px;
}
</style>
