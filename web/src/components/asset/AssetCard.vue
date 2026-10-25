<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { LbIcon, LbInfoTip, LbStatusTag, LbUsageBar, metaOf, nodeStatusMeta, type LbStatusMeta } from '@/components/lb'
import { formatBytes, formatDate, formatRate, formatRelative, formatTime } from '@/utils/format'
import ProbeRow from './ProbeRow.vue'
import {
  assetTypeLabel,
  money,
  regionFlag,
  shortBytes,
  uptimeNow,
  uptimeText,
  type AssetCard,
} from './assetModel'

/**
 * 一张资产卡片。五块,顺序固定:身份 → 资源与流量 → 网络质量 → 套餐资产 → 操作。
 *
 * 身份只占两行:第一行是地区旗帜、名称、状态(在线时带开机时长);第二行是异常标记与自定义标签,
 * 一行放不下就截掉,悬停或键盘聚焦时浮出全部 —— 浮层盖在下面的内容上,不把卡片撑高。
 * 类型(落地节点 / 外部代理…)与内部名称收进名称的悬停说明,搜索照样搜得到。
 *
 * 没有的数据写明「不可用」,不画 0%;几种状态各给各的标记;网络一格入站在左、出站在右,两组不混。
 * 每组最多 3 项,展示哪几项由节点的「卡片展示设置」决定(没设过就自动推荐)—— 那是展示数量,
 * 不是检测数量。规则都在 assetModel.ts / probeView.ts,这里只负责画。
 *
 * sortable 时标题行左侧多一个拖动把手:按住拖动排顺序(鼠标与触屏都行),聚焦后用方向键也能挪。
 * 拖动的逻辑在卡片网格(AssetCardsView)里,这里只把把手上的事件交出去。
 */
const props = defineProps<{ card: AssetCard; sortable?: boolean }>()
const emit = defineEmits<{
  /** 打开「套餐与续费」弹窗;focus 决定先看哪一块。 */
  (e: 'asset', card: AssetCard, focus: 'billing' | 'renew' | 'history'): void
  /** 在拖动把手上按下(开始拖)。 */
  (e: 'grab', ev: PointerEvent): void
  /** 拖动把手上的方向键:-1 往前挪一位,+1 往后挪一位。 */
  (e: 'nudge', step: -1 | 1): void
}>()

function onGripKey(ev: KeyboardEvent) {
  if (ev.key === 'ArrowLeft' || ev.key === 'ArrowUp') emit('nudge', -1)
  else if (ev.key === 'ArrowRight' || ev.key === 'ArrowDown') emit('nudge', 1)
  else return
  ev.preventDefault()
}

const c = computed(() => props.card)
const titleId = computed(() => `asset-card-${c.value.key}`)

// ---------- 身份 ----------

const flag = computed(() => regionFlag(c.value.region))
const regionTitle = computed(() => (c.value.region ? `地区:${c.value.region}` : '地区未设置(在「编辑套餐」里设置)'))

/** 名称的悬停说明:类型与内部名称都在这里,卡片上不再单占一行。 */
const nameTitle = computed(() => {
  const card = c.value
  return [card.name, `内部名称:${card.subtitle}`, `${assetTypeLabel[card.type]} · 自建`].join('\n')
})

const uptimeSeconds = computed(() => (c.value.uptime ? uptimeNow(c.value.uptime) : null))

/** 第一行右侧的状态。已关机压过在线:节点状态是上一次探测的结论,实例此刻停着就不会在线。 */
const headStatus = computed<LbStatusMeta>(() => {
  const card = c.value
  if (card.cloudStopped) return metaOf.mute('已关机', 'square')
  if (card.node.status === 'ONLINE') {
    return metaOf.ok(uptimeSeconds.value != null ? `在线 ${uptimeText(uptimeSeconds.value)}` : '在线')
  }
  return nodeStatusMeta[card.node.status] ?? metaOf.mute(card.node.status)
})

