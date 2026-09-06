<script setup lang="ts">
import { computed, ref } from 'vue'
import { formatBytes, formatUTCDay } from '@/utils/format'

/**
 * 轻量内联 SVG 趋势图。不引图表库。
 *
 * 关键规则:**缺失的点传 null,不补 0、不插值。**
 * 补 0 会让当天看起来「没人用」,插值会凭空造出一个从未存在的数字。
 * 折线用灰虚线跨过缺口(面积不填),柱状用空心虚线柱。
 *
 * V18:线 --brand 2.3px round + lb-draw 入场;面积 --brand 28%→0 渐变;
 * 峰值点 r5 描 --surface;柱圆角 3,峰值 --brand、其余 --brand-bg,逐柱 lb-growy;
 * 网格 --sep2;X 轴标签由调用方放在 #axis 插槽。
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
    height: 150,
    markMax: true,
    format: formatBytes,
    labelFormat: formatUTCDay,
  },
)

const W = 800

// 渐变 id 要按实例唯一:同一页上两张图共用一个 id,后一张会引到前一张的 defs。
let seq = 0
const uid = `lb-spark-${++seq}`

const max = computed(() => {
  const vals = props.points.map((p) => p.value).filter((v): v is number => v !== null)
  return vals.length ? Math.max(...vals) : 0
})

const top = 8
const bottom = computed(() => props.height - 10)

const scaled = computed(() => {
  const n = Math.max(props.points.length - 1, 1)
  return props.points.map((p, i) => ({
    ...p,
    x: (i / n) * W,
    y:
      p.value === null || max.value === 0
        ? null
        : bottom.value - (p.value / max.value) * (bottom.value - top),
  }))
})

/** 把折线切成若干连续段,缺口不参与 polyline。 */
const segments = computed(() => {
  const segs: { x: number; y: number }[][] = []
  let cur: { x: number; y: number }[] = []
  for (const p of scaled.value) {
    if (p.y === null) {
      if (cur.length > 1) segs.push(cur)
      cur = []
    } else {
      cur.push({ x: p.x, y: p.y })
    }
  }
  if (cur.length > 1) segs.push(cur)
  return segs
})

const segmentPoints = computed(() => segments.value.map((s) => s.map((p) => `${p.x},${p.y}`).join(' ')))

