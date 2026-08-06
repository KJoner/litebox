<script setup lang="ts">
/**
 * 节点资源趋势图。
 *
 * 与 LbSparkline 一样用内联 SVG:整个前端要嵌进 Go 二进制,
 * 为几条折线引入图表库会让产物大出几百 KB。
 *
 * 支持一到两条序列(上下行速率要画在一起才看得出方向差异),
 * 单序列时不画图例 —— 标题已经说明了画的是什么。
 */
import { computed, ref } from 'vue'

const props = withDefaults(
  defineProps<{
    /** 每条序列:名称、颜色、按时间顺序排列的值 */
    series: { name: string; color: string; values: number[] }[]
    /** 与 values 等长的时间戳(RFC3339) */
    labels: string[]
    /** 把数值格式化成可读文本 */
    format: (value: number) => string
    height?: number
    /** 纵轴上限。给百分比类指标固定 100,免得 3% 的曲线被拉满整张图 */
    maxOverride?: number
  }>(),
  { height: 180 },
)

const width = 800
const padding = { top: 12, right: 12, bottom: 24, left: 64 }

const plotWidth = computed(() => width - padding.left - padding.right)
const plotHeight = computed(() => props.height - padding.top - padding.bottom)

const pointCount = computed(() => props.labels.length)

const maxValue = computed(() => {
  if (props.maxOverride !== undefined) return props.maxOverride
  const max = Math.max(0, ...props.series.flatMap((s) => s.values))
  return max === 0 ? 1 : max
})

const yTicks = computed(() => {
  const steps = 4
  return Array.from({ length: steps + 1 }, (_, i) => {
    const value = (maxValue.value / steps) * i
    return {
      value,
      y: padding.top + plotHeight.value - (value / maxValue.value) * plotHeight.value,
    }
  })
})

function xAt(index: number): number {
  if (pointCount.value <= 1) return padding.left + plotWidth.value / 2
  return padding.left + (index / (pointCount.value - 1)) * plotWidth.value
}

function yAt(value: number): number {
  const clamped = Math.max(0, Math.min(value, maxValue.value))
  return padding.top + plotHeight.value - (clamped / maxValue.value) * plotHeight.value
}

const paths = computed(() =>
  props.series.map((s) => ({
    ...s,
    d: s.values
      .map((v, i) => `${i === 0 ? 'M' : 'L'}${xAt(i).toFixed(1)},${yAt(v).toFixed(1)}`)
      .join(' '),
  })),
)

// X 轴最多标 6 个时间点,避免标签互相压住。
const xLabels = computed(() => {
  const n = pointCount.value
  if (n === 0) return []
  const stride = Math.max(1, Math.ceil(n / 6))
  return props.labels
    .map((label, i) => ({ label, i }))
    .filter(({ i }) => i % stride === 0 || i === n - 1)
    .map(({ label, i }) => ({ x: xAt(i), text: shortTime(label) }))
})

function shortTime(value: string): string {
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`
}

const hoverIndex = ref<number | null>(null)

function onMove(event: MouseEvent) {
  if (pointCount.value === 0) return
  const svg = event.currentTarget as SVGSVGElement
  const rect = svg.getBoundingClientRect()
  // SVG 用 viewBox 缩放,鼠标坐标要换算回 viewBox 坐标系。
  const x = ((event.clientX - rect.left) / rect.width) * width
  let nearest = 0
  let best = Infinity
  for (let i = 0; i < pointCount.value; i++) {
    const d = Math.abs(xAt(i) - x)
    if (d < best) {
      best = d
      nearest = i
    }
  }
  hoverIndex.value = nearest
}

const tooltipStyle = computed(() => {
  if (hoverIndex.value === null) return {}
  const x = xAt(hoverIndex.value)
  const leftPercent = (x / width) * 100
  const flip = leftPercent > 62
  return {
    left: `${leftPercent}%`,
    top: '8px',
    transform: flip ? 'translateX(-100%)' : 'translateX(12px)',
    marginLeft: flip ? '-12px' : '0',
  }
})
</script>

<template>
  <div class="chart-root">
    <div v-if="pointCount === 0" class="chart-empty">暂无监控数据</div>
    <div v-else class="chart-wrap">
      <svg
        :viewBox="`0 0 ${width} ${height}`"
        :style="{ height: `${height}px` }"
        class="chart-svg"
        role="img"
        @mousemove="onMove"
        @mouseleave="hoverIndex = null"
      >
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

        <path
          v-for="p in paths"
          :key="p.name"
          :d="p.d"
          class="line"
          :style="{ stroke: p.color }"
        />

        <template v-if="hoverIndex !== null">
          <line
            :x1="xAt(hoverIndex)"
            :x2="xAt(hoverIndex)"
            :y1="padding.top"
            :y2="padding.top + plotHeight"
            class="crosshair"
          />
          <circle
            v-for="p in series"
            :key="p.name"
            :cx="xAt(hoverIndex)"
            :cy="yAt(p.values[hoverIndex] ?? 0)"
            r="4"
            class="marker"
            :style="{ fill: p.color }"
          />
        </template>

        <g class="axis-text">
          <text
            v-for="tick in yTicks"
            :key="`y-${tick.value}`"
            :x="padding.left - 8"
            :y="tick.y + 4"
            text-anchor="end"
          >
            {{ format(tick.value) }}
          </text>
          <text
            v-for="(l, i) in xLabels"
            :key="`x-${i}`"
            :x="l.x"
            :y="height - 6"
            text-anchor="middle"
          >
            {{ l.text }}
          </text>
        </g>
      </svg>

      <div v-if="hoverIndex !== null" class="tooltip" :style="tooltipStyle">
        <div class="tooltip-time">{{ shortTime(labels[hoverIndex]) }}</div>
        <div v-for="s in series" :key="s.name" class="tooltip-row">
          <span class="dot" :style="{ background: s.color }" />
          <span>{{ s.name }}</span>
          <b>{{ format(s.values[hoverIndex] ?? 0) }}</b>
        </div>
      </div>

      <!-- 单序列不画图例:标题已经说明了画的是什么,再加一行只是噪声。 -->
      <div v-if="series.length > 1" class="legend">
        <span v-for="s in series" :key="s.name" class="legend-item">
          <span class="dot" :style="{ background: s.color }" />{{ s.name }}
        </span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.chart-root {
  --series-1: #2a78d6;
  --text-muted: #898781;
  --gridline: #e1e0d9;
  position: relative;
}

@media (prefers-color-scheme: dark) {
  .chart-root {
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

.line {
  fill: none;
  stroke-width: 2;
  stroke-linejoin: round;
  stroke-linecap: round;
}

.crosshair {
  stroke: var(--text-muted);
  stroke-width: 1;
}

.marker {
  stroke: #fff;
  stroke-width: 2;
}

.axis-text text {
  fill: var(--text-muted);
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

.tooltip-time {
  color: var(--text-muted);
  margin-bottom: 4px;
}

.tooltip-row {
  display: flex;
  align-items: center;
  gap: 6px;
}

.tooltip-row b {
  margin-left: auto;
  font-variant-numeric: tabular-nums;
}

.dot {
  display: inline-block;
  width: 8px;
  height: 8px;
  border-radius: 50%;
}

.legend {
  display: flex;
  gap: 16px;
  justify-content: center;
  font-size: 12px;
  color: var(--text-muted);
}

.legend-item {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.chart-empty {
  height: 140px;
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--text-muted);
  font-size: 13px;
}
</style>
