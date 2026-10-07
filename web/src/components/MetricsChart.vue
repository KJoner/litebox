<script setup lang="ts">
/**
 * 节点资源趋势图(V20 重做)。
 *
 * 与 LbSparkline 一样用内联 SVG:整个前端要嵌进 Go 二进制,
 * 为几条折线引入图表库会让产物大出几百 KB。
 *
 * 重做的三件事,都是原实现里"鼠标位置与读数不一致"的来源:
 *
 *   1. **按容器实际像素画,不用 viewBox 缩放。** 原来 viewBox 固定 800 宽、高度又按 px
 *      定死,容器宽高比不等于 800:height 时浏览器按 meet 规则留白居中,鼠标坐标、
 *      指示线与提示框三套换算各错各的。现在用 ResizeObserver 量容器宽度,SVG 的
 *      width / height 就是像素,x 坐标 = 像素,文字也不再随宽度缩放。
 *   2. **横轴按时间,不按下标。** 采样会缺(机器重启、采集失败、刚改完地址),
 *      按下标等距排列会把 3 小时的空档画成相邻两点之间的一条直线。相邻两点间隔超过
 *      正常采样间隔两倍时线断开,悬停在空档上显示「没有采样」而不是插一个值出来。
 *   3. **提示框显示完整日期时间与单位**,鼠标按时间找最近的点;数据刷新时
 *      悬停位置按时间重算,不会跳到别的点上。
 */
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { formatBytes } from '@/utils/format'

const props = withDefaults(
  defineProps<{
    /** 每条序列:名称、颜色、按时间顺序排列的值(null = 这个点没有采样) */
    series: { name: string; color: string; values: (number | null)[] }[]
    /** 与 values 等长的时间戳(RFC3339) */
    labels: string[]
    /** 把数值格式化成可读文本(带单位) */
    format: (value: number) => string
    height?: number
    /** 纵轴上限。给百分比类指标固定 100,免得 3% 的曲线被拉满整张图 */
    maxOverride?: number
    /** 相邻两点间隔超过它就算缺采样、线断开。缺省按采样间隔中位数的 2.5 倍 */
    gapMs?: number
    /** 纵轴刻度的单位提示(进提示框标题),例如 "%" 或 "B/s" */
    unitHint?: string
  }>(),
  { height: 180 },
)

const root = ref<HTMLElement | null>(null)
const width = ref(600)
let ro: ResizeObserver | null = null

onMounted(() => {
  if (root.value) width.value = root.value.clientWidth || 600
  if (typeof ResizeObserver !== 'undefined' && root.value) {
    ro = new ResizeObserver((entries) => {
      const w = entries[0]?.contentRect.width
      if (w && Math.abs(w - width.value) > 0.5) width.value = w
    })
    ro.observe(root.value)
  }
})
onBeforeUnmount(() => ro?.disconnect())

const padding = { top: 12, right: 14, bottom: 26, left: 64 }
const plotWidth = computed(() => Math.max(10, width.value - padding.left - padding.right))
const plotHeight = computed(() => props.height - padding.top - padding.bottom)

/** 时间轴:解析不了的标签当缺失。 */
const times = computed(() => props.labels.map((l) => new Date(l).getTime()))
const pointCount = computed(() => times.value.length)
const tMin = computed(() => (pointCount.value ? Math.min(...times.value.filter(Number.isFinite)) : 0))
const tMax = computed(() => (pointCount.value ? Math.max(...times.value.filter(Number.isFinite)) : 1))

/** 断线阈值:采样间隔中位数的 2.5 倍;点太少时不断。 */
const gapThreshold = computed(() => {
  if (props.gapMs) return props.gapMs
  const ts = times.value.filter(Number.isFinite).sort((a, b) => a - b)
  if (ts.length < 3) return Infinity
  const diffs: number[] = []
  for (let i = 1; i < ts.length; i++) diffs.push(ts[i]! - ts[i - 1]!)
  diffs.sort((a, b) => a - b)
  const median = diffs[Math.floor(diffs.length / 2)] ?? 0
  return median > 0 ? median * 2.5 : Infinity
})

function xAt(t: number): number {
  if (tMax.value <= tMin.value) return padding.left + plotWidth.value / 2
  return padding.left + ((t - tMin.value) / (tMax.value - tMin.value)) * plotWidth.value
}

/** 纵轴上限取"好看"的数:1 / 2 / 5 × 10^n,百分比类固定 100。 */
const maxValue = computed(() => {
  if (props.maxOverride !== undefined) return props.maxOverride
  let max = 0
  for (const s of props.series) for (const v of s.values) if (v !== null && v > max) max = v
  if (max <= 0) return 1
  const exp = Math.pow(10, Math.floor(Math.log10(max)))
  const m = max / exp
  const nice = m <= 1 ? 1 : m <= 2 ? 2 : m <= 5 ? 5 : 10
  return nice * exp
})

function yAt(value: number): number {
  const clamped = Math.max(0, Math.min(value, maxValue.value))
  return padding.top + plotHeight.value - (clamped / maxValue.value) * plotHeight.value
}