const headTitle = computed(() => {
  const card = c.value
  if (card.cloudStopped || card.node.status !== 'ONLINE') return undefined
  if (uptimeSeconds.value == null || !card.uptime) return '在线(还没有开机时长的读数)'
  const s = uptimeSeconds.value
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  return `在线:这台机器已连续运行 ${d ? `${d} 天 ` : ''}${h} 小时(按节点的开机时长,读数于 ${formatTime(card.uptime.at)})`
})

/**
 * 第二行的异常标记:资源过期、出站 / 入口 / 外部拨测失败、监测关闭、同步失败各一枚,互不覆盖。
 * 运行状态与关机已经在第一行,这里不重复。
 */
const badges = computed<LbStatusMeta[]>(() => {
  const out: LbStatusMeta[] = []
  const card = c.value
  if (card.resources.stale) out.push({ text: '资源数据过期', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' })
  if (card.outbound.some((p) => p.state === 'DOWN')) out.push(metaOf.bad('出站探测失败'))
  else if (card.outbound.some((p) => p.state === 'FAILING')) out.push(metaOf.warn('出站最近一次失败'))
  if (card.outbound.some((p) => p.state === 'COLLECT_FAILED')) {
    out.push({ text: '出站采集失败', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' })
  } else if (card.outbound.some((p) => p.state === 'TOOL_ERROR')) out.push(metaOf.warn('探测工具不可用'))
  if (card.probes.some((p) => p.state === 'DOWN')) out.push(metaOf.bad('入口检测失败'))
  else if (card.probes.some((p) => p.state === 'FAILING')) out.push(metaOf.warn('入口最近一次失败'))
  // Boce 拨测点连不上只说明那个地区 / 运营商到这里不通,不等于机器离线:琥珀色,不用红色。
  if (card.boce.some((p) => p.state === 'DOWN')) out.push(metaOf.warn('外部拨测失败'))
  if (card.monitor === 'NONE' || card.monitor === 'PAUSED' || card.monitor === 'GLOBAL_OFF') {
    out.push(metaOf.paused(card.monitor === 'NONE' ? '未配置监测' : '监测关闭'))
  }
  return out
})

const chipsText = computed(() => [...badges.value.map((b) => b.text), ...c.value.tags].join('、'))

// 第二行放不下时截掉,悬停浮出全部。判断按子元素的宽度加起来比容器宽,而不是 scrollWidth ——
// 浮出时那一行会换行,scrollWidth 跟着变,拿它判断会在「截断 / 展开」之间来回跳。
const chipsBox = ref<HTMLElement | null>(null)
const chipsRow = ref<HTMLElement | null>(null)
const clipped = ref(false)
let ro: ResizeObserver | null = null

function measure() {
  const box = chipsBox.value
  const row = chipsRow.value
  if (!box || !row) {
    clipped.value = false
    return
  }
  const gap = parseFloat(getComputedStyle(row).columnGap) || 0
  let need = 0
  const kids = Array.from(row.children) as HTMLElement[]
  kids.forEach((el, i) => (need += el.offsetWidth + (i ? gap : 0)))
  clipped.value = need > box.clientWidth + 0.5
}

onMounted(() => {
  if (typeof ResizeObserver !== 'undefined') {
    ro = new ResizeObserver(() => measure())
    if (chipsBox.value) ro.observe(chipsBox.value)
  }
  measure()
})
onBeforeUnmount(() => ro?.disconnect())
watch(chipsBox, (el, old) => {
  if (old) ro?.unobserve(old)
  if (el) ro?.observe(el)
})
watch(chipsText, () => nextTick(measure))

// ---------- 资源与流量 ----------

const res = computed(() => c.value.resources)

const trafficPct = computed(() => {
  const t = c.value.traffic
  if (!t || t.used == null || t.quota <= 0) return null
  return Math.min(100, (t.used / t.quota) * 100)
})
const trafficTone = computed(() => {
  switch (c.value.traffic?.warning) {
    case 'WARNING':
      return 'warn'
    case 'DANGER':
    case 'EXCEEDED':
      return 'bad'
  }
  return 'normal'
})
const trafficTitle = computed(() => {
  const t = c.value.traffic
  if (!t) return c.value.trafficNote
  const used = t.used == null ? '读取失败' : formatBytes(t.used)
  const quota = t.quota > 0 ? formatBytes(t.quota) : '不限'
  const pct = trafficPct.value == null ? '' : `(${trafficPct.value < 1 && trafficPct.value > 0 ? '<1' : Math.round(trafficPct.value)}%)`
  return `${t.source}:${used} / ${quota}${pct}`
})

const rateTitle = computed(() => {
  const r = res.value
  if (r.rateLive) {
    return `最近 ${Math.round(r.rateWindow ?? 0)} 秒的平均网速(仪表盘开着时定时读网卡计数,除 lo 以外全部网卡)`
  }
  return '资源采样里的网速(每 5 分钟采一次,1 秒窗口);仪表盘开着一会儿之后会换成定时读数'
})

// ---------- 网络 ----------

const netRows = computed(() => Math.max(1, c.value.shownIngress.length, c.value.shownOutbound.length))
const ingressTitle = computed(() => {
  const s = new Set(c.value.shownIngress.map((p) => p.source))
  const parts: string[] = []
  if (s.has('MASTER')) parts.push('主控 · TCP 建连')
  const boce = new Set(c.value.shownIngress.filter((p) => p.source === 'BOCE').map((p) => p.metric))
  if (boce.has('BOCE')) parts.push('Boce · TCPing')
  if (boce.has('BOCE_PING')) parts.push('Boce · Ping')
  if (s.has('PEER')) parts.push('自建节点 · Ping / TCPing')
  return parts.length
    ? `入站:${parts.join(' / ')}`
    : '入站:主控连这台机器、Boce 拨测点从外部连这台机器、别的自建机器连这台机器'
})
const ingressAny = computed(() => c.value.probes.length + c.value.boce.length + c.value.peer.length > 0)

// ---------- 套餐 ----------

const billing = computed(() => c.value.billing)
const val = computed(() => billing.value?.valuation ?? null)
const hasBilling = computed(() => !!billing.value?.has_billing)
const nextText = computed(() => {
  const v = val.value
  if (!v) return ''
  if (v.next_renewal_at) {
    const label = v.billing_cycle === 'ONE_TIME' || v.billing_cycle === 'TRAFFIC_PACK' ? '到期' : '下次续费'
    return `${label} ${formatDate(v.next_renewal_at)}`
  }
  return v.billing_cycle === 'PERMANENT' ? '永久,无到期' : '未填到期日'
})

/** 商家到期剩余。未设置不显示 —— 它不是「永不过期」。 */
const remain = computed(() => {
  const v = c.value.expiry
  if (!v || v.state === 'UNSET') return null
  if (v.state === 'OVERDUE') return { text: '已到期 · 待确认', tone: 'bad' }
  const d = Math.max(v.days_left ?? 0, 0)
  return { text: `剩余 ${d} 天`, tone: v.state === 'SOON' ? 'warn' : '' }
})
</script>

<template>
  <article class="ac" :aria-labelledby="titleId" :data-testid="`asset-card-${c.key}`">
    <!-- 1. 身份:两行 -->
    <header class="ac__head">
      <div class="ac__title-row">
        <button
          v-if="sortable"
          type="button"
          class="ac__grip"
          :aria-label="`拖动排序:${c.name}(也可以用方向键前后挪)`"
          title="按住拖动排序;聚焦后用方向键前后挪"
          data-testid="card-grip"
          @pointerdown="(ev) => emit('grab', ev)"
          @keydown="onGripKey"
        >
          <svg viewBox="0 0 10 16" width="10" height="16" aria-hidden="true">
            <circle cx="2.5" cy="3" r="1.4" /><circle cx="7.5" cy="3" r="1.4" />
            <circle cx="2.5" cy="8" r="1.4" /><circle cx="7.5" cy="8" r="1.4" />
            <circle cx="2.5" cy="13" r="1.4" /><circle cx="7.5" cy="13" r="1.4" />
          </svg>
        </button>
        <span v-if="flag" class="ac__flag lb-flag" role="img" :aria-label="regionTitle" :title="regionTitle" data-testid="card-flag">
          {{ flag }}
        </span>
        <span v-else class="ac__flag ac__flag--unset" role="img" :aria-label="regionTitle" :title="regionTitle">
          <LbIcon name="globe" :size="14" />
        </span>
        <RouterLink :id="titleId" class="ac__name lb-ellipsis" :to="c.to" :title="nameTitle">{{ c.name }}</RouterLink>
        <LbStatusTag class="ac__state" :meta="headStatus" :title="headTitle" data-testid="card-state" />
      </div>
      <div
        v-if="badges.length || c.tags.length"
        ref="chipsBox"
        class="ac__chips"
        :class="{ 'ac__chips--clipped': clipped }"
        :tabindex="clipped ? 0 : undefined"
        :title="clipped ? chipsText : undefined"
        :aria-label="clipped ? `标签:${chipsText}` : undefined"
        data-testid="card-chips"
      >
        <div ref="chipsRow" class="ac__chips-row">
          <LbStatusTag v-for="(b, i) in badges" :key="`b-${i}`" :meta="b" />
          <span v-for="t in c.tags" :key="t" class="lb-chip lb-chip--outline">{{ t }}</span>
        </div>
      </div>
    </header>

    <!-- 2. 资源与流量:一行两项,网速单独一行 -->
    <section class="ac__sec" aria-label="资源与流量">
      <div class="ac__res">
        <div class="ac__cell">
          <span class="ac__k">CPU</span>
          <LbUsageBar :percent="res.cpu" :stale="res.stale" :min-width="20" />
        </div>
        <div class="ac__cell">
          <span class="ac__k">内存</span>
          <LbUsageBar :percent="res.mem" :detail="res.memDetail" :stale="res.stale" :min-width="20" />
        </div>
        <div class="ac__cell">
          <span class="ac__k">磁盘</span>
          <LbUsageBar :percent="res.disk" :detail="res.diskDetail" :stale="res.stale" :min-width="20" />
        </div>
        <div class="ac__cell" :title="trafficTitle" data-testid="card-traffic">
          <span class="ac__k">流量</span>
          <template v-if="c.traffic">
            <span class="ac__tbar" :class="`ac__tbar--${trafficTone}`">
              <span v-if="trafficPct != null" class="ac__tbar-fill" :style="{ width: `${trafficPct}%` }" />
            </span>
            <span class="ac__tval lb-tabular">
              <b :class="`ac__tone--${trafficTone}`">{{ c.traffic.used == null ? '—' : shortBytes(c.traffic.used) }}</b>/{{
                c.traffic.quota > 0 ? shortBytes(c.traffic.quota) : '不限'
              }}
            </span>
          </template>
          <span v-else class="ac__muted lb-ellipsis">{{ c.trafficNote }}</span>
        </div>
      </div>
      <div
        class="ac__rates lb-tabular"
        :class="{ 'ac__rates--stale': res.rateStale }"
        :title="rateTitle"
        data-testid="card-rates"
      >
        <template v-if="res.rx != null">
          <span><LbIcon name="arrow-down" :size="12" />{{ formatRate(res.rx) }}</span>
          <span><LbIcon name="arrow-up" :size="12" />{{ formatRate(res.tx) }}</span>
          <span v-if="res.rateAt" class="ac__muted">
            {{ res.rateStale ? '采样已过期 · ' : '' }}{{ formatRelative(res.rateAt) }}
          </span>
        </template>
        <span v-else class="ac__muted">{{ c.resourcesNote }}</span>
      </div>
    </section>

    <!-- 3. 网络质量:入站在左、出站在右 -->
    <section class="ac__sec" aria-label="网络质量">
      <div class="ac__sec-head">
        <span class="ac__sec-title">网络</span>
        <LbInfoTip
          :width="320"
          text="入站(左):主控对这台机器的端口做 TCP 建连(TCP 连接失败率);Boce 拨测点从各地区 / 运营商对订阅入口做 TCPing(TCPing 丢包率)或 Ping(Ping 丢包率;很多机器不回 ICMP,ping 不通不等于入口不可用),都是第三方测点,拨测点异常与接口异常不计入;别的自建机器对订阅入口做 Ping / TCPing(自建节点拨测),来源机器连不上记采集失败、不计入。出站(右):这台机器在本机测公共目标 —— ICMP 看真实丢包率与往返时间,TCP 建连看建连时间与连接失败率,TCP 握手近似看握手超时无应答率(不是丢包率),HTTP(S) 看响应耗时与请求失败率(404、5xx 都算取得了响应),都不含 SSH 的耗时。采集失败、工具不可用、DNS 失败都没有发起,不计入任何比例。端口能连上不等于代理协议可用。协议与端口在名字的悬停说明里;每组最多展示 3 项,在节点详情的「卡片展示设置」里挑选与改名(改名只改展示)。"
        />
        <RouterLink v-if="c.monitor === 'ON' && c.hiddenCount" class="ac__more" :to="`/nodes/${c.id}/network`">
          还有 {{ c.hiddenCount }} 个目标 <LbIcon name="chevron-right" :size="12" />
        </RouterLink>
      </div>
      <template v-if="c.monitor === 'ON'">
        <div class="ac__net" data-testid="card-net">
          <div class="ac__net-h" :title="ingressTitle">入站</div>
          <div class="ac__net-h" title="出站:这台机器在本机测公共目标">出站</div>
          <template v-for="i in netRows" :key="i">
            <ProbeRow v-if="c.shownIngress[i - 1]" compact :p="c.shownIngress[i - 1]" :aliases="c.ingressAliases" />
            <div v-else-if="i === 1" class="ac__net-empty">
              <span>{{ ingressAny ? '未展示' : '未启用' }}</span>
              <RouterLink :to="`/nodes/${c.id}/network`">{{ ingressAny ? '调整' : '配置' }}</RouterLink>
            </div>
            <div v-else />
            <ProbeRow v-if="c.shownOutbound[i - 1]" compact :p="c.shownOutbound[i - 1]" :aliases="c.outboundAliases" />
            <div v-else-if="i === 1" class="ac__net-empty">
              <span>{{ c.outbound.length ? '未展示' : '未启用' }}</span>
              <RouterLink :to="c.outbound.length ? `/nodes/${c.id}/network` : '/probe-library'">
                {{ c.outbound.length ? '调整' : '配置' }}
              </RouterLink>
            </div>
            <div v-else />
          </template>
        </div>
        <div v-if="c.shownInvalid" class="ac__na" data-testid="card-invalid-items">
          <span>{{ c.shownInvalid }} 个展示项已失效(不会自动换成别的测点)。</span>
          <RouterLink :to="`/nodes/${c.id}/network`">调整</RouterLink>
        </div>
      </template>
      <div v-else-if="c.monitor === 'NONE'" class="ac__na">
        <span>还没有监测目标。</span>
        <RouterLink to="/probe-library">配置公共目标</RouterLink>
      </div>
      <div v-else-if="c.monitor === 'PAUSED'" class="ac__na">监测已暂停,点阵与延迟不再更新</div>
      <div v-else-if="c.monitor === 'GLOBAL_OFF'" class="ac__na">网络探测已在系统设置里关闭</div>
      <div v-else-if="c.monitor === 'NA'" class="ac__na">这个面板没有接入网络探测</div>
      <div v-else class="ac__na">网络探测数据没有读到(不是没有配置)</div>
    </section>

    <!-- 4. 套餐资产:两行 -->
    <section class="ac__sec ac__sec--bill" aria-label="套餐资产">
      <template v-if="hasBilling && val">
        <div class="ac__bill-line" :title="billing?.plan_name || undefined" data-testid="card-bill-plan">
          <span class="ac__bill-cycle">{{ val.cycle_label }}</span>
          <b v-if="val.amount" class="lb-tabular">{{ money(val.amount, val.currency) }}</b>
          <span class="ac__sep" aria-hidden="true">·</span>
          <span class="ac__muted lb-tabular">{{ nextText }}</span>
          <span v-if="billing?.auto_renew" class="lb-chip lb-chip--brand" title="商家侧的设置状态,面板不代为扣款">自动续费</span>
        </div>
        <div class="ac__bill-line" data-testid="card-bill-value">
          <template v-if="remain">
            <span class="lb-tabular" :class="remain.tone ? `ac__tone--${remain.tone}` : ''" title="商家到期剩余">{{ remain.text }}</span>
            <span class="ac__sep" aria-hidden="true">·</span>
          </template>
          <span class="ac__muted">剩余价值(估算)</span>
          <span class="lb-tabular" :title="val.time.available ? val.time.basis : val.time.reason">
            <template v-if="val.traffic.available">按时间 </template>
            <b>{{ val.time.available ? money(val.time.value, val.currency) : '—' }}</b>
          </span>
          <span v-if="val.traffic.available" class="lb-tabular" :title="val.traffic.basis">
            · 按流量 <b>{{ money(val.traffic.value, val.currency) }}</b>
          </span>
          <LbInfoTip
            :width="320"
            text="剩余未摊销价值(估算):已付金额里还对应着没用完的时间 / 流量的部分,按比例估算。它不是余额,也不代表商家会退款;按时间与按流量是两种口径,分开列,不相加。"
          />
        </div>
      </template>
      <div v-else-if="!c.cardsLoaded" class="ac__na">套餐与估值没有读到(不是没有设置)</div>
      <template v-else>
        <div v-if="remain && c.expiry?.expires_at" class="ac__bill-line">
          <span class="ac__muted lb-tabular">到期 {{ formatDate(c.expiry.expires_at) }}</span>
          <span class="ac__sep" aria-hidden="true">·</span>
          <span class="lb-tabular" :class="remain.tone ? `ac__tone--${remain.tone}` : ''">{{ remain.text }}</span>
        </div>
        <div class="ac__na">
          <span>未设置套餐</span>
          <button type="button" class="lb-linkbtn" @click="emit('asset', c, 'billing')">填写</button>
        </div>
      </template>
    </section>

    <!-- 5. 操作 -->
    <footer class="ac__actions">
      <RouterLink :to="c.to">详情</RouterLink>
      <button type="button" class="lb-linkbtn" @click="emit('asset', c, 'billing')">编辑套餐</button>
      <button type="button" class="lb-linkbtn" @click="emit('asset', c, 'renew')">续费</button>
      <RouterLink :to="`/nodes/${c.id}/network`">探测历史</RouterLink>
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
  gap: var(--sp-2);
  padding: var(--sp-3) var(--sp-4);
}
.ac__title-row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  min-width: 0;
}
/* 拖动把手:按住拖。touch-action: none 让触屏上的按住拖不被页面滚动抢走。 */
.ac__grip {
  flex: none;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 20px;
  height: 24px;
  margin-left: -6px;
  padding: 0;
  border: 0;
  border-radius: var(--r-xs);
  background: transparent;
  color: var(--text3);
  cursor: grab;
  touch-action: none;
}
.ac__grip svg {
  fill: currentColor;
}
.ac__grip:hover {
  color: var(--text2);
  background: var(--fill);
}
.ac__grip:active {
  cursor: grabbing;
}
.ac__grip:focus-visible {
  outline: 2px solid var(--brand);
  outline-offset: 1px;
}
.ac__flag {
  flex: none;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 22px;
  font-size: var(--fs-lg);
}
.ac__flag--unset {
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
.ac__state {
  flex: none;
}

/* 第二行:一行放不下就截掉;悬停 / 聚焦时整行浮出来盖在下面,卡片高度不变。 */
.ac__chips {
  position: relative;
  min-width: 0;
  height: 22px;
}
.ac__chips-row {
  display: flex;
  align-items: center;
  gap: var(--sp-1);
  height: 100%;
  overflow: hidden;
}
.ac__chips-row > * {
  flex: none;
}
.ac__chips--clipped .ac__chips-row {
  mask-image: linear-gradient(to right, black calc(100% - 28px), transparent);
}
.ac__chips--clipped {
  cursor: default;
}
.ac__chips--clipped:focus-visible {
  outline: 2px solid var(--brand);
  outline-offset: 2px;
  border-radius: var(--r-xs);
}
.ac__chips--clipped:hover .ac__chips-row,
.ac__chips--clipped:focus .ac__chips-row {
  position: absolute;
  z-index: 3;
  top: -5px;
  left: -7px;
  right: -7px;
  height: auto;
  flex-wrap: wrap;
  row-gap: var(--sp-1);
  padding: 4px 6px;
  overflow: visible;
  mask-image: none;
  background: var(--surface);
  border: 1px solid var(--sep);
  border-radius: var(--r-sm);
  box-shadow: var(--shadow-lg);
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

/* 资源:一行两项 */
.ac__res {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  column-gap: var(--sp-4);
  row-gap: 6px;
}
.ac__cell {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  min-width: 0;
}
.ac__cell > :deep(.lb-usage) {
  flex: 1;
}
.ac__k {
  flex: none;
  min-width: 2em;
  font-size: var(--fs-xs);
  color: var(--text3);
  white-space: nowrap;
}
.ac__tbar {
  flex: 1;
  min-width: 16px;
  height: 4px;
  border-radius: var(--r-pill);
  background: var(--track);
  overflow: hidden;
}
.ac__tbar-fill {
  display: block;
  height: 100%;
  border-radius: inherit;
  background: var(--brand);
  transform-origin: left;
  animation: lb-grow 0.6s var(--ease);
}
.ac__tbar--warn .ac__tbar-fill {
  background: var(--warn);
}
.ac__tbar--bad .ac__tbar-fill {
  background: var(--bad);
}
.ac__tval {
  flex: none;
  font-size: var(--fs-xs);
  color: var(--text3);
  white-space: nowrap;
}
.ac__tval b {
  font-weight: 600;
  color: var(--text2);
}
.ac__tval b.ac__tone--warn {
  color: var(--warn);
}
.ac__tval b.ac__tone--bad {
  color: var(--bad);
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

/* 网络:入站 | 出站 两列,同一行的两项上下对齐 */
.ac__net {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  column-gap: var(--sp-4);
  row-gap: var(--sp-2);
  align-items: start;
}
.ac__net-h {
  font-size: var(--fs-2xs);
  font-weight: 600;
  color: var(--text3);
  letter-spacing: 0.02em;
}
.ac__net-empty {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-1);
  font-size: var(--fs-xs);
  color: var(--text3);
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
.ac__more {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  margin-left: auto;
  font-size: var(--fs-xs);
}

/* 套餐:两行 */
.ac__sec--bill {
  gap: var(--sp-1);
}
.ac__bill-line {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  column-gap: var(--sp-1);
  row-gap: 2px;
  min-width: 0;
  font-size: var(--fs-sm);
  color: var(--text2);
}
.ac__bill-line b {
  color: var(--text);
}
.ac__bill-line .ac__muted {
  font-size: var(--fs-sm);
}
.ac__bill-cycle {
  color: var(--text2);
}
.ac__sep {
  color: var(--text3);
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
