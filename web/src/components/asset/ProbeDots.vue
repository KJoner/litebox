<script setup lang="ts">
import { computed } from 'vue'
import type { ProbeBucket } from '@/api/client'
import { formatTime } from '@/utils/format'

/**
 * 探测点阵:一个点一个时间窗口。
 *
 * 六种状态各有各的画法,**没有样本的窗口画成空心虚线点,绝不按正常(绿)画** ——
 * 「没有数据 = 正常」是这类面板最常见的骗局。颜色之外还有形状:失败是实心方块、
 * 部分失败是半填、DNS 失败是菱形、未执行(关机)是灰色实心,色觉障碍下也分得出。
 */
const props = defineProps<{
  dots: ProbeBucket[]
  /** 每个点的宽度(秒),写进悬停说明。 */
  seconds: number
  /** 整行暂停 / 失效时降成灰色(历史仍然画,但不再暗示「此刻」)。 */
  muted?: boolean
}>()

const STATE_TEXT: Record<ProbeBucket['state'], string> = {
  OK: '全部成功',
  PARTIAL: '部分失败',
  FAIL: '全部失败',
  DNS: 'DNS 解析失败(未发起连接)',
  SKIP: '未执行(节点关机等)',
  NONE: '没有样本',
}

function title(d: ProbeBucket): string {
  const parts = [`${formatTime(d.at)} 起 ${Math.round(props.seconds / 60)} 分钟:${STATE_TEXT[d.state]}`]
  if (d.attempts) parts.push(`TCP 连接 ${d.attempts} 次,失败 ${d.failures} 次`)
  if (d.rtt_avg_ms != null) parts.push(`平均 ${d.rtt_avg_ms.toFixed(1)} ms`)
  if (d.dns_errors) parts.push(`DNS 失败 ${d.dns_errors} 次`)
  if (d.skipped) parts.push(`未执行 ${d.skipped} 次`)
  return parts.join(' · ')
}

/** 给读屏的一句话:不念 30 个点,只念合计。 */
const summary = computed(() => {
  const c = { OK: 0, PARTIAL: 0, FAIL: 0, DNS: 0, SKIP: 0, NONE: 0 }
  for (const d of props.dots) c[d.state]++
  const minutes = Math.round((props.dots.length * props.seconds) / 60)
  return `最近 ${minutes} 分钟 ${props.dots.length} 个窗口:成功 ${c.OK},部分失败 ${c.PARTIAL},失败 ${c.FAIL},DNS 失败 ${c.DNS},未执行 ${c.SKIP},无样本 ${c.NONE}`
})
</script>

<template>
  <div class="pd" :class="{ 'pd--muted': muted }" role="img" :aria-label="summary">
    <span
      v-for="d in dots"
      :key="d.at"
      class="pd__dot"
      :class="`pd__dot--${d.state.toLowerCase()}`"
      :title="title(d)"
      :data-state="d.state"
    />
  </div>
</template>

<style scoped>
.pd {
  display: grid;
  grid-auto-flow: column;
  grid-auto-columns: minmax(3px, 1fr);
  gap: 2px;
  align-items: center;
  height: 12px;
  min-width: 0;
}
.pd__dot {
  display: block;
  height: 10px;
  border-radius: 2px;
  background: var(--ok);
}
.pd__dot--partial {
  background: linear-gradient(to top, var(--warn) 50%, var(--warn-bg) 50%);
  box-shadow: inset 0 0 0 1px var(--warn);
}
.pd__dot--fail {
  background: var(--bad);
}
.pd__dot--dns {
  background: var(--bad-bg);
  box-shadow: inset 0 0 0 1px var(--bad);
  transform: scaleY(0.7);
}
.pd__dot--skip {
  background: var(--fill2);
}
.pd__dot--none {
  background: transparent;
  box-shadow: inset 0 0 0 1px var(--sep);
  border: 1px dashed var(--fill2);
}
.pd--muted .pd__dot--ok,
.pd--muted .pd__dot--fail,
.pd--muted .pd__dot--partial {
  filter: grayscale(1);
  opacity: 0.55;
}
</style>
