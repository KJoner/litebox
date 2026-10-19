<script setup lang="ts">
import { computed, reactive } from 'vue'
import type { DashboardCards } from '@/api/client'
import { LbEmptyState, LbIcon, LbInfoTip } from '@/components/lb'
import { formatBytes, formatRate } from '@/utils/format'
import AssetCard from './AssetCard.vue'
import {
  REGION_UNSET,
  assetTypeLabel,
  blankFilters,
  buildOverview,
  filterCards,
  filterOptions,
  money,
  sortCards,
  sortOptions,
  statusKeyLabel,
  type AssetCard as AssetCardModel,
  type AssetType,
  type StatusKey,
} from './assetModel'

/**
 * 卡片视图:筛选栏 → 全局概览 → 响应式卡片网格。
 *
 * 数据全部由仪表盘传进来(它已经批量读过节点、采样、周期流量、外部代理,另加一次卡片聚合接口),
 * 这里**不发任何请求** —— 卡片多少张都是同样那几次请求,不会按卡片逐个去取。
 */
const props = defineProps<{
  cards: AssetCardModel[]
  dash: DashboardCards | null
  loading: boolean
  /** 卡片聚合接口读失败(估值、标签、探测那几块不可用)。 */
  dashError: boolean
}>()
const emit = defineEmits<{
  (e: 'asset', card: AssetCardModel, focus: 'billing' | 'renew' | 'history'): void
  (e: 'retry'): void
}>()

const f = reactive(blankFilters())
const opts = computed(() => filterOptions(props.cards))
const shown = computed(() => sortCards(filterCards(props.cards, f), f.sort))
const filtered = computed(
  () => !!(f.q || f.region || f.group || f.tag || f.status || f.type),
)
const ov = computed(() => buildOverview(props.cards, props.dash))

function reset() {
  Object.assign(f, blankFilters())
}

const statusOptions = Object.entries(statusKeyLabel).map(([value, label]) => ({ value: value as StatusKey, label }))
const typeOptions = Object.entries(assetTypeLabel).map(([value, label]) => ({ value: value as AssetType, label }))
</script>