const yTicks = computed(() => {
  const steps = 4
  return Array.from({ length: steps + 1 }, (_, i) => {
    const value = (maxValue.value / steps) * i
    return { value, y: yAt(value) }
  })
})

/** 折线:按时间排,缺采样或间隔过大处断开成多段。 */
const paths = computed(() =>
  props.series.map((s) => {
    const parts: string[] = []
    let pen = false
    let prevT = NaN
    for (let i = 0; i < pointCount.value; i++) {
      const t = times.value[i]!
      const v = s.values[i]
      if (!Number.isFinite(t) || v === null || v === undefined) {
        pen = false
        continue
      }
      const broken = Number.isFinite(prevT) && t - prevT > gapThreshold.value
      parts.push(`${pen && !broken ? 'L' : 'M'}${xAt(t).toFixed(1)},${yAt(v).toFixed(1)}`)
      pen = true
      prevT = t
    }
    return { ...s, d: parts.join(' ') }
  }),
)

/** 单独的点(前后都断开)画成小圆点,否则一条孤立的采样在图上什么都看不见。 */
const lonePoints = computed(() => {
  const out: { x: number; y: number; color: string }[] = []
  for (const s of props.series) {
    for (let i = 0; i < pointCount.value; i++) {
      const v = s.values[i]
      const t = times.value[i]!
      if (v === null || v === undefined || !Number.isFinite(t)) continue
      const prevOK = i > 0 && s.values[i - 1] != null && t - times.value[i - 1]! <= gapThreshold.value
      const nextOK =
        i < pointCount.value - 1 && s.values[i + 1] != null && times.value[i + 1]! - t <= gapThreshold.value
      if (!prevOK && !nextOK) out.push({ x: xAt(t), y: yAt(v), color: s.color })
    }
  }
  return out
})

// X 轴标签:按像素密度决定个数(每 110px 一个),标签带日期 —— 72h / 168h 下只有时分看不出是哪天。
const xLabels = computed(() => {
  const n = pointCount.value
  if (n === 0 || tMax.value <= tMin.value) return []
  const count = Math.max(2, Math.min(8, Math.floor(plotWidth.value / 110)))
  const out: { x: number; text: string }[] = []
  const span = tMax.value - tMin.value
  const multiDay = span > 36 * 3600000
  // 实时曲线整段只有两分钟,只到分钟的标签会是一排一模一样的「18:02」——
  // 跨度不到十分钟就带上秒。
  const withSeconds = span < 10 * 60000
  for (let i = 0; i < count; i++) {
    const t = tMin.value + (span * i) / (count - 1)
    out.push({ x: xAt(t), text: axisTime(t, multiDay, withSeconds) })
  }
  return out
})

