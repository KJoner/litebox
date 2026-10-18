<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import type { ProbeBucket, ProbeProtocol } from '@/api/client'
import { formatTime } from '@/utils/format'
import { rateText, rttText } from './probeView'

/**
 * 延迟曲线。横轴按时间等距(每个窗口一格),纵轴是窗口内的平均 RTT
 * (TCP 是成功建连的耗时,ICMP 是收到的包的往返时间,按包加权)。
 *
 *   - 窗口里一次可达都没有:曲线断开,不补 0、不插值;
 *   - 失败 / 部分丢包 / DNS 失败 / 采集失败或工具不可用 / 未执行各画一枚底部记号
 *     (红 / 琥珀 / 橙 / 灰斜纹 / 灰),失败点在曲线断开的地方也看得见;
 *   - 宽度由 ResizeObserver 量容器像素,不靠 viewBox 拉伸 —— 拉伸会让鼠标位置与读数对不上。
 */
const props = defineProps<{ buckets: ProbeBucket[]; height?: number; protocol?: ProbeProtocol }>()

const box = ref<HTMLElement | null>(null)
const width = ref(600)
let ro: ResizeObserver | null = null
onMounted(() => {
  if (!box.value) return
  width.value = box.value.clientWidth || 600
  ro = new ResizeObserver((entries) => {
    const w = entries[0]?.contentRect.width
    if (w) width.value = w
  })
  ro.observe(box.value)
})
onBeforeUnmount(() => ro?.disconnect())

const H = computed(() => props.height ?? 180)
const PAD = { l: 44, r: 8, t: 10, b: 22 }
const plotW = computed(() => Math.max(width.value - PAD.l - PAD.r, 10))
const plotH = computed(() => H.value - PAD.t - PAD.b)

const maxY = computed(() => {
  let m = 0
  for (const b of props.buckets) if (b.rtt_max_ms != null && b.rtt_avg_ms != null) m = Math.max(m, b.rtt_avg_ms)
  if (m <= 0) return 10
  const pow = 10 ** Math.floor(Math.log10(m))
  return Math.ceil((m * 1.15) / pow) * pow
})

function x(i: number): number {
  const n = props.buckets.length
  return PAD.l + (n <= 1 ? plotW.value / 2 : (i / (n - 1)) * plotW.value)
}
function y(v: number): number {
  return PAD.t + plotH.value - (v / maxY.value) * plotH.value
}

/** 连续有值的窗口连成一段;中间缺一个就断开。 */
const segments = computed(() => {
  const out: string[] = []
  let cur: string[] = []
  props.buckets.forEach((b, i) => {
    if (b.rtt_avg_ms == null) {
      if (cur.length) out.push(cur.join(' '))
      cur = []
      return
    }
    cur.push(`${cur.length ? 'L' : 'M'}${x(i).toFixed(1)},${y(b.rtt_avg_ms).toFixed(1)}`)
  })
  if (cur.length) out.push(cur.join(' '))
  return out
})

/** 单独一个有值的窗口画成点,否则一段长度为 0 的线看不见。 */
const lonely = computed(() =>
  props.buckets
    .map((b, i) => ({ b, i }))
    .filter(
      ({ b, i }) =>
        b.rtt_avg_ms != null &&
        props.buckets[i - 1]?.rtt_avg_ms == null &&
        props.buckets[i + 1]?.rtt_avg_ms == null,
    ),
)

const marks = computed(() =>
  props.buckets
    .map((b, i) => ({ b, i }))
    .filter(
      ({ b }) =>
        b.failures > 0 ||
        b.lossy > 0 ||
        b.dns_errors > 0 ||
        (b.attempts === 0 && (b.ssh_errors > 0 || b.tool_errors > 0 || b.blocked > 0 || b.skipped > 0)),
    ),
)

function markClass(b: ProbeBucket) {
  if (b.failures > 0) return 'pc__mark--fail'
  if (b.lossy > 0) return 'pc__mark--loss'
  if (b.dns_errors > 0) return 'pc__mark--dns'
  if (b.ssh_errors > 0 || b.tool_errors > 0 || b.blocked > 0) return 'pc__mark--nosend'
  return 'pc__mark--skip'
}

const ticks = computed(() => [0, maxY.value / 2, maxY.value])
const xLabels = computed(() => {
  const n = props.buckets.length
  if (n < 2) return []
  return [0, Math.floor(n / 2), n - 1].map((i) => ({ i, text: formatTime(props.buckets[i]!.at).slice(5) }))
})

