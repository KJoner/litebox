<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { portalApi, ApiError, type PortalProbeCard, type PortalProbeItem } from '@/api/client'
import { formatTime } from '@/utils/format'
import { LbEmptyState, LbInfoTip, LbStatusTag, LbTimeText, LbUsageBar, metaOf, type LbStatusMeta } from '@/components/lb'
import { regionFlag, uptimeNow, uptimeText } from '@/components/asset/assetModel'
import { rateName, rateText, rttText } from '@/components/asset/probeView'

/**
 * 探针。你能用的那几台机器此刻怎么样:CPU / 内存 / 磁盘的使用率,以及管理员挑出来的几个网络检测。
 *
 * 只有这几样,别的一概不给(后端是白名单,这里也没有地方画):
 *   - 资源只给百分比,不给内存 / 磁盘的绝对用量,也不给网速与流量;
 *   - 网络只给管理员卡片上那几项(入站、出站各最多 3 项),没有地址、端口与测点的来历;
 *   - 没有套餐、价格与到期日 —— 那是管理员的账。
 * 范围按你的访问等级(与「我的节点」同一个来源);看不到的机器不是坏了,是不归你用。
 *
 * 页面开着时每分钟自动重读一次(只读面板库里已有的结果,不会去连机器),藏到后台时停。
 */
const items = ref<PortalProbeCard[]>([])
const loading = ref(true)
const loadError = ref('')
const loadedAt = ref<string | null>(null)

const REFRESH_MS = 60_000
let timer: ReturnType<typeof setTimeout> | null = null
/** 离开页面之后,还在路上的那一次刷新回来时不能再排下一次。 */
let disposed = false

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    const r = await portalApi.probe()
    items.value = r.items
    loadError.value = ''
    loadedAt.value = new Date().toISOString()
  } catch (err) {
    // 自动刷新读失败时保留上一次的卡片,只在页头说一句;第一次就读不到才整页报错。
    loadError.value = err instanceof ApiError ? err.message : '暂时读不到探针数据'
  } finally {
    loading.value = false
  }
}

function stop() {
  if (timer) clearTimeout(timer)
  timer = null
}

function schedule() {
  stop()
  if (disposed || document.hidden) return
  timer = setTimeout(() => void load(true).then(schedule), REFRESH_MS)
}

function onVisibility() {
  if (document.hidden) {
    stop()
    return
  }
  const since = loadedAt.value ? Date.now() - new Date(loadedAt.value).getTime() : Infinity
  if (since >= REFRESH_MS) void load(true).then(schedule)
  else schedule()
}

async function refresh() {
  stop()
  await load(items.value.length > 0)
  schedule()
}

onMounted(async () => {
  document.addEventListener('visibilitychange', onVisibility)
  await load()
  schedule()
})
onBeforeUnmount(() => {
  disposed = true
  document.removeEventListener('visibilitychange', onVisibility)
  stop()
})

const summary = computed(() => {
  const online = items.value.filter((c) => c.status === 'online').length
  return `${items.value.length} 台机器 · ${online} 台在线 · 每分钟自动刷新`
})

function headStatus(c: PortalProbeCard): LbStatusMeta {
  if (c.status === 'disabled') return metaOf.mute('已停用', 'square')
  if (c.status === 'offline') return metaOf.bad('离线')
  if (c.uptime_seconds != null && c.uptime_at) {
    return metaOf.ok(`在线 ${uptimeText(uptimeNow({ seconds: c.uptime_seconds, at: c.uptime_at }))}`)
  }
  return metaOf.ok('在线')
}

function flagTitle(c: PortalProbeCard): string {
  return c.region ? `地区:${c.region}` : '地区未设置'
}

const STATE_TEXT: Record<PortalProbeItem['state'], string> = {
  OK: '',
  LOSSY: '部分丢包',
  FAILING: '最近一次失败',
  DOWN: '连续失败',
  HTTP_5XX: '对方服务返回异常',
  PAUSED: '已暂停检测',
  NO_DATA: '暂无最新数据',
}

function tone(p: PortalProbeItem): string {
  switch (p.state) {
    case 'DOWN':
      return 'bad'
    case 'FAILING':
    case 'LOSSY':
    case 'HTTP_5XX':
      return 'warn'
    case 'OK':
      return ''
  }
  return 'muted'
}

function valueText(p: PortalProbeItem): string {
  if (p.rtt_ms != null) return rttText(p.rtt_ms)
  return p.last_label || '—'
}

const DOT_TEXT: Record<string, string> = {
  OK: '正常',
  PARTIAL: '部分失败',
  FAIL: '失败',
  HTTP5XX: '有响应,但对方服务返回异常',
  NONE: '没有数据',
}