function pad(n: number): string {
  return String(n).padStart(2, '0')
}
function axisTime(t: number, withDay: boolean, withSeconds = false): string {
  const d = new Date(t)
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}${withSeconds ? `:${pad(d.getSeconds())}` : ''}`
  return withDay ? `${d.getMonth() + 1}/${d.getDate()} ${hm}` : hm
}
function fullTime(t: number): string {
  const d = new Date(t)
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

// ---------- 悬停 ----------

/** 记住的是时间不是下标:数据刷新(实时曲线每 2 秒来一个点)时按时间重找最近点。 */
const hoverT = ref<number | null>(null)

const hoverIndex = computed(() => {
  if (hoverT.value === null || pointCount.value === 0) return null
  let best = -1
  let bestD = Infinity
  for (let i = 0; i < pointCount.value; i++) {
    const t = times.value[i]!
    if (!Number.isFinite(t)) continue
    const d = Math.abs(t - hoverT.value)
    if (d < bestD) {
      bestD = d
      best = i
    }
  }
  return best >= 0 ? best : null
})

/** 悬停处离最近的采样太远 —— 那是空档,不能把最近那个点的值当成这里的值。 */
const hoverInGap = computed(() => {
  if (hoverIndex.value === null || hoverT.value === null) return false
  const t = times.value[hoverIndex.value]!
  return Math.abs(t - hoverT.value) > gapThreshold.value / 2 && gapThreshold.value !== Infinity
})

function onMove(event: MouseEvent) {
  if (pointCount.value === 0) return
  const svg = event.currentTarget as SVGSVGElement
  const rect = svg.getBoundingClientRect()
  // SVG 现在就是像素尺寸,不再有 viewBox 换算;只要把像素映射回时间。
  const px = event.clientX - rect.left
  const ratio = Math.max(0, Math.min(1, (px - padding.left) / plotWidth.value))
  hoverT.value = tMin.value + ratio * (tMax.value - tMin.value)
}

const hoverX = computed(() => {
  if (hoverIndex.value === null) return 0
  return hoverInGap.value && hoverT.value !== null ? xAt(hoverT.value) : xAt(times.value[hoverIndex.value]!)
})

/** 提示框按像素定位,靠右时翻到左侧,不越出容器。 */
const tooltipStyle = computed(() => {
  if (hoverIndex.value === null) return {}
  const x = hoverX.value
  const flip = x > width.value * 0.6
  return {
    left: `${x}px`,
    top: '6px',
    transform: flip ? 'translateX(calc(-100% - 10px))' : 'translateX(10px)',
  }
})

watch(
  () => props.labels.length,
  () => {
    // 点数变了(刷新 / 切换范围)但鼠标还停着:hoverIndex 按时间重算,这里什么都不用做。
    // 只在数据整个换掉、原时间已不在范围内时清掉悬停。
    if (hoverT.value !== null && (hoverT.value < tMin.value || hoverT.value > tMax.value)) hoverT.value = null
  },
)

const unit = computed(() => props.unitHint ?? '')
const defaultFormat = (v: number) => formatBytes(v)
</script>

<template>
  <div ref="root" class="chart-root">
    <div v-if="pointCount === 0" class="chart-empty" :style="{ height: `${height}px` }">暂无监控数据</div>
    <div v-else class="chart-wrap">
      <svg
        :width="width"
        :height="height"
        class="chart-svg"
        role="img"
        @mousemove="onMove"
        @mouseleave="hoverT = null"
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

        <path v-for="p in paths" :key="p.name" :d="p.d" class="line" :style="{ stroke: p.color }" />
        <circle
          v-for="(lp, i) in lonePoints"
          :key="`lp${i}`"
          :cx="lp.x"
          :cy="lp.y"
          r="2.5"
          :style="{ fill: lp.color }"
        />

        <template v-if="hoverIndex !== null">
          <line :x1="hoverX" :x2="hoverX" :y1="padding.top" :y2="padding.top + plotHeight" class="crosshair" />
          <template v-if="!hoverInGap">
            <circle
              v-for="p in series"
              :key="p.name"
              v-show="p.values[hoverIndex] != null"
              :cx="hoverX"
              :cy="yAt(p.values[hoverIndex] ?? 0)"
              r="4"
              class="marker"
              :style="{ fill: p.color }"
            />
          </template>
        </template>

        <g class="axis-text">
          <text v-for="tick in yTicks" :key="`y-${tick.value}`" :x="padding.left - 8" :y="tick.y + 4" text-anchor="end">
            {{ (format ?? defaultFormat)(tick.value) }}
          </text>
          <text
            v-for="(l, i) in xLabels"
            :key="`x-${i}`"
            :x="l.x"
            :y="height - 7"
            :text-anchor="i === 0 ? 'start' : i === xLabels.length - 1 ? 'end' : 'middle'"
          >
            {{ l.text }}
          </text>
        </g>
      </svg>

      <div v-if="hoverIndex !== null" class="tooltip" :style="tooltipStyle">
        <div class="tooltip-time">
          {{ fullTime(hoverInGap && hoverT !== null ? hoverT : times[hoverIndex]!) }}
          <span v-if="unit" class="tooltip-unit">{{ unit }}</span>
        </div>
        <div v-if="hoverInGap" class="tooltip-gap">这一段没有采样</div>
        <div v-for="s in series" v-else :key="s.name" class="tooltip-row">
          <span class="dot" :style="{ background: s.color }" />
          <span>{{ s.name }}</span>
          <b class="lb-tabular">{{ s.values[hoverIndex] == null ? '无采样' : format(s.values[hoverIndex] as number) }}</b>
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
  position: relative;
  width: 100%;
  min-width: 0;
}

.chart-wrap {
  position: relative;
}

.chart-svg {
  display: block;
  overflow: visible;
}

.grid line {
  stroke: var(--sep2);
  stroke-width: 1;
}

.line {
  fill: none;
  stroke-width: 2.2;
  stroke-linejoin: round;
  stroke-linecap: round;
}

.crosshair {
  stroke: var(--text3);
  stroke-width: 1;
  stroke-dasharray: 2 2;
}

.marker {
  stroke: var(--surface);
  stroke-width: 2;
}

.axis-text text {
  fill: var(--text3);
  font-size: 11px;
  font-variant-numeric: tabular-nums;
}

.tooltip {
  position: absolute;
  pointer-events: none;
  background: var(--surface);
  border: 1px solid var(--sep);
  border-radius: 10px;
  padding: 8px 11px;
  font-size: 12.5px;
  box-shadow: var(--shadow-lg);
  white-space: nowrap;
  z-index: 2;
  color: var(--text);
}

.tooltip-time {
  color: var(--text3);
  font-size: 11px;
  margin-bottom: 4px;
  font-variant-numeric: tabular-nums;
}
.tooltip-unit {
  margin-left: 6px;
}
.tooltip-gap {
  color: var(--text3);
}

.tooltip-row {
  display: flex;
  align-items: center;
  gap: 6px;
}

.tooltip-row b {
  margin-left: auto;
  padding-left: 12px;
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
  margin-top: 6px;
  font-size: 12px;
  color: var(--text3);
}

.legend-item {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.chart-empty {
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--text3);
  font-size: 13px;
}
</style>
