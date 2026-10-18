<script setup lang="ts">
import { computed } from 'vue'
import { LbIcon, LbInfoTip, LbQuotaBar, LbStatusTag, LbUsageBar, metaOf, type LbStatusMeta } from '@/components/lb'
import { formatDate, formatRate, formatRelative } from '@/utils/format'
import ProbeRow from './ProbeRow.vue'
import { assetTypeLabel, money, rttText, type AssetCard } from './assetModel'

/**
 * 一张资产卡片。五块,顺序固定:身份 → 资源与流量 → 网络质量 → 套餐资产 → 操作。
 *
 * 没有的数据写明「不可用」,不画 0%;几种状态各给各的标记;网络一格先列出站网络质量
 * (这台机器在本机测公共目标),再单列入口连通性(主控连这台机器),两组不混 ——
 * 规则都在 assetModel.ts / probeView.ts,这里只负责画。
 */
const props = defineProps<{ card: AssetCard }>()
const emit = defineEmits<{
  /** 打开「套餐与续费」弹窗;focus 决定先看哪一块。 */
  (e: 'asset', card: AssetCard, focus: 'billing' | 'renew' | 'history'): void
}>()

const c = computed(() => props.card)
const titleId = computed(() => `asset-card-${c.value.key}`)

/** 头部的状态标记:运行状态之外,关机 / 资源过期 / 出站失败 / 入口失败 / 监测关闭各一枚,互不覆盖。 */
const badges = computed<LbStatusMeta[]>(() => {
  const out: LbStatusMeta[] = []
  const card = c.value
  if (card.kind === 'NODE') {
    if (card.cloudStopped) out.push(metaOf.mute('已关机', 'square'))
    if (card.resources?.stale) out.push({ text: '资源数据过期', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' })
    if (card.outbound.some((p) => p.state === 'DOWN')) out.push(metaOf.bad('出站探测失败'))
    else if (card.outbound.some((p) => p.state === 'FAILING')) out.push(metaOf.warn('出站最近一次失败'))
    if (card.outbound.some((p) => p.state === 'COLLECT_FAILED')) {
      out.push({ text: '出站采集失败', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' })
    } else if (card.outbound.some((p) => p.state === 'TOOL_ERROR')) out.push(metaOf.warn('探测工具不可用'))
    if (card.probes.some((p) => p.state === 'DOWN')) out.push(metaOf.bad('入口检测失败'))
    else if (card.probes.some((p) => p.state === 'FAILING')) out.push(metaOf.warn('入口最近一次失败'))
    if (card.monitor === 'NONE' || card.monitor === 'PAUSED' || card.monitor === 'GLOBAL_OFF') {
      out.push(metaOf.paused(card.monitor === 'NONE' ? '未配置监测' : '监测关闭'))
    }
  } else if (card.proxy) {
    out.push(card.proxy.status === 'DISABLED' ? metaOf.mute('已停用', 'square') : metaOf.ok('启用中'))
  } else if (card.source) {
    out.push(card.source.enabled ? metaOf.ok('启用中') : metaOf.mute('已停用', 'square'))
    if (card.source.last_sync_status === 'FAILED') out.push(metaOf.warn('同步失败'))
  }
  return out
})

/** 到期剩余天数。未设置不显示 —— 它不是「永不过期」。 */
const days = computed(() => {
  const v = c.value.expiry
  if (!v || v.state === 'UNSET') return null
  if (v.state === 'OVERDUE') return { text: '已到期 · 待确认', tone: 'bad' }
  const d = Math.max(v.days_left ?? 0, 0)
  return { text: `${d} 天`, tone: v.state === 'SOON' ? 'warn' : 'muted' }
})

const regionTitle = computed(() => (c.value.region ? `地区:${c.value.region}` : '地区未设置(在「编辑套餐」里设置)'))

// 出站最多 3 行、入口最多 2 行,其余进节点详情 —— 卡片网格里一张卡不能比别的高出一截。
const shownOutbound = computed(() => c.value.outbound.slice(0, 3))
const shownProbes = computed(() => c.value.probes.slice(0, 2))
const moreProbes = computed(
  () => Math.max(c.value.outbound.length - 3, 0) + Math.max(c.value.probes.length - 2, 0),
)

const billing = computed(() => c.value.billing)
const val = computed(() => billing.value?.valuation ?? null)
const hasBilling = computed(() => !!billing.value?.has_billing)
const nextLabel = computed(() => {
  const cy = val.value?.billing_cycle
  return cy === 'ONE_TIME' || cy === 'TRAFFIC_PACK' ? '到期' : '下次续费'
})

</script>

<template>
  <article class="ac" :aria-labelledby="titleId" :data-testid="`asset-card-${c.key}`">
    <!-- 1. 身份 -->
    <header class="ac__head">
      <div class="ac__title-row">
        <span class="ac__region" :class="{ 'ac__region--unset': !c.region }" :title="regionTitle">
          <LbIcon name="globe" :size="12" />{{ c.region || '—' }}
        </span>
        <RouterLink :id="titleId" class="ac__name lb-ellipsis" :to="c.to" :title="c.name">{{ c.name }}</RouterLink>
        <span v-if="days" class="ac__days lb-tabular" :class="`ac__days--${days.tone}`" title="商家到期剩余天数">
          <LbIcon name="calendar" :size="12" />{{ days.text }}
        </span>
      </div>
      <div class="ac__sub">
        <span class="ac__subtitle lb-ellipsis" :title="c.subtitle">{{ c.subtitle }}</span>
      </div>
      <div class="ac__chips">
        <span class="lb-chip">{{ assetTypeLabel[c.type] }}</span>
        <span class="lb-chip" :class="c.selfHosted ? 'lb-chip--brand' : 'lb-chip--purple'">
          {{ c.selfHosted ? '自建' : '外部' }}
        </span>
        <span v-for="t in c.tags.slice(0, 3)" :key="t" class="lb-chip lb-chip--outline">{{ t }}</span>
        <span v-if="c.tags.length > 3" class="lb-chip lb-chip--outline" :title="c.tags.join('、')">+{{ c.tags.length - 3 }}</span>
      </div>
      <div class="ac__status">
        <LbStatusTag v-if="c.node" kind="node" :status="c.node.status" />
        <LbStatusTag v-for="(b, i) in badges" :key="i" :meta="b" />
      </div>
    </header>

    <!-- 2. 资源与流量 -->
    <section class="ac__sec" aria-label="资源与流量">
      <template v-if="c.resources">
        <div class="ac__usage">
          <span class="ac__k">CPU</span>
          <LbUsageBar :percent="c.resources.cpu" :stale="c.resources.stale" :min-width="40" />
          <span class="ac__k">内存</span>
          <LbUsageBar :percent="c.resources.mem" :detail="c.resources.memDetail" :stale="c.resources.stale" :min-width="40" />
          <span class="ac__k">磁盘</span>
          <LbUsageBar :percent="c.resources.disk" :detail="c.resources.diskDetail" :stale="c.resources.stale" :min-width="40" />
        </div>
        <div class="ac__rates lb-tabular" :class="{ 'ac__rates--stale': c.resources.stale }">
          <template v-if="c.resources.rx != null">
            <span><LbIcon name="arrow-down" :size="12" />{{ formatRate(c.resources.rx) }}</span>
            <span><LbIcon name="arrow-up" :size="12" />{{ formatRate(c.resources.tx) }}</span>
            <span v-if="c.resources.collectedAt" class="ac__muted">
              {{ c.resources.stale ? '采样已过期 · ' : '' }}{{ formatRelative(c.resources.collectedAt) }}
            </span>
          </template>
          <span v-else class="ac__muted">{{ c.resourcesNote }}</span>
        </div>
      </template>
      <div v-else class="ac__na">
        <LbIcon name="info" :size="13" />
        <span>{{ c.resourcesNote }}:CPU / 内存 / 磁盘不可用</span>
      </div>
      <div class="ac__traffic">
        <span class="ac__k">流量</span>
        <LbQuotaBar
          v-if="c.traffic"
          :used-bytes="c.traffic.used"
          :quota-bytes="c.traffic.quota"
          :warning-level="c.traffic.warning"
          :title="c.traffic.source"
        />
        <span v-else class="ac__muted">{{ c.trafficNote }}</span>
      </div>
    </section>

    <!-- 3. 网络质量 -->
    <section class="ac__sec" aria-label="网络质量">
      <div class="ac__sec-head">
        <span class="ac__sec-title">网络</span>
        <span v-if="c.monitor === 'ON' && c.outbound.length" class="ac__muted">节点本机 · 出站</span>
        <LbInfoTip
          :width="320"
          text="出站:这台机器在本机测公共目标 —— ICMP 看丢包与往返时间,TCP 看建连时间与失败率,都不含 SSH 的耗时。采集失败(面板连不上这台机器)、工具不可用、DNS 失败都没有发起,不计入丢包率。入口连通性:主控对这台机器的端口做 TCP 建连,端口能连上不等于代理协议可用。"
        />
      </div>
      <template v-if="c.monitor === 'ON'">
        <ProbeRow v-for="p in shownOutbound" :key="p.id" :p="p" />
        <div v-if="!c.outbound.length" class="ac__na">
          <span>没有启用出站探测。</span>
          <RouterLink to="/settings#probe-public">去配置公共目标</RouterLink>
        </div>
        <template v-if="shownProbes.length">
          <div class="ac__subhead">
            <span>入口连通性</span>
            <span class="ac__muted">{{ c.probes[0]?.source_label ?? '主控探测' }} · TCP</span>
          </div>
          <ProbeRow v-for="p in shownProbes" :key="p.id" :p="p" />
        </template>
        <RouterLink v-if="moreProbes" class="ac__more" :to="`/nodes/${c.id}/network`">
          还有 {{ moreProbes }} 个目标 <LbIcon name="chevron-right" :size="12" />
        </RouterLink>
      </template>
      <div v-else-if="c.monitor === 'NONE'" class="ac__na">
        <span>还没有监测目标。</span>
        <RouterLink to="/settings#probe-public">配置公共目标</RouterLink>
      </div>
      <div v-else-if="c.monitor === 'PAUSED'" class="ac__na">监测已暂停,点阵与延迟不再更新</div>
      <div v-else-if="c.monitor === 'GLOBAL_OFF'" class="ac__na">网络探测已在系统设置里关闭</div>
      <div v-else-if="c.monitor === 'NA'" class="ac__na">这个面板没有接入网络探测</div>
      <div v-else-if="c.monitor === 'UNKNOWN'" class="ac__na">网络探测数据没有读到(不是没有配置)</div>
      <div v-else class="ac__na ac__na--col">
        <span>外部线路不做定时探测,只看手动检查的结果。</span>
        <span v-if="c.external?.last_check_at" class="lb-tabular">
          上次检查 {{ formatRelative(c.external.last_check_at) }}:
          <b :class="c.external.last_check_ok ? 'ac__tone--ok' : 'ac__tone--bad'">
            {{ c.external.last_check_ok ? `可达 ${rttText(c.external.latency_ms)}` : '不可达' }}
          </b>
        </span>
        <span v-else-if="c.kind === 'EXTERNAL_PROXY'" class="ac__muted">还没有手动检查过</span>
      </div>
    </section>

    <!-- 4. 套餐资产 -->
    <section class="ac__sec" aria-label="套餐资产">
      <template v-if="hasBilling && val">
        <div class="ac__bill-line">
          <span class="ac__bill-cycle">{{ val.cycle_label }}</span>
          <b v-if="val.amount" class="ac__bill-amount lb-tabular">{{ money(val.amount, val.currency) }}</b>
          <span v-if="billing?.plan_name" class="ac__muted lb-ellipsis" :title="billing.plan_name">{{ billing.plan_name }}</span>
        </div>
        <div class="ac__bill-line ac__muted">
          <span v-if="val.next_renewal_at">{{ nextLabel }} {{ formatDate(val.next_renewal_at) }}</span>
          <span v-else-if="val.billing_cycle === 'PERMANENT'">永久,无到期</span>
          <span v-else>未填到期日</span>
          <span v-if="billing?.auto_renew" class="lb-chip lb-chip--brand" title="商家侧的设置状态,面板不代为扣款">自动续费</span>
        </div>
        <div class="ac__value">
          <span class="ac__value-k">
            剩余未摊销价值(估算)
            <LbInfoTip
              :width="320"
              text="已付金额里还对应着没用完的时间 / 流量的部分,按比例估算。它不是余额,也不代表商家会退款;按时间与按流量是两种口径,分开列,不相加。"
            />
          </span>
          <span class="ac__value-v lb-tabular">
            <span :title="val.time.available ? val.time.basis : val.time.reason">
              按时间 <b>{{ val.time.available ? money(val.time.value, val.currency) : '—' }}</b>
            </span>
            <span v-if="val.traffic.available" :title="val.traffic.basis">
              · 按流量 <b>{{ money(val.traffic.value, val.currency) }}</b>
            </span>
          </span>
        </div>
      </template>
      <div v-else-if="!c.cardsLoaded" class="ac__na">套餐与估值没有读到(不是没有设置)</div>
      <div v-else class="ac__na">
        <span>未设置套餐</span>
        <button type="button" class="lb-linkbtn" @click="emit('asset', c, 'billing')">填写</button>
      </div>
    </section>

    <!-- 5. 操作 -->
    <footer class="ac__actions">
      <RouterLink :to="c.to">详情</RouterLink>
      <button type="button" class="lb-linkbtn" @click="emit('asset', c, 'billing')">编辑套餐</button>
      <button type="button" class="lb-linkbtn" @click="emit('asset', c, 'renew')">续费</button>
      <RouterLink v-if="c.kind === 'NODE'" :to="`/nodes/${c.id}/network`">探测历史</RouterLink>
      <button v-else type="button" class="lb-linkbtn" @click="emit('asset', c, 'history')">续费历史</button>
    </footer>
  </article>
</template>

<style scoped>
.ac {
  display: flex;
  flex-direction: column;
  min-width: 0;
  background: var(--surface);
  border: 1px solid var(--sep);
  border-radius: var(--r-card);
  box-shadow: var(--shadow);
  transition: border-color var(--dur-fast), box-shadow var(--dur) var(--ease);
}
.ac:hover,
.ac:focus-within {
  border-color: var(--fill2);
  box-shadow: var(--shadow-hover);
}
.ac__head {
  display: flex;
  flex-direction: column;
  gap: var(--sp-1);
  padding: var(--sp-4) var(--sp-4) var(--sp-3);
}
.ac__title-row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  min-width: 0;
}
.ac__region {
  flex: none;
  display: inline-flex;
  align-items: center;
  gap: 3px;
  padding: 1px 6px;
  border-radius: var(--r-xs);
  background: var(--fill);
  color: var(--text2);
  font-size: var(--fs-2xs);
  font-weight: 600;
  letter-spacing: 0.02em;
}
.ac__region--unset {
  color: var(--text3);
}
.ac__name {
  flex: 1;
  min-width: 0;
  font-size: var(--fs-md);
  font-weight: 600;
  color: var(--text);
}
.ac__name:hover {
  color: var(--brand);
}
.ac__days {
  flex: none;
  display: inline-flex;
  align-items: center;
  gap: 3px;
  font-size: var(--fs-xs);
  color: var(--text3);
}
.ac__days--warn {
  color: var(--warn);
  font-weight: 600;
}
.ac__days--bad {
  color: var(--bad);
  font-weight: 600;
}
.ac__sub {
  min-width: 0;
  font-size: var(--fs-xs);
  color: var(--text3);
}
.ac__subtitle {
  display: block;
}
.ac__chips,
.ac__status {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-1);
}
.ac__status {
  margin-top: 2px;
}
.ac__sec {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-3) var(--sp-4);
  border-top: 1px solid var(--sep2);
  min-width: 0;
}
.ac__sec-head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}
.ac__sec-title {
  font-size: var(--fs-xs);
  font-weight: 600;
  color: var(--text2);
}
.ac__usage {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr);
  column-gap: var(--sp-2);
  row-gap: 6px;
  align-items: center;
}
.ac__k {
  font-size: var(--fs-xs);
  color: var(--text3);
  white-space: nowrap;
}
.ac__traffic {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr);
  column-gap: var(--sp-2);
  align-items: center;
}
.ac__rates {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-3);
  font-size: var(--fs-xs);
  color: var(--text2);
}
.ac__rates span {
  display: inline-flex;
  align-items: center;
  gap: 2px;
}
.ac__rates--stale {
  color: var(--text3);
}
.ac__muted {
  color: var(--text3);
  font-size: var(--fs-xs);
}
.ac__na {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--sp-1);
  font-size: var(--fs-xs);
  color: var(--text3);
}
.ac__na--col {
  flex-direction: column;
  align-items: flex-start;
}
.ac__subhead {
  display: flex;
  align-items: center;
  gap: var(--sp-1);
  margin-top: var(--sp-1);
  padding-top: var(--sp-1);
  border-top: 1px dashed var(--sep);
  font-size: var(--fs-2xs);
  font-weight: 600;
  color: var(--text2);
}
.ac__tone--bad {
  color: var(--bad);
}
.ac__tone--warn {
  color: var(--warn);
}
.ac__tone--ok {
  color: var(--ok);
}
.ac__tone--muted {
  color: var(--text3);
}
.ac__more {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  font-size: var(--fs-xs);
}
.ac__bill-line {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--sp-2);
  min-width: 0;
  font-size: var(--fs-sm);
}
.ac__bill-cycle {
  color: var(--text2);
}
.ac__bill-amount {
  color: var(--text);
}
.ac__value {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: var(--sp-2);
  border-radius: var(--r-sm);
  background: var(--surface2);
}
.ac__value-k {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.ac__value-v {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  font-size: var(--fs-sm);
  color: var(--text2);
}
.ac__value-v b {
  color: var(--text);
}
.ac__actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-3);
  margin-top: auto;
  padding: var(--sp-3) var(--sp-4);
  border-top: 1px solid var(--sep2);
  font-size: var(--fs-sm);
}
</style>