function dotTitle(p: PortalProbeItem, d: { at: string; state: string }): string {
  return `${formatTime(d.at)} 起 ${Math.round(p.dot_seconds / 60)} 分钟:${DOT_TEXT[d.state] ?? '没有数据'}`
}

function rows(c: PortalProbeCard): number {
  return Math.max(1, c.ingress.length, c.outbound.length)
}
</script>

<template>
  <div class="lb-page pp">
    <div class="lb-page__head">
      <div class="lb-page__title-wrap">
        <h1 class="lb-page__title">
          <span>探针</span>
          <LbInfoTip
            text="这里是你能用的几台机器。CPU / 内存 / 磁盘是面板每 5 分钟采一次的使用率;网络是管理员挑的几个检测:「入站」是从各地连到这台机器,「出站」是这台机器访问外面。延迟与失败率按各自的检测方式算 —— 端口能连上不等于你的客户端一定能用,某个地区连不上也不等于机器坏了。"
            :width="320"
          />
        </h1>
        <div class="lb-page__summary">
          {{ items.length ? summary : '你能用的机器此刻的状态' }}
          <template v-if="loadedAt"> · 更新于 <LbTimeText :value="loadedAt" /></template>
        </div>
      </div>
      <div class="lb-page__actions">
        <a-button :loading="loading" @click="refresh">刷新</a-button>
      </div>
    </div>

    <LbEmptyState v-if="loadError && !items.length" variant="error" :title="loadError" @retry="refresh" />

    <div v-else-if="loading && !items.length" class="pp__grid" aria-busy="true">
      <div v-for="i in 3" :key="i" class="pp__card"><a-skeleton active :paragraph="{ rows: 4 }" /></div>
    </div>

    <div v-else-if="!items.length" class="pp__card">
      <LbEmptyState
        variant="empty"
        title="还没有可以查看的机器"
        description="你的访问等级下暂时没有分配节点。订阅地址仍然有效;需要的话请联系管理员。"
      />
    </div>

    <template v-else>
      <div v-if="loadError" class="pp__stale" role="status">刚才那次刷新没有读到({{ loadError }}),下面是上一次的数据。</div>
      <div class="pp__grid" data-testid="portal-probe-grid">
        <article v-for="(c, idx) in items" :key="`${idx}-${c.name}`" class="pp__card" data-testid="portal-probe-card">
          <header class="pp__head">
            <div class="pp__title-row">
              <span v-if="regionFlag(c.region)" class="pp__flag lb-flag" role="img" :aria-label="flagTitle(c)" :title="flagTitle(c)">
                {{ regionFlag(c.region) }}
              </span>
              <span class="pp__name lb-ellipsis" :title="c.name">{{ c.name }}</span>
              <LbStatusTag class="pp__state" :meta="headStatus(c)" />
            </div>
            <div v-if="c.tags.length" class="pp__tags">
              <span v-for="t in c.tags" :key="t" class="lb-chip lb-chip--outline">{{ t }}</span>
            </div>
          </header>

          <section class="pp__sec" aria-label="资源">
            <template v-if="c.resources">
              <div class="pp__res">
                <div class="pp__cell">
                  <span class="pp__k">CPU</span>
                  <LbUsageBar :percent="c.resources.cpu" :stale="c.resources.stale" :min-width="20" />
                </div>
                <div class="pp__cell">
                  <span class="pp__k">内存</span>
                  <LbUsageBar :percent="c.resources.mem" :stale="c.resources.stale" :min-width="20" />
                </div>
                <div class="pp__cell">
                  <span class="pp__k">磁盘</span>
                  <LbUsageBar :percent="c.resources.disk" :stale="c.resources.stale" :min-width="20" />
                </div>
              </div>
              <div v-if="c.resources.stale" class="pp__muted">数据已过期(最近一次:<LbTimeText :value="c.resources.collected_at" />),不代表此刻</div>
            </template>
            <div v-else class="pp__muted">还没有资源数据</div>
          </section>

          <section class="pp__sec" aria-label="网络">
            <div class="pp__sec-title">网络</div>
            <div v-if="c.ingress.length || c.outbound.length" class="pp__net">
              <div class="pp__net-h" title="从各地连到这台机器的检测">入站</div>
              <div class="pp__net-h" title="这台机器访问外面的检测">出站</div>
              <template v-for="i in rows(c)" :key="i">
                <template v-for="side in [c.ingress, c.outbound]" :key="side === c.ingress ? 'in' : 'out'">
                  <div v-if="side[i - 1]" class="pp__item" data-testid="portal-probe-item">
                    <div class="pp__line">
                      <span class="pp__label lb-ellipsis" :title="side[i - 1].name">{{ side[i - 1].name }}</span>
                      <span class="pp__val lb-tabular" :class="`pp__tone--${tone(side[i - 1])}`">{{ valueText(side[i - 1]) }}</span>
                    </div>
                    <div class="pp__dots" role="img" :aria-label="`最近 ${side[i - 1].dots.length} 个时间段的检测结果`">
                      <span
                        v-for="d in side[i - 1].dots"
                        :key="d.at"
                        class="pp__dot"
                        :class="`pp__dot--${d.state.toLowerCase()}`"
                        :title="dotTitle(side[i - 1], d)"
                      />
                    </div>
                    <div class="pp__meta lb-tabular">
                      <span>1h {{ rateName(side[i - 1].metric) }} {{ rateText(side[i - 1].h1_rate) }}</span>
                      <span>24h {{ rateText(side[i - 1].h24_rate) }}</span>
                      <span v-if="STATE_TEXT[side[i - 1].state]" :class="`pp__tone--${tone(side[i - 1])}`">
                        {{ STATE_TEXT[side[i - 1].state] }}
                      </span>
                    </div>
                  </div>
                  <div v-else-if="i === 1" class="pp__muted">暂无检测</div>
                  <div v-else />
                </template>
              </template>
            </div>
            <div v-else class="pp__muted">这台机器暂时没有展示网络检测</div>
          </section>
        </article>
      </div>
    </template>
  </div>
