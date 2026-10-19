<script setup lang="ts">
import { computed } from 'vue'
import type { ProbeSummary } from '@/api/client'
import ProbeDots from './ProbeDots.vue'
import {
  displayName,
  familyText,
  httpCodeText,
  peerSourceText,
  probeMuted,
  probeTone,
  probeValue,
  protoChip,
  rateName,
  rateOf,
  rateText,
  rateTitle,
  targetTitle,
} from './probeView'

/**
 * 探测的一行:名称 · 协议 · 地址族 · 最近一次的延迟,下面是点阵与 1h / 24h 的丢包率(ICMP)、
 * 连接失败率(TCP 建连)、握手无应答率(TCP 握手近似)、TCPing 丢包率或 Ping 丢包率(Boce)。出站与入站共用 ——
 * 规则都在 probeView.ts。Boce 的一行带「Boce」标记,名字是拨测点(地区 + 运营商,接口原样)。
 *
 * compact:资产卡片上入站 / 出站并排的半宽一列。只留名字与延迟,协议、端口、地址族、来源
 * 收进名字的悬停说明 —— 比例那一行照旧按 metric 起名(丢包 / 失败率 / 无应答 / TCPing 丢包 / Ping 丢包 /
 * 请求失败),少了它,同一列里的两个百分比就分不清是哪一种口径。
 *
 * aliases 是这台节点卡片上的自定义显示名称(只改展示):有别名时显示别名,悬停与键盘聚焦仍能看到
 * 原始名称、来源与测法 —— 两个项目起了同一个别名也分得出。协议、来源、口径名都不被别名覆盖。
 */
const props = defineProps<{ p: ProbeSummary; compact?: boolean; aliases?: Record<string, string> | null }>()

const shownName = computed(() => (props.p.source === 'PEER' && !props.compact && !props.aliases?.[String(props.p.id)]
  ? props.p.label
  : displayName(props.p, props.aliases)))
const aliased = computed(() => !!props.aliases?.[String(props.p.id)]?.trim())

const labelTitle = computed(() => {
  const p = props.p
  const via = p.source === 'BOCE' ? 'Boce 第三方拨测点' : p.source === 'PEER' ? `自建节点 ${peerSourceText(p)}`
    : p.direction === 'INGRESS' ? '主控探测' : '节点本机'
  const head = aliased.value ? `显示名称「${shownName.value}」,原始名称:` : ''
  if (!props.compact && !aliased.value) return targetTitle(p)
  const fam = p.source === 'BOCE' ? '' : ` · ${familyText(p.family)}`
  return `${head}${targetTitle(p)}\n${via} · ${protoChip(p)}${fam}`
})
</script>

<template>
  <div
    class="pr"
    :class="{ 'pr--compact': compact }"
    :data-testid="`probe-row-${p.id}`"
    :data-direction="p.direction"
    :data-source="p.source"
  >
    <div class="pr__line">
      <!-- 名字可能被截断、可能是别名:悬停看完整说明(含原始名称),键盘聚焦时整段展开、读屏念完整说明。 -->
      <span
        class="pr__label lb-ellipsis"
        :title="labelTitle"
        :aria-label="labelTitle"
        tabindex="0"
        data-testid="probe-row-name"
      >{{ shownName }}</span>
      <template v-if="!compact">
        <span v-if="p.source === 'BOCE'" class="lb-chip lb-chip--purple pr__chip" title="Boce 第三方拨测点">Boce</span>
        <span
          v-else-if="p.source === 'PEER'"
          class="lb-chip lb-chip--brand pr__chip"
          :title="`自建节点拨测:在「${p.peer_source?.name ?? ''}」上测这个入口`"
        >{{ peerSourceText(p) }}</span>
        <span v-else-if="p.direction === 'INGRESS'" class="lb-chip pr__chip" title="主控探测">主控</span>
        <span class="lb-chip pr__chip">{{ protoChip(p) }}</span>
        <span v-if="p.source !== 'BOCE'" class="lb-chip pr__chip">{{ familyText(p.family) }}</span>
      </template>
      <span
        v-if="httpCodeText(p)"
        class="pr__code lb-tabular"
        :class="{ 'pr__tone--warn': (p.last?.http_status ?? 0) >= 500 }"
        :title="(p.last?.http_status ?? 0) >= 500 ? '取得了响应,但服务返回异常状态(网络是通的)' : '最近一次的 HTTP 状态码'"
      >{{ httpCodeText(p) }}</span>
      <span class="pr__rtt lb-tabular" :class="`pr__tone--${probeTone(p.state)}`" :title="p.state_text">
        {{ probeValue(p) }}
      </span>
    </div>
    <ProbeDots :dots="p.dots" :seconds="p.dot_seconds" :metric="p.metric" :muted="probeMuted(p.state)" :dense="compact" />
    <div class="pr__meta lb-tabular">
      <span :title="rateTitle(p.h1, p.metric, '近 1 小时')">1h {{ rateName(p.metric) }} {{ rateText(rateOf(p.h1, p.metric)) }}</span>
      <span :title="rateTitle(p.h24, p.metric, '近 24 小时')">24h {{ rateText(rateOf(p.h24, p.metric)) }}</span>
      <span
        v-if="p.state !== 'OK'"
        class="pr__state"
        :class="[`pr__tone--${probeTone(p.state)}`, { 'lb-ellipsis': compact }]"
        :title="p.state_text"
      >{{ p.state_text }}</span>
    </div>
  </div>
</template>

<style scoped>
.pr {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}
.pr__line {
  display: flex;
  align-items: center;
  gap: var(--sp-1);
  min-width: 0;
}
.pr__label {
  flex: 1;
  min-width: 0;
  font-size: var(--fs-xs);
  color: var(--text2);
}
.pr__label:focus-visible {
  outline: 2px solid var(--brand);
  outline-offset: 1px;
  border-radius: 2px;
  white-space: normal;
  overflow: visible;
  word-break: break-all;
}
.pr__chip {
  flex: none;
}
.pr__code {
  flex: none;
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.pr__rtt {
  flex: none;
  font-size: var(--fs-xs);
  font-weight: 600;
  color: var(--text);
}
.pr__meta {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-2);
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.pr--compact {
  gap: 3px;
}
.pr--compact .pr__meta {
  column-gap: var(--sp-1);
  row-gap: 0;
}
.pr--compact .pr__state {
  flex-basis: 100%;
}
.pr__state {
  min-width: 0;
}
.pr__tone--bad {
  color: var(--bad);
}
.pr__tone--warn {
  color: var(--warn);
}
.pr__tone--muted {
  color: var(--text3);
}
</style>
