<script setup lang="ts">
import type { ProbeSummary } from '@/api/client'
import ProbeDots from './ProbeDots.vue'
import {
  familyText,
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
 * 探测的一行:名称 · 协议 · 地址族 · 最近一次的延迟,下面是点阵与 1h / 24h 的丢包率(ICMP)
 * 或失败率(TCP)。出站与入口共用 —— 规则都在 probeView.ts。
 */
defineProps<{ p: ProbeSummary }>()
</script>

<template>
  <div class="pr" :data-testid="`probe-row-${p.id}`" :data-direction="p.direction">
    <div class="pr__line">
      <span class="pr__label lb-ellipsis" :title="targetTitle(p)">{{ p.label }}</span>
      <span class="lb-chip pr__chip">{{ protoChip(p) }}</span>
      <span class="lb-chip pr__chip">{{ familyText(p.family) }}</span>
      <span class="pr__rtt lb-tabular" :class="`pr__tone--${probeTone(p.state)}`" :title="p.state_text">
        {{ probeValue(p) }}
      </span>
    </div>
    <ProbeDots :dots="p.dots" :seconds="p.dot_seconds" :protocol="p.protocol" :muted="probeMuted(p.state)" />
    <div class="pr__meta lb-tabular">
      <span :title="rateTitle(p.h1, p.protocol, '近 1 小时')">1h {{ rateName(p.protocol) }} {{ rateText(rateOf(p.h1, p.protocol)) }}</span>
      <span :title="rateTitle(p.h24, p.protocol, '近 24 小时')">24h {{ rateText(rateOf(p.h24, p.protocol)) }}</span>
      <span v-if="p.state !== 'OK'" class="pr__state" :class="`pr__tone--${probeTone(p.state)}`">{{ p.state_text }}</span>
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
.pr__chip {
  flex: none;
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