</template>

<style scoped>
.pp {
  gap: 20px;
}
.pp__grid {
  display: grid;
  /* 两列网络检测并排,一张卡至少要这么宽才放得下「1h TCPing 丢包 0%」不折成两行。 */
  grid-template-columns: repeat(auto-fill, minmax(340px, 1fr));
  gap: var(--sp-4);
}
.pp__card {
  display: flex;
  flex-direction: column;
  min-width: 0;
  background: var(--surface);
  border: 1px solid var(--sep);
  border-radius: var(--r-card);
  box-shadow: var(--shadow);
}
.pp__grid > .pp__card:has(.ant-skeleton) {
  padding: var(--sp-4);
}
.pp__head {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-3) var(--sp-4);
}
.pp__title-row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  min-width: 0;
}
.pp__flag {
  flex: none;
  font-size: var(--fs-lg);
}
.pp__name {
  flex: 1;
  min-width: 0;
  font-size: var(--fs-md);
  font-weight: 600;
  color: var(--text);
}
.pp__state {
  flex: none;
}
.pp__tags {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-1);
}
.pp__sec {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-3) var(--sp-4);
  border-top: 1px solid var(--sep2);
  min-width: 0;
}
.pp__sec-title {
  font-size: var(--fs-xs);
  font-weight: 600;
  color: var(--text2);
}
.pp__res {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  column-gap: var(--sp-3);
}
.pp__cell {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.pp__k {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.pp__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.pp__stale {
  padding: 0 4px;
  margin-bottom: -8px;
  font-size: var(--fs-xs);
  color: var(--warn);
}
.pp__net {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  column-gap: var(--sp-4);
  row-gap: var(--sp-3);
}
.pp__net-h {
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.pp__item {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}
.pp__line {
  display: flex;
  align-items: center;
  gap: var(--sp-1);
  min-width: 0;
}
.pp__label {
  flex: 1;
  min-width: 0;
  font-size: var(--fs-xs);
  color: var(--text2);
}
.pp__val {
  flex: none;
  font-size: var(--fs-xs);
  font-weight: 600;
  color: var(--text);
}
.pp__meta {
  display: flex;
  flex-wrap: wrap;
  column-gap: var(--sp-2);
  min-width: 0;
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.pp__tone--bad {
  color: var(--bad);
}
.pp__tone--warn {
  color: var(--warn);
}
.pp__tone--muted {
  color: var(--text3);
}
/* 点阵:正常绿、部分失败琥珀、失败红、有响应但 5xx 琥珀描边;没有数据画空心虚线 —— 绝不按正常画。 */
.pp__dots {
  display: flex;
  gap: 1px;
  height: 10px;
}
.pp__dot {
  flex: 1;
  min-width: 2px;
  border-radius: 1px;
}
.pp__dot--ok {
  background: var(--ok);
}
.pp__dot--partial {
  background: var(--warn);
}
.pp__dot--fail {
  background: var(--bad);
}
.pp__dot--http5xx {
  background: var(--warn-bg);
  box-shadow: inset 0 0 0 1px var(--warn);
}
.pp__dot--none {
  box-shadow: inset 0 0 0 1px var(--sep);
}
@media (max-width: 767px) {
  .pp__grid {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