/** 每段下面的面积:沿折线走一遍再沿底边回来。缺口处不填 —— 那里没有数据。 */
const areaPaths = computed(() =>
  segments.value.map((s) => {
    const first = s[0]
    const last = s[s.length - 1]
    const line = s.map((p, i) => `${i === 0 ? 'M' : 'L'}${p.x},${p.y}`).join(' ')
    return `${line} L${last.x},${bottom.value} L${first.x},${bottom.value} Z`
  }),
)

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
// 「当天是 0」在图上一个是空心柱一个是贴底的实柱,但那点差别在 150px 高的
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
      <defs>
        <linearGradient :id="uid" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stop-color="var(--brand)" stop-opacity="0.28" />
          <stop offset="1" stop-color="var(--brand)" stop-opacity="0" />
        </linearGradient>
      </defs>

      <line
        v-for="f in [0.25, 0.5, 0.75]"
        :key="f"
        x1="0"
        :y1="props.height * f"
        :x2="W"
        :y2="props.height * f"
        stroke="var(--sep2)"
      />
      <line x1="0" :y1="bottom" :x2="W" :y2="bottom" stroke="var(--sep)" />

      <template v-if="props.type === 'bar'">
        <rect
          v-for="(p, i) in scaled"
          :key="i"
          class="lb-spark__bar"
          :class="{ 'lb-spark__bar--gap': p.y === null }"
          :x="p.x - barW / 2"
          :y="p.y === null ? bottom - 8 : p.y"
          :width="barW"
          :height="p.y === null ? 8 : bottom - p.y"
          rx="3"
          :fill="p.y === null ? 'var(--fill)' : p.value === max ? 'var(--brand)' : 'var(--brand-bg)'"
          :stroke="p.y === null ? 'var(--text3)' : 'none'"
          :stroke-dasharray="p.y === null ? '2 2' : undefined"
          stroke-width="1"
          vector-effect="non-scaling-stroke"
          :style="{ animationDelay: `${i * 18}ms` }"
        />
      </template>

      <template v-else>
        <path
          v-for="(d, i) in areaPaths"
          :key="'a' + i"
          :d="d"
          :fill="`url(#${uid})`"
          class="lb-spark__area"
        />
        <line
          v-for="(g, i) in gapLines"
          :key="'g' + i"
          :x1="g.x1"
          :y1="g.y1"
          :x2="g.x2"
          :y2="g.y2"
          stroke="var(--text3)"
          stroke-width="1.5"
          stroke-dasharray="4 4"
          vector-effect="non-scaling-stroke"
        />
        <polyline
          v-for="(s, i) in segmentPoints"
          :key="'s' + i"
          :points="s"
          fill="none"
          stroke="var(--brand)"
          stroke-width="2.3"
          stroke-linejoin="round"
          stroke-linecap="round"
          vector-effect="non-scaling-stroke"
          class="lb-spark__line"
        />
      </template>

      <circle
        v-if="maxPoint && maxPoint.y !== null"
        :cx="maxPoint.x"
        :cy="maxPoint.y"
        r="5"
        fill="var(--brand)"
        stroke="var(--surface)"
        stroke-width="2.5"
        vector-effect="non-scaling-stroke"
        class="lb-spark__peak"
      />

      <!-- 悬停标记画在最后,盖在柱子和折线之上。 -->
      <template v-if="hoverPoint">
        <line
          :x1="hoverPoint.x"
          y1="4"
          :x2="hoverPoint.x"
          :y2="bottom"
          stroke="var(--text3)"
          stroke-width="1"
          stroke-dasharray="2 2"
          vector-effect="non-scaling-stroke"
        />
        <circle
          v-if="hoverPoint.y !== null"
          :cx="hoverPoint.x"
          :cy="hoverPoint.y"
          r="4"
          fill="var(--brand)"
          stroke="var(--surface)"
          stroke-width="2"
          vector-effect="non-scaling-stroke"
          class="lb-spark__peak"
        />
      </template>
    </svg>

    <!-- 读数框跟着鼠标横向走,纵向固定在顶部:柱高会变,跟着纵向跳会读不稳。 -->
    <div v-if="hoverPoint" class="lb-spark__tip" :style="tipStyle">
      <div class="lb-spark__tip-day">{{ props.labelFormat(hoverPoint.at) }}</div>
      <div v-if="hoverPoint.value === null" class="lb-spark__tip-none">
        当天没有记录(不是 0)
      </div>
      <b v-else class="lb-tabular">{{ props.format(hoverPoint.value) }}</b>
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
  overflow: visible;
}

.lb-spark__line {
  stroke-dasharray: 1600;
  animation: lb-draw 1.4s var(--ease);
}

.lb-spark__area {
  animation: lb-fade 1.2s var(--ease);
}

/* preserveAspectRatio="none" 下圆会被拉成椭圆;peak 点不参与缩放的办法是
   把它画小一点、靠描边撑视觉 —— 这里接受轻微变形,换不用第二层 SVG。 */
.lb-spark__peak {
  animation: lb-fade 0.6s var(--ease) both;
  animation-delay: 1s;
}

.lb-spark__bar {
  transform-box: fill-box;
  transform-origin: bottom;
  animation: lb-growy 0.7s var(--ease) both;
}

.lb-spark__tip {
  position: absolute;
  top: 2px;
  z-index: 2;
  padding: 8px 11px;
  background: var(--surface);
  border: 1px solid var(--sep);
  border-radius: 10px;
  box-shadow: var(--shadow-lg);
  font-size: 12.5px;
  line-height: 1.5;
  white-space: nowrap;
  /* 读数框自己不能吃鼠标事件,否则鼠标移到它上面就触发 mouseleave,框会闪。 */
  pointer-events: none;
}

.lb-spark__tip-day {
  font-size: 11px;
  color: var(--text3);
}

.lb-spark__tip-none {
  color: var(--text3);
}
</style>