<template>
  <section class="acv" aria-label="资产卡片">
    <!-- 筛选 -->
    <div class="lb-card lb-card--flush acv__bar">
      <div class="lb-filter">
        <a-input v-model:value="f.q" allow-clear placeholder="搜索名称" aria-label="按名称搜索">
          <template #prefix><LbIcon name="search" :size="14" /></template>
        </a-input>
        <a-select v-model:value="f.region" class="acv__sel" aria-label="地区" :dropdown-match-select-width="false">
          <a-select-option value="">全部地区</a-select-option>
          <a-select-option v-for="r in opts.regions" :key="r" :value="r">{{ r }}</a-select-option>
          <a-select-option v-if="opts.hasUnsetRegion" :value="REGION_UNSET">未设置地区</a-select-option>
        </a-select>
        <a-select v-model:value="f.group" class="acv__sel" aria-label="分组" :dropdown-match-select-width="false">
          <a-select-option value="">全部分组</a-select-option>
          <a-select-option v-for="g in opts.groups" :key="g" :value="g">{{ g }}</a-select-option>
        </a-select>
        <a-select v-if="opts.tags.length" v-model:value="f.tag" class="acv__sel" aria-label="标签" :dropdown-match-select-width="false">
          <a-select-option value="">全部标签</a-select-option>
          <a-select-option v-for="t in opts.tags" :key="t" :value="t">{{ t }}</a-select-option>
        </a-select>
        <a-select v-model:value="f.status" class="acv__sel" aria-label="状态" :dropdown-match-select-width="false">
          <a-select-option value="">全部状态</a-select-option>
          <a-select-option v-for="s in statusOptions" :key="s.value" :value="s.value">{{ s.label }}</a-select-option>
        </a-select>
        <a-select v-model:value="f.type" class="acv__sel" aria-label="类型" :dropdown-match-select-width="false">
          <a-select-option value="">全部类型</a-select-option>
          <a-select-option v-for="t in typeOptions" :key="t.value" :value="t.value">{{ t.label }}</a-select-option>
        </a-select>
        <a-select v-model:value="f.sort" class="acv__sel" aria-label="排序" :dropdown-match-select-width="false">
          <a-select-option v-for="s in sortOptions" :key="s.value" :value="s.value">{{ s.label }}</a-select-option>
        </a-select>
        <a-button v-if="filtered" size="small" type="link" @click="reset">清除筛选</a-button>
        <span class="acv__count lb-tabular">{{ shown.length }} / {{ cards.length }}</span>
      </div>
    </div>

    <div v-if="dashError" class="lb-notice acv__notice" role="alert">
      <span class="lb-notice__icon lb-notice__icon--warn"><LbIcon name="circle-alert" :size="18" /></span>
      <div class="lb-notice__body">
        <div class="lb-notice__title">套餐估值、地区标签与网络探测没有读到</div>
        <div class="lb-notice__text">卡片上这几块显示为不可用,不是零;资源、流量、状态照常来自原有数据。</div>
      </div>
      <div class="lb-notice__actions">
        <a-button size="small" @click="emit('retry')">重试</a-button>
      </div>
    </div>
    <div v-else-if="dash?.errors.length" class="lb-notice acv__notice" role="alert">
      <span class="lb-notice__icon lb-notice__icon--warn"><LbIcon name="circle-alert" :size="18" /></span>
      <div class="lb-notice__body">
        <div class="lb-notice__title">{{ dash.errors.join('、') }}读取失败</div>
        <div class="lb-notice__text">对应的那一块显示为不可用;其余照常。</div>
      </div>
    </div>

    <!-- 概览 -->
    <div class="acv__ov" data-testid="asset-overview">
      <div class="acv__tile">
        <div class="acv__k">节点</div>
        <div class="acv__v lb-tabular">{{ ov.online }} <span class="acv__unit">/ {{ ov.nodeTotal }} 在线</span></div>
        <div class="acv__h" :class="{ 'acv__h--bad': ov.offline }">{{ ov.offline ? `${ov.offline} 台离线或部署失败` : '没有离线节点' }}</div>
      </div>
      <div class="acv__tile">
        <div class="acv__k">
          实时网速
          <LbInfoTip text="最近一次资源采样(每 5 分钟)的网卡速率之和;采样过期的机器不计入,台数另列。" :width="260" />
        </div>
        <div class="acv__v acv__v--rate lb-tabular">
          <span><LbIcon name="arrow-down" :size="13" />{{ formatRate(ov.rx) }}</span>
          <span><LbIcon name="arrow-up" :size="13" />{{ formatRate(ov.tx) }}</span>
        </div>
        <div class="acv__h">{{ ov.rateSamples }} 台{{ ov.rateStale ? ` · ${ov.rateStale} 台采样过期未计入` : '' }}</div>
      </div>
      <div class="acv__tile">
        <div class="acv__k">
          本周期流量
          <LbInfoTip text="各节点当前额度周期的用量之和(计费口径);中转主机不计,读不到的不计。" :width="260" />
        </div>
        <div class="acv__v lb-tabular">{{ formatBytes(ov.cycleUsed) }}</div>
        <div class="acv__h">{{ ov.cycleNodes }} 台节点</div>
      </div>
      <div class="acv__tile">
        <div class="acv__k">7 天内到期</div>
        <div class="acv__v lb-tabular" :class="{ 'acv__v--warn': ov.expiring7d }">{{ ov.expiring7d ?? '—' }}</div>
        <div class="acv__h" :class="{ 'acv__h--bad': ov.overdue }">
          {{ ov.overdue == null ? '读取失败' : ov.overdue ? `另有 ${ov.overdue} 个已到期待确认` : '没有已到期的' }}
        </div>
      </div>
      <div class="acv__tile acv__tile--wide">
        <div class="acv__k">
          月均成本
          <LbInfoTip text="周期性套餐按标价折算到每月;按币种分组,不换汇、不相加。一次性、流量包、永久套餐不进月均。" :width="280" />
        </div>
        <div v-if="ov.costs.length" class="acv__costs lb-tabular">
          <span v-for="r in ov.costs" :key="r.currency" :title="`剩余未摊销价值(按时间估算)合计 ${money(r.unamortized, r.currency)}`">
            <b>{{ money(r.monthly_cost, r.currency) }}</b><span class="acv__unit">/月</span>
          </span>
        </div>
        <div v-else class="acv__v acv__v--muted">—</div>
        <div class="acv__h">{{ ov.costs.length ? '悬停查看剩余未摊销价值' : dash ? '还没有填写任何套餐价格' : '读取失败' }}</div>
      </div>
      <div class="acv__tile">
        <div class="acv__k">网络探测</div>
        <template v-if="dash?.probe.available && dash.probe.enabled">
          <div
            class="acv__v lb-tabular"
            :class="{ 'acv__v--bad': ov.outboundFailing + ov.probeFailing }"
            :title="`出站 ${ov.outboundTargets} 条序列失败 ${ov.outboundFailing};入口 ${ov.probeTargets} 个目标失败 ${ov.probeFailing}` + (ov.boceTargets ? `;Boce 外部拨测 ${ov.boceTargets} 条失败 ${ov.boceFailing}(第三方测点,单列)` : '')"
          >
            {{ ov.outboundFailing + ov.probeFailing }}
            <span class="acv__unit">/ {{ ov.outboundTargets + ov.probeTargets }} 失败</span>
          </div>
          <div class="acv__h">
            出站 {{ ov.outboundTargets }} · 入口 {{ ov.probeTargets }}<span v-if="ov.boceTargets" class="acv__nowrap"> · Boce {{ ov.boceTargets }}</span>
          </div>
        </template>
        <template v-else>
          <div class="acv__v acv__v--muted">未开启</div>
          <div class="acv__h">{{ dash ? '在系统设置里打开' : '读取失败' }}</div>
        </template>
      </div>
    </div>

    <!-- 网格 -->
    <div v-if="loading && !cards.length" class="acv__grid" aria-busy="true">
      <div v-for="i in 6" :key="i" class="lb-card acv__skel"><a-skeleton active :paragraph="{ rows: 5 }" /></div>
    </div>
    <LbEmptyState
      v-else-if="!cards.length"
      variant="empty"
      title="还没有任何资产"
      description="添加自建节点、外部代理或代理源后,这里会为每一项生成一张卡片。"
    />
    <LbEmptyState
      v-else-if="!shown.length"
      variant="filtered"
      title="没有符合筛选条件的卡片"
      description="换一个条件,或清除筛选。"
    />
    <div v-else class="acv__grid">
      <AssetCard v-for="c in shown" :key="c.key" :card="c" @asset="(card, focus) => emit('asset', card, focus)" />
    </div>
  </section>
