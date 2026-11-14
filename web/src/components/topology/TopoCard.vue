<script setup lang="ts">
import { computed } from 'vue'
import type { TopoEntry, TopoTier } from '@/api/client'
import { LbIcon, type LbIconName } from '@/components/lb'
import { regionFlag } from '@/components/asset/assetModel'
import { hasMore, visibleEndpoints } from './cardMetrics'
import { egressText, endpointText, familyText, kindLabel, PENDING_TEXT, protocolLabel } from './topoText'

/**
 * 画布上的一张入口卡片(一个入口一张;同一个入口的多条地址都在这一张里)。
 *
 * 三种含义各用各的视觉,不用同一大片颜色:访问等级是左侧的小色条 + 名字;地区是国旗(背景是低透明度的水印,
 * 文字区保持清晰底色);状态是文字 + 图形 + 既有状态色。访问等级不是线路速度或质量。
 * 卡片上的地址是「可以被连的地址」,不是已验证的出口 IP。
 */
const props = defineProps<{
  entry: TopoEntry
  tier?: TopoTier
  expanded: boolean
  selected: boolean
  dimmed: boolean
  /** 被选中的边接在哪一条地址上(高亮那一行) */
  highlightEndpoint?: number
  /** 低缩放下只画名字与种类,细节看详情抽屉 */
  compact: boolean
  /** 筛选时被隐藏的上游个数(「隐藏边界」) */
  hiddenUpstream?: number
}>()

const emit = defineEmits<{
  select: []
  menu: [MouseEvent]
  toggle: []
}>()

const ICON: Record<string, LbIconName> = {
  SINGBOX: 'server',
  MIERU: 'layers',
  NGINX: 'route',
  REALM: 'route',
  EXTERNAL: 'globe',
}

const flag = computed(() => regionFlag(props.entry.region))
const rows = computed(() => visibleEndpoints(props.entry, props.expanded))
const more = computed(() => (hasMore(props.entry) ? props.entry.endpoints.length - rows.value.length : 0))

/** 等级色条:按等级高低取固定的几档颜色,不随数据变。 */
const tierTone = computed(() => {
  const lv = props.tier?.level ?? 0
  if (props.entry.access_mode === 'RESTRICTED') return 'restricted'
  if (lv >= 3) return 't3'
  if (lv === 2) return 't2'
  return 't1'
})

const statusChips = computed(() => {
  const e = props.entry
  const out: { text: string; tone: 'bad' | 'warn' | 'neutral' | 'ok' }[] = []
  if (!e.enabled) out.push({ text: '已停用', tone: 'neutral' })
  if (!e.deployed && e.kind !== 'EXTERNAL') out.push({ text: '未部署', tone: 'warn' })
  if (e.pending) out.push({ text: PENDING_TEXT[e.pending] ?? '待下发', tone: e.pending === 'BLOCKED' ? 'bad' : 'warn' })
  if (e.desired_protocol && e.protocol && e.desired_protocol !== e.protocol)
    out.push({ text: `改为 ${protocolLabel(e.desired_protocol)} 待下发`, tone: 'warn' })
  return out
})

const egress = computed(() => {
  const e = props.entry
  if (e.next) return e.next.dial ? `→ ${e.next.dial}` : '→ 下一跳'
  return egressText(e)
})

const accessText = computed(() => {
  if (props.entry.access_mode === 'RESTRICTED') return '仅指定用户'
  return props.tier?.name ?? '—'
})
</script>

