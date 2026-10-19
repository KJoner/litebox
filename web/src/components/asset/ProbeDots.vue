<script setup lang="ts">
import { computed } from 'vue'
import type { ProbeBucket, ProbeMetric } from '@/api/client'
import { formatTime } from '@/utils/format'

/**
 * 探测点阵:一个点一个时间窗口。
 *
 * 每种状态各有各的画法,**没有样本的窗口画成空心虚线点,绝不按正常(绿)画** ——
 * 「没有数据 = 正常」是这类面板最常见的骗局。颜色之外还有形状:失败是实心方块、
 * 部分失败是半填、DNS 失败是菱形、未执行(关机)是灰色实心、采集失败是灰色斜纹、
 * 工具不可用 / 目标受限是空心琥珀框,色觉障碍下也分得出。
 * 采集失败与工具不可用**不按失败画**:那几轮根本没有发出去,目标那头是什么样子并不知道。
 * Boce 的拨测点异常、接口异常、未取得结果同理:第三方那一侧的问题,画成灰色点线框,不算这台 VPS 的丢包。
 */
const props = defineProps<{
  dots: ProbeBucket[]
  /** 每个点的宽度(秒),写进悬停说明。 */
  seconds: number
  /** 悬停说明按口径写:ICMP 按包(丢包)、TCP 建连按建连(失败)、握手近似按 SYN(无应答)。 */
  metric?: ProbeMetric
  /** 整行暂停 / 失效时降成灰色(历史仍然画,但不再暗示「此刻」)。 */
  muted?: boolean
  /** 卡片里半宽的一列:点之间只留 1px、点矮一些,30 个点在 120px 里也放得下。 */
  dense?: boolean
}>()


const STATE_TEXT: Record<ProbeBucket['state'], string> = {
  OK: '全部成功',
  PARTIAL: '部分失败 / 部分丢包 / 部分无应答',
  FAIL: '全部失败',
  DNS: 'DNS 解析失败(未发起)',
  SSH: '采集失败(面板连不上这台机器,不是丢包)',
  TOOL: '探测工具不可用(未发起)',
  BLOCKED: '目标受限(未发起)',
  VANTAGE: 'Boce 拨测点异常(不是目标不通)',
  API: 'Boce 接口异常(不是目标不通)',
  EXPIRED: '任务没有取得结果',
  SKIP: '未执行(节点关机、预算保护等)',
  NONE: '没有样本',
}

function title(d: ProbeBucket): string {
  const parts = [`${formatTime(d.at)} 起 ${Math.round(props.seconds / 60)} 分钟:${STATE_TEXT[d.state]}`]
  if (d.attempts) {
    if (props.metric === 'LOSS') parts.push(`ICMP ${d.attempts} 轮,发 ${d.sent} 个包、收到 ${d.received} 个`)
    else if (props.metric === 'SYN') parts.push(`握手 ${d.attempts} 轮,发出 ${d.sent} 个 SYN、有应答 ${d.received} 个`)
    else if (props.metric === 'BOCE') parts.push(`TCPing ${d.attempts} 轮,发 ${d.sent} 次、成功 ${d.received} 次`)
    else if (props.metric === 'BOCE_PING') parts.push(`Ping ${d.attempts} 轮,发 ${d.sent} 个包、收到 ${d.received} 个`)
    else parts.push(`TCP 连接 ${d.attempts} 次,失败 ${d.failures} 次`)
  }
  if (d.rtt_avg_ms != null) parts.push(`平均 ${d.rtt_avg_ms.toFixed(1)} ms`)
  if (d.dns_errors) parts.push(`DNS 失败 ${d.dns_errors} 次`)
  if (d.ssh_errors) parts.push(`采集失败 ${d.ssh_errors} 次`)
  if (d.tool_errors) parts.push(`工具不可用 ${d.tool_errors} 次`)
  if (d.blocked) parts.push(`目标受限 ${d.blocked} 次`)
  if (d.vantage_errors) parts.push(`拨测点异常 ${d.vantage_errors} 次`)
  if (d.api_errors) parts.push(`接口异常 ${d.api_errors} 次`)
  if (d.expired) parts.push(`未取得结果 ${d.expired} 次`)
  if (d.skipped) parts.push(`未执行 ${d.skipped} 次`)
  return parts.join(' · ')
}

/** 给读屏的一句话:不念 30 个点,只念合计。 */
const summary = computed(() => {
  const c: Record<ProbeBucket['state'], number> = {
    OK: 0, PARTIAL: 0, FAIL: 0, DNS: 0, SSH: 0, TOOL: 0, BLOCKED: 0, VANTAGE: 0, API: 0, EXPIRED: 0, SKIP: 0, NONE: 0,
  }
  for (const d of props.dots) c[d.state]++
  const minutes = Math.round((props.dots.length * props.seconds) / 60)
  const parts = [`成功 ${c.OK}`, `部分失败 ${c.PARTIAL}`, `失败 ${c.FAIL}`, `DNS 失败 ${c.DNS}`]
  if (c.SSH) parts.push(`采集失败 ${c.SSH}`)
  if (c.TOOL + c.BLOCKED) parts.push(`未发起 ${c.TOOL + c.BLOCKED}`)
  if (c.VANTAGE + c.API + c.EXPIRED) parts.push(`第三方异常 ${c.VANTAGE + c.API + c.EXPIRED}`)
  parts.push(`未执行 ${c.SKIP}`, `无样本 ${c.NONE}`)
  return `最近 ${minutes} 分钟 ${props.dots.length} 个窗口:${parts.join(',')}`
})
</script>

<template>
  <div class="pd" :class="{ 'pd--muted': muted, 'pd--dense': dense }" role="img" :aria-label="summary">
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
.pd--dense {
  grid-auto-columns: minmax(2px, 1fr);
  gap: 1px;
  height: 10px;
}
.pd--dense .pd__dot {
  height: 8px;
  border-radius: 1px;
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
.pd__dot--ssh {
  background: repeating-linear-gradient(135deg, var(--fill2) 0 2px, transparent 2px 4px);
  box-shadow: inset 0 0 0 1px var(--text3);
}
.pd__dot--tool,
.pd__dot--blocked {
  background: var(--warn-bg);
  box-shadow: inset 0 0 0 1px var(--warn);
  transform: scaleY(0.6);
}
.pd__dot--vantage,
.pd__dot--api,
.pd__dot--expired {
  background: transparent;
  box-shadow: inset 0 0 0 1px var(--text3);
  border: 1px dotted var(--text3);
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