</template>

<style scoped>
.acv {
  display: flex;
  flex-direction: column;
  gap: var(--sp-4);
}
.acv__bar .lb-filter {
  border-bottom: none;
}
.acv__sel {
  min-width: 112px;
}
.acv__count {
  margin-left: auto;
  font-size: var(--fs-xs);
  color: var(--text3);
}
.acv__ov {
  display: grid;
  /* 1440 宽下六格(月均成本占两格)放得进一行;更窄时自然折行。 */
  grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
  gap: var(--sp-3);
}
.acv__tile {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
  padding: var(--sp-3) var(--sp-4);
  background: var(--surface);
  border: 1px solid var(--sep);
  border-radius: var(--r-card);
  box-shadow: var(--shadow-xs);
}
.acv__tile--wide {
  grid-column: span 2;
}
.acv__k {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-xs);
  color: var(--text3);
}
.acv__v {
  font-size: var(--fs-xl);
  font-weight: 600;
  color: var(--text);
  line-height: 1.3;
}
.acv__v--rate {
  display: flex;
  flex-wrap: wrap;
  column-gap: var(--sp-3);
  font-size: var(--fs-md);
  line-height: 1.9;
}
.acv__v--rate span {
  display: inline-flex;
  align-items: center;
  gap: 2px;
}
.acv__v--warn {
  color: var(--warn);
}
.acv__v--bad {
  color: var(--bad);
}
.acv__v--muted {
  color: var(--text3);
}
.acv__unit {
  font-size: var(--fs-sm);
  font-weight: 400;
  color: var(--text3);
}
.acv__h {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.acv__h--bad {
  color: var(--bad);
}
.acv__costs {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-1) var(--sp-4);
  font-size: var(--fs-lg);
  color: var(--text);
}
.acv__grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
  gap: var(--sp-4);
  align-items: stretch;
}
@media (max-width: 767px) {
  .acv__grid {
    grid-template-columns: minmax(0, 1fr);
  }
  .acv__tile--wide {
    grid-column: auto;
  }
  .acv__sel {
    min-width: 0;
    flex: 1 1 40%;
  }
  .acv__count {
    margin-left: 0;
  }
}
.acv__nowrap {
  white-space: nowrap;
}
</style>