<template>
  <div
    class="tc"
    :class="[`tc--${tierTone}`, { 'is-sel': selected, 'is-dim': dimmed, 'is-off': !entry.enabled, 'is-relay': entry.transparent, 'is-compact': compact }]"
    role="button"
    tabindex="0"
    :aria-label="`${kindLabel(entry.kind)} ${entry.name}${entry.node_name ? ',所在 ' + entry.node_name : ''}`"
    :data-testid="`card-${entry.key}`"
    @click.stop="emit('select')"
    @keydown.enter.prevent="emit('select')"
    @contextmenu.prevent.stop="(ev: MouseEvent) => emit('menu', ev)"
  >
    <span v-if="flag" class="tc__mark lb-flag" aria-hidden="true">{{ flag }}</span>
    <div class="tc__head">
      <span class="tc__kind" :title="kindLabel(entry.kind)"><LbIcon :name="ICON[entry.kind] ?? 'box'" :size="14" /></span>
      <span v-if="flag" class="tc__flag lb-flag" role="img" :aria-label="`地区 ${entry.region}`">{{ flag }}</span>
      <span v-else class="tc__noregion" title="地区未设置">◌</span>
      <span class="tc__name" :title="entry.name">{{ entry.name }}</span>
      <button
        type="button"
        class="tc__more"
        aria-label="更多操作"
        :data-testid="`card-menu-${entry.key}`"
        @click.stop="(ev: MouseEvent) => emit('menu', ev)"
      >
        <LbIcon name="more" :size="16" />
      </button>
    </div>
    <template v-if="!compact">
      <div class="tc__sub" :title="entry.node_name || entry.source || ''">
        {{ entry.node_name || entry.source || '外部入口' }} · {{ kindLabel(entry.kind) }}<template v-if="entry.protocol && !entry.transparent"> · {{ protocolLabel(entry.protocol) }}</template>
      </div>
      <div class="tc__access">
        <span class="tc__tier">{{ accessText }}</span>
        <span v-if="entry.direct_access" class="tc__badge" title="启用、已部署、订阅公开,且确实有人有凭据 —— 只按配置事实,不代表已验证连通">允许直接接入</span>
        <span v-if="entry.referenced" class="tc__badge tc__badge--ref">被 {{ entry.referenced }} 个入口引用<template v-if="hiddenUpstream">(隐藏 {{ hiddenUpstream }})</template></span>
      </div>
      <ul class="tc__eps">
        <li
          v-for="ep in rows"
          :key="ep.id + ':' + ep.address"
          class="tc__ep"
          :class="{ 'is-hl': highlightEndpoint !== undefined && (ep.id === highlightEndpoint || (ep.default && !highlightEndpoint)) }"
          :title="`${endpointText(ep)}(${familyText(ep.family)}${ep.label ? ' · ' + ep.label : ''})`"
        >
          <span class="tc__addr lb-mono">{{ endpointText(ep) }}</span>
          <span class="tc__fam">{{ ep.family === 'V6' ? 'v6' : 'v4' }}</span>
          <span v-if="ep.referenced" class="tc__used" title="被选作下一跳的目的地址">●</span>
        </li>
      </ul>
      <button v-if="more" type="button" class="tc__fold" @click.stop="emit('toggle')">另外 {{ more }} 条地址 ▾</button>
      <button v-else-if="expanded && entry.endpoints.length > 3" type="button" class="tc__fold" @click.stop="emit('toggle')">收起 ▴</button>
      <div class="tc__status">
        <span v-for="c in statusChips" :key="c.text" class="tc__chip" :class="`tc__chip--${c.tone}`">{{ c.text }}</span>
        <span class="tc__egress" :class="{ 'is-direct': entry.egress === 'DIRECT' }" :title="egress">{{ egress }}</span>
        <span v-if="entry.problems.length" class="tc__prob" :title="entry.problems.join('\n')">⚠ {{ entry.problems.length }}</span>
      </div>
    </template>
    <div v-else class="tc__csub">{{ kindLabel(entry.kind) }} · {{ entry.endpoints.length }} 条地址</div>
  </div>
</template>