const hover = ref<number | null>(null)
function onMove(e: MouseEvent) {
  const rect = (e.currentTarget as SVGElement).getBoundingClientRect()
  const px = e.clientX - rect.left
  const n = props.buckets.length
  if (!n) return
  const i = Math.round(((px - PAD.l) / plotW.value) * (n - 1))
  hover.value = Math.max(0, Math.min(n - 1, i))
}
const hoverText = computed(() => {
  if (hover.value == null) return ''
  const b = props.buckets[hover.value]
  if (!b) return ''
  const parts = [formatTime(b.at)]
  if (b.attempts) {
    if (props.protocol === 'ICMP') parts.push(`发 ${b.sent} 个包 · 收到 ${b.received} · 丢包 ${rateText(b.loss_rate)}`)
    else parts.push(`连接 ${b.attempts} 次 · 失败 ${b.failures}`)
  }
  if (b.rtt_avg_ms != null) parts.push(`平均 ${rttText(b.rtt_avg_ms)}(${rttText(b.rtt_min_ms)} ~ ${rttText(b.rtt_max_ms)})`)
  if (b.dns_errors) parts.push(`DNS 失败 ${b.dns_errors}`)
  if (b.ssh_errors) parts.push(`采集失败 ${b.ssh_errors}(不是丢包)`)
  if (b.tool_errors) parts.push(`工具不可用 ${b.tool_errors}`)
  if (b.blocked) parts.push(`目标受限 ${b.blocked}`)
  if (b.skipped) parts.push(`未执行 ${b.skipped}`)
  if (!b.attempts && !b.dns_errors && !b.skipped && !b.ssh_errors && !b.tool_errors && !b.blocked) {
    parts.push('这一段没有样本')
  }
  return parts.join(' · ')
})
</script>

<template>
  <div ref="box" class="pc">
    <svg
      :width="width"
      :height="H"
      role="img"
      :aria-label="protocol === 'ICMP' ? 'ICMP 往返时间曲线' : 'TCP 建连时间曲线'"
      @mousemove="onMove"
      @mouseleave="hover = null"
    >
      <g class="pc__grid">
        <line v-for="t in ticks" :key="t" :x1="PAD.l" :x2="PAD.l + plotW" :y1="y(t)" :y2="y(t)" />
      </g>
      <g class="pc__axis">
        <text v-for="t in ticks" :key="t" :x="PAD.l - 6" :y="y(t) + 3" text-anchor="end">{{ Math.round(t) }}</text>
        <text
          v-for="(l, k) in xLabels"
          :key="l.i"
          :x="x(l.i)"
          :y="H - 6"
          :text-anchor="k === 0 ? 'start' : k === xLabels.length - 1 ? 'end' : 'middle'"
        >
          {{ l.text }}
        </text>
      </g>
      <path v-for="(d, k) in segments" :key="k" class="pc__line" :d="d" />
      <circle v-for="p in lonely" :key="p.i" class="pc__point" :cx="x(p.i)" :cy="y(p.b.rtt_avg_ms ?? 0)" r="2.5" />
      <rect
        v-for="m in marks"
        :key="`m${m.i}`"
        class="pc__mark"
        :class="markClass(m.b)"
        :x="x(m.i) - 2"
        :y="PAD.t + plotH - 6"
        width="4"
        height="6"
      />
      <line v-if="hover != null" class="pc__cursor" :x1="x(hover)" :x2="x(hover)" :y1="PAD.t" :y2="PAD.t + plotH" />
    </svg>
    <div class="pc__tip lb-tabular" aria-live="polite">{{ hoverText || '悬停查看每个时间窗口的计数与延迟;曲线断开处没有可达的样本' }}</div>
  </div>
</template>

<style scoped>
.pc {
  width: 100%;
  min-width: 0;
}
.pc svg {
  display: block;
}
.pc__grid line {
  stroke: var(--sep2);
  stroke-width: 1;
}
.pc__axis text {
  fill: var(--text3);
  font-size: var(--fs-2xs);
  font-variant-numeric: tabular-nums;
}
.pc__line {
  fill: none;
  stroke: var(--brand);
  stroke-width: 1.6;
  stroke-linejoin: round;
}
.pc__point {
  fill: var(--brand);
}
.pc__mark--fail {
  fill: var(--bad);
}
.pc__mark--loss {
  fill: var(--warn);
  opacity: 0.6;
}
.pc__mark--dns {
  fill: var(--warn);
}
.pc__mark--nosend {
  fill: var(--text3);
  opacity: 0.5;
}
.pc__mark--skip {
  fill: var(--fill2);
}
.pc__cursor {
  stroke: var(--text3);
  stroke-dasharray: 3 3;
}
.pc__tip {
  min-height: 18px;
  margin-top: 4px;
  font-size: var(--fs-xs);
  color: var(--text3);
}
</style>