<style scoped>
.tc {
  position: absolute;
  box-sizing: border-box;
  padding: 10px 12px 10px 14px;
  display: flex;
  flex-direction: column;
  gap: 4px;
  overflow: hidden;
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
  background: var(--surface);
  box-shadow: var(--shadow-xs);
  color: var(--text);
  cursor: pointer;
  transition:
    box-shadow var(--dur-fast) var(--ease),
    border-color var(--dur-fast) var(--ease),
    opacity var(--dur) var(--ease);
}
.tc::before {
  /* 访问等级:左侧的小色条。 */
  content: '';
  position: absolute;
  left: 0;
  top: 10px;
  bottom: 10px;
  width: 4px;
  border-radius: 0 var(--r-xs) var(--r-xs) 0;
  background: var(--neutral);
}
.tc--t2::before {
  background: var(--brand);
}
.tc--t3::before {
  background: var(--purple);
}
.tc--restricted::before {
  background: repeating-linear-gradient(180deg, var(--purple) 0 4px, transparent 4px 7px);
}
.tc:hover {
  box-shadow: var(--shadow-hover);
}
.tc:focus-visible {
  outline: none;
  box-shadow: var(--focus-ring);
}
.tc.is-sel {
  border-color: var(--brand);
  box-shadow: 0 0 0 2px var(--brand-border), var(--shadow-hover);
}
.tc.is-dim {
  opacity: 0.32;
}
.tc.is-off {
  background: var(--surface2);
}
.tc.is-relay {
  border-style: dashed;
}
.tc__mark {
  /* 国旗水印:低透明度放在右侧背后、避开「⋯」按钮,文字区保持清晰底色。 */
  position: absolute;
  right: -4px;
  top: 30px;
  font-size: 46px;
  opacity: 0.07;
  pointer-events: none;
}
.tc__head {
  display: flex;
  align-items: center;
  gap: 6px;
  height: 24px;
  position: relative;
}
.tc__kind {
  display: inline-flex;
  color: var(--text3);
}
.tc__flag {
  font-size: 15px;
}
.tc__noregion {
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.tc__name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: var(--fs-sm);
  font-weight: 600;
}
.tc__more {
  display: inline-flex;
  padding: 2px;
  border: 0;
  border-radius: var(--r-xs);
  background: transparent;
  color: var(--text3);
  cursor: pointer;
}
.tc__more:hover,
.tc__more:focus-visible {
  background: var(--fill);
  outline: none;
}
.tc__sub,
.tc__csub {
  height: 18px;
  font-size: var(--fs-2xs);
  color: var(--text3);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  position: relative;
}
.tc__access {
  height: 20px;
  display: flex;
  gap: 4px;
  align-items: center;
  overflow: hidden;
  white-space: nowrap;
  position: relative;
}
.tc__tier,
.tc__badge {
  padding: 0 6px;
  border-radius: var(--r-xs);
  font-size: var(--fs-2xs);
  background: var(--fill);
  color: var(--text2);
}
.tc__badge {
  background: var(--ok-bg);
  color: var(--ok);
}
.tc__badge--ref {
  background: var(--brand-bg);
  color: var(--brand);
}
.tc__eps {
  list-style: none;
  margin: 0;
  padding: 0;
  position: relative;
}
.tc__ep {
  height: 20px;
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 0 4px;
  border-radius: var(--r-xs);
  font-size: var(--fs-xs);
}
.tc__ep.is-hl {
  background: var(--brand-bg);
  box-shadow: inset 2px 0 0 var(--brand);
}
.tc__addr {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.tc__fam {
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.tc__used {
  font-size: 8px;
  color: var(--brand);
}
.tc__fold {
  height: 18px;
  padding: 0 4px;
  border: 0;
  background: transparent;
  color: var(--brand);
  font: inherit;
  font-size: var(--fs-2xs);
  text-align: left;
  cursor: pointer;
}
.tc__status {
  height: 22px;
  display: flex;
  align-items: center;
  gap: 4px;
  overflow: hidden;
  white-space: nowrap;
  position: relative;
}
.tc__chip {
  padding: 0 6px;
  border-radius: var(--r-pill);
  font-size: var(--fs-2xs);
}
.tc__chip--bad {
  background: var(--bad-bg);
  color: var(--bad);
}
.tc__chip--warn {
  background: var(--warn-bg);
  color: var(--warn);
}
.tc__chip--neutral {
  background: var(--neutral-bg);
  color: var(--neutral);
}
.tc__egress {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  font-size: var(--fs-2xs);
  color: var(--text2);
}
.tc__egress.is-direct::before {
  content: '⤴ ';
  color: var(--ok);
}
.tc__prob {
  font-size: var(--fs-2xs);
  color: var(--warn);
}
.tc.is-compact .tc__name {
  font-size: var(--fs-lg);
}
</style>
