<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import {
  api,
  type DailyPoint,
  type DashboardAlert,
  type DashboardSummary,
  type DeploymentRecord,
  type Node,
  type NodeCycleUsage,
  type NodeMetrics,
} from '@/api/client'
import { formatBytes, formatUTCDay } from '@/utils/format'
import {
  LbEmptyState,
  LbIcon,
  LbInfoTip,
  LbMetricCard,
  LbQuotaBar,
  LbSectionTitle,
  LbSparkline,
  LbStatusTag,
  LbTimeText,
  configStatusMeta,
  subscriptionOffMeta,
  type LbPoint,
} from '@/components/lb'
import { configState, nodeBadges } from '@/components/lb/derive'
import { threshold } from '@/theme/tokens'

/**
 * 仪表盘要回答的只有一句话:今天有没有事。V18(方案 B「留白」)把这句话
 * 直接做成 h1:没有 error 级告警时是「今天没有大事。」,有则「有 N 件事要处理。」
 *
 * 四块内容各自独立取数、各自降级 —— 原来是一个 Promise.all 加一个
 * message.error,任何一个接口挂掉整页都只弹一条三秒吐司,而卡片渲染的是
 * `summary?.traffic_month ?? 0`,页面稳稳地显示「本月流量 0 B」。
 * 读不到和真的是零长得一模一样,这是最容易骗到管理员的一种失败。
 */
const router = useRouter()

const summary = ref<DashboardSummary | null>(null)
const summaryError = ref(false)

const alerts = ref<DashboardAlert[]>([])

const nodes = ref<Node[]>([])
const cycles = ref<Record<number, NodeCycleUsage>>({})
const metrics = ref<Record<number, NodeMetrics>>({})
const nodesError = ref(false)

const deploys = ref<DeploymentRecord[]>([])
const deployError = ref(false)

const daily = ref<DailyPoint[]>([])
const dailyError = ref(false)
const range = ref(30)

const loading = ref(true)

async function loadSummary() {
  summaryError.value = false
  try {
    summary.value = await api.dashboardSummary()
  } catch {
    summary.value = null
    summaryError.value = true
  }
  // 预警取不到就当没有预警 —— 它是附加信息,不值得让整页进错误态。
  try {
    alerts.value = (await api.dashboardAlerts()).items
  } catch {
    alerts.value = []
  }
}

async function loadNodes() {
  nodesError.value = false
  try {
    nodes.value = (await api.nodes()).items
  } catch {
    nodes.value = []
    nodesError.value = true
    return
  }
  // 周期流量与资源采样都是列级信息:读不到只让那一列显示「—」,
  // 不能把整张节点健康表判成失败。
  api
    .nodesCycleTraffic()
    .then((r) => (cycles.value = Object.fromEntries(r.items.map((c) => [c.node_id, c]))))
    .catch(() => (cycles.value = {}))
  api
    .nodeMetricsLatest()
    .then((r) => (metrics.value = Object.fromEntries(r.items.map((m) => [m.node_id, m]))))
    .catch(() => (metrics.value = {}))
}

async function loadDeploys() {
  deployError.value = false
  try {
    deploys.value = (await api.deployments(100)).items
  } catch {
    deploys.value = []
    deployError.value = true
  }
}

async function loadDaily() {
  dailyError.value = false
  try {
    daily.value = (await api.siteDailyTraffic(range.value)).daily
  } catch {
    daily.value = []
    dailyError.value = true
  }
}

async function load() {
  loading.value = true
  await Promise.all([loadSummary(), loadNodes(), loadDeploys(), loadDaily()])
  loading.value = false
}

// 切换时间范围只重取曲线,不刷整页 —— 指标卡与节点表跟这个范围无关。
watch(range, loadDaily)
onMounted(load)

// ---------- 派生 ----------

const metricState = computed(() =>
  summaryError.value ? 'error' : loading.value ? 'loading' : summary.value ? 'ready' : 'empty',
)

const nodeMetricState = computed(() =>
  nodesError.value
    ? 'error'
    : loading.value
      ? 'loading'
      : nodes.value.length
        ? 'ready'
        : 'empty',
)

const offlineCount = computed(() => nodes.value.filter((n) => n.status === 'OFFLINE').length)
const subOffCount = computed(() => nodes.value.filter((n) => !n.subscription_enabled).length)
const errorAlerts = computed(() => alerts.value.filter((a) => a.level === 'error').length)

/** 一句话结论。取自告警数量:有 error 级告警才算「有事」。 */
const headline = computed(() => {
  if (loading.value && !summary.value) return '正在看今天的情况…'
  return errorAlerts.value > 0 ? `有 ${errorAlerts.value} 件事要处理。` : '今天没有大事。'
})

const summaryLine = computed(() => {
  const parts: string[] = []
  if (alerts.value.length) parts.push(`${alerts.value.length} 条告警待处理`)
  if (summary.value) {
    const online = summary.value.node_online
    const total = summary.value.node_total
    if (total === 0) parts.push('还没有添加任何节点')
    else if (online === total) parts.push(`${total} 台节点全部运行正常`)
    else parts.push(`${online} / ${total} 台节点运行正常`)
  }
  if (!parts.length) return summaryError.value ? '概览数据读取失败。' : ''
  return parts.join(',') + '。'
})

const today = computed(() => {
  const d = new Date()
  const week = ['日', '一', '二', '三', '四', '五', '六'][d.getDay()]
  return `${d.getFullYear()} 年 ${d.getMonth() + 1} 月 ${d.getDate()} 日 · 星期${week}`
})

/** 近 7 天部署。分母也取这个窗口,否则「2 / 共 100 次」里的 100 是几个月的量。 */
const recentDeploys = computed(() => {
  const since = Date.now() - 7 * 86400000
  return deploys.value.filter((d) => new Date(d.started_at).getTime() >= since)
})
const failedDeploys = computed(() =>
  recentDeploys.value.filter((d) => d.status === 'FAILED' || d.status === 'ROLLED_BACK'),
)

/**
 * 缺的日子传 null,不补 0,也不插值。
 * 补 0 会让当天看起来「没人用」,插值会凭空造出一个从未存在的数字 ——
 * 而 traffic_daily 里没有那一行,本来就同时意味着「没流量」和「同步没跑完」。
 */
const points = computed<LbPoint[]>(() => {
  const byDay = new Map(daily.value.map((d) => [d.day, d.total]))
  const out: LbPoint[] = []
  for (let i = range.value - 1; i >= 0; i--) {
    const key = new Date(Date.now() - i * 86400000).toISOString().slice(0, 10)
    out.push({ at: key, value: byDay.has(key) ? (byDay.get(key) as number) : null })
  }
  return out
})

const axisLabels = computed(() => {
  const p = points.value
  if (p.length < 2) return []
  const idx = [0, Math.floor(p.length / 4), Math.floor(p.length / 2), Math.floor((p.length * 3) / 4), p.length - 1]
  return idx.map((i) => p[i].at.slice(5))
})

const peak = computed(() => {
  let best: DailyPoint | null = null
  for (const d of daily.value) if (!best || d.total > best.total) best = d
  return best
})

const monthStart = computed(() => {
  const d = new Date()
  return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, '0')}-01`
})

const headTip = computed(
  () =>
    `一句话结论取自告警数量:有 error 级告警时改为「有 N 件事要处理」。周期边界均为 UTC 00:00,本月自 ${monthStart.value} 起;节点额度只预警,不会停服。`,
)

/** 部署记录的一行结论。失败要分两种说法 —— 见 DeploymentsView 里的说明。 */
function conclusion(d: DeploymentRecord): string {
  if (d.status === 'SUCCESS') return `成功 · ${d.steps?.length ?? 0} 步`
  if (d.status === 'RUNNING') {
    const done = (d.steps ?? []).filter((s) => s.status !== 'SKIPPED').length
    return `部署中 · 步骤 ${done}/${d.steps?.length ?? '?'}`
  }
  return d.error_message || '失败'
}

function alertKind(a: DashboardAlert): 'bad' | 'warn' {
  return a.level === 'error' ? 'bad' : 'warn'
}

function alertCategory(a: DashboardAlert): string {
  switch (a.category) {
    case 'user':
      return '用户'
    case 'cloud_account':
      return '云账号'
    default:
      return '节点'
  }
}

function alertGo(a: DashboardAlert) {
  switch (a.category) {
    case 'user':
      return { label: '查看用户', to: '/users' }
    case 'cloud_account':
      return { label: '查看云账号', to: '/settings' }
    default:
      return { label: '查看节点', to: a.target_id ? `/nodes/${a.target_id}` : '/nodes' }
  }
}
</script>

<template>
  <div class="lb-page dv">
    <div class="lb-page__head">
      <div class="lb-page__title-wrap">
        <div class="lb-page__eyebrow">{{ today }}</div>
        <h1 class="lb-page__title">
          <span>{{ headline }}</span>
          <LbInfoTip :text="headTip" :width="300" />
        </h1>
        <div class="lb-page__summary">{{ summaryLine }}</div>
      </div>
      <div class="lb-page__actions">
        <a-button type="primary" :loading="loading" @click="load">刷新</a-button>
      </div>
    </div>

    <!-- 系统告警:iOS 通知式卡片。0 条时整块不出现 —— 不要给「今天没事」也占一块版面。 -->
    <section v-if="alerts.length" class="dv__alerts">
      <div v-for="(a, i) in alerts" :key="i" class="lb-notice lb-card--hover dv__alert">
        <span class="lb-notice__icon" :class="`lb-notice__icon--${alertKind(a)}`">
          <LbIcon :name="a.category === 'user' ? 'user' : a.category === 'cloud_account' ? 'cloud' : 'alert-triangle'" :size="18" />
        </span>
        <div class="lb-notice__body">
          <div class="lb-notice__title">
            <span>{{ a.target }}</span>
            <span class="lb-notice__cat">{{ alertCategory(a) }}</span>
          </div>
          <div class="lb-notice__text">{{ a.message }}</div>
        </div>
        <div class="lb-notice__actions">
          <a-button size="small" class="lb-btn-ghost" @click="router.push(alertGo(a).to)">{{ alertGo(a).label }}</a-button>
        </div>
      </div>
    </section>

    <!-- 指标条:一张卡内的分隔栅格,不是四张卡。 -->
    <section class="lb-metrics">
      <LbMetricCard
        label="有效用户"
        :state="metricState"
        :value="summary?.user_active"
        :total="summary?.user_total"
        :hint="
          summary
            ? summary.quota_exceeded + summary.expiring_soon
              ? `${summary.quota_exceeded + summary.expiring_soon} 人已过期或超额`
              : '没有人过期或超额'
            : undefined
        "
      />
      <LbMetricCard
        label="在线节点"
        :state="nodeMetricState"
        :value="summary?.node_online"
        :total="summary?.node_total"
        empty-hint="尚未添加节点"
        :tone="offlineCount ? 'danger' : 'default'"
      >
        <template #foot>
          <LbStatusTag v-if="offlineCount" kind="node" status="OFFLINE" :suffix="String(offlineCount)" />
          <LbStatusTag v-if="subOffCount" :meta="subscriptionOffMeta" :suffix="String(subOffCount)" />
          <span v-if="!offlineCount && !subOffCount && summary && summary.node_online < summary.node_total">
            {{ summary.node_total - summary.node_online }} 台待初始化或已禁用
          </span>
          <span v-else-if="!offlineCount && !subOffCount">全部正常运行</span>
        </template>
      </LbMetricCard>
      <LbMetricCard
        label="本月流量"
        :state="metricState"
        :value="summary ? formatBytes(summary.traffic_month).split(' ')[0] : undefined"
        :unit="summary ? formatBytes(summary.traffic_month).split(' ')[1] : undefined"
        :hint="summary ? `今日 ${formatBytes(summary.traffic_today)}` : undefined"
      />
      <LbMetricCard
        label="失败部署"
        tip="统计近 7 天。分母也取这个窗口,否则「2 / 共 100 次」里的 100 是几个月的量。"
        :state="deployError ? 'error' : loading ? 'loading' : 'ready'"
        :value="failedDeploys.length"
        :total="`${recentDeploys.length} 次`"
        :tone="failedDeploys.length ? 'danger' : 'default'"
      >
        <template #foot>
          <a v-if="failedDeploys.length" class="dv__link" @click="router.push('/deployments')">查看部署记录 ›</a>
          <span v-else>近 7 天没有失败的部署</span>
        </template>
      </LbMetricCard>
    </section>

    <!-- 趋势:面积图 -->
    <section class="lb-card dv__trend">
      <div class="dv__trend-head">
        <div>
          <div class="dv__trend-label">
            <span>{{ range }} 天流量趋势</span>
            <LbInfoTip text="按 UTC 日聚合,全站上下行合计。虚线跨过的日子没有记录 —— 不补 0 也不插值。悬停查看当日流量。" :width="300" />
          </div>
          <div v-if="peak" class="dv__trend-peak">
            <span class="dv__trend-peak-value lb-tabular">{{ formatBytes(peak.total) }}</span>
            <span class="dv__trend-peak-note">峰值 · {{ formatUTCDay(peak.day) }}</span>
          </div>
        </div>
        <!-- 范围切换用分段控件,不用下拉:三个选项摊开比藏起来快。 -->
        <div class="lb-seg lb-seg--sm" role="tablist">
          <button
            v-for="r in [7, 30, 90]"
            :key="r"
            type="button"
            role="tab"
            class="lb-seg__item"
            :class="{ 'lb-seg__item--on': range === r }"
            :aria-selected="range === r"
            @click="range = r"
          >
            {{ r }} 天
          </button>
        </div>
      </div>

      <LbEmptyState
        v-if="dailyError"
        variant="error"
        title="无法读取流量汇总"
        description="图表保持空白而不是画一条零线 —— 零线会被误读成「今天没人用」。"
        @retry="loadDaily"
      />
      <LbEmptyState
        v-else-if="!loading && daily.length === 0"
        variant="empty"
        title="这段时间没有流量记录"
        description="订阅还没有被任何客户端拉取过,或者流量同步尚未跑过一轮。"
      />
      <template v-else>
        <LbSparkline :points="points" type="line" :height="150" class="dv__spark" />
        <div class="dv__axis lb-tabular">
          <span v-for="(l, i) in axisLabels" :key="i">{{ l }}</span>
        </div>
      </template>
    </section>

    <div class="lb-grid-2 dv__cols">
      <section>
        <LbSectionTitle title="节点健康">
          <template #extra><a class="dv__link" @click="router.push('/nodes')">全部节点 ›</a></template>
        </LbSectionTitle>
        <div class="lb-card lb-card--flush">
          <LbEmptyState
            v-if="nodesError"
            variant="error"
            title="无法加载节点列表"
            description="不显示「暂无数据」—— 那会被读成一台机器都没有。"
            @retry="loadNodes"
          />
          <LbEmptyState
            v-else-if="!loading && nodes.length === 0"
            variant="empty"
            title="还没有任何节点"
            description="添加第一台 VPS 后,这里会显示节点健康与本周期流量。"
          >
            <template #action>
              <a-button type="primary" size="small" @click="router.push('/nodes')">添加节点</a-button>
            </template>
          </LbEmptyState>
          <template v-else>
            <div v-for="n in nodes" :key="n.id" class="dv__node">
              <div class="dv__node-main">
                <a class="dv__node-name" @click="router.push(`/nodes/${n.id}`)">{{ n.display_name }}</a>
                <div class="dv__node-sub">
                  <span class="lb-mono">{{ n.host }}</span>
                  <span> · 同步 </span>
                  <LbTimeText
                    :value="n.last_heartbeat_at"
                    :warn-after-ms="threshold.metricsStaleMs"
                    :danger-after-ms="threshold.metricsStaleMs * 6"
                    empty="从未"
                  />
                </div>
              </div>
              <!-- 运行与配置分两列:一台在跑旧配置、部署失败的机器,
                   挤在一格里只显示「部署失败」,看不出它其实还在服务用户。 -->
              <div class="dv__node-tags">
                <LbStatusTag kind="node" :status="n.status" />
                <LbStatusTag
                  v-for="(b, i) in nodeBadges(n, metrics[n.id]?.collected_at)"
                  :key="i"
                  :meta="b"
                />
              </div>
              <LbStatusTag :meta="configStatusMeta[configState(n)]" :suffix="`rev ${n.config_revision}`" />
              <div class="dv__node-quota">
                <LbQuotaBar
                  :used-bytes="cycles[n.id]?.used_bytes ?? null"
                  :quota-bytes="cycles[n.id]?.quota_bytes ?? n.traffic_quota_bytes"
                  :warning-level="cycles[n.id]?.warning_level"
                />
              </div>
            </div>
          </template>
        </div>
      </section>

      <section>
        <LbSectionTitle title="最近部署">
          <template #extra><a class="dv__link" @click="router.push('/deployments')">全部记录 ›</a></template>
        </LbSectionTitle>
        <div class="lb-card lb-card--flush">
          <LbEmptyState
            v-if="deployError"
            variant="error"
            title="无法加载部署记录"
            @retry="loadDeploys"
          />
          <LbEmptyState
            v-else-if="!loading && deploys.length === 0"
            variant="empty"
            title="还没有部署记录"
            description="添加节点并执行第一次部署后,这里会记录每一步的结果。"
          />
          <div v-else class="dv__deploys">
            <div v-for="d in deploys.slice(0, 6)" :key="d.id" class="dv__deploy">
              <LbStatusTag kind="deploy" :status="d.status" />
              <div class="dv__deploy-body">
                <div class="dv__deploy-title">
                  <span>{{ nodes.find((n) => n.id === d.node_id)?.display_name ?? `节点 ${d.node_id}` }}</span>
                  <span class="dv__deploy-rev lb-tabular">rev {{ d.revision }}</span>
                </div>
                <div class="dv__deploy-msg lb-clamp-2">{{ conclusion(d) }}</div>
                <div v-if="d.rollback_result" class="dv__deploy-rb">{{ d.rollback_result }}</div>
              </div>
              <LbTimeText :value="d.started_at" />
            </div>
          </div>
        </div>
      </section>
    </div>
  </div>
</template>

<style scoped>
.dv__alerts {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.dv__alert {
  align-items: center;
}

.dv__link {
  font-weight: 500;
}

.dv__trend {
  padding: 22px 28px 20px;
}

.dv__trend-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  margin-bottom: 16px;
}

.dv__trend-label {
  display: flex;
  align-items: center;
  gap: 4px;
  font-size: 13px;
  font-weight: 500;
  color: var(--text3);
}

.dv__trend-peak {
  display: flex;
  align-items: baseline;
  gap: 8px;
  margin-top: 6px;
}

.dv__trend-peak-value {
  font-size: 28px;
  font-weight: 700;
  letter-spacing: -0.03em;
}

.dv__trend-peak-note {
  font-size: 13px;
  color: var(--text3);
}

.dv__axis {
  display: flex;
  justify-content: space-between;
  margin-top: 6px;
  font-size: 11px;
  color: var(--text3);
}

.dv__cols {
  align-items: start;
}

.dv__node {
  display: grid;
  grid-template-columns: minmax(0, 1.3fr) auto auto minmax(120px, 1fr);
  align-items: center;
  gap: 18px;
  padding: 16px 22px;
  transition: background 0.15s;
}
.dv__node + .dv__node {
  border-top: 1px solid var(--sep2);
}
.dv__node:hover {
  background: var(--surface2);
}

.dv__node-main {
  min-width: 0;
}

.dv__node-name {
  font-size: 14px;
  font-weight: 600;
  color: var(--text);
}
.dv__node-name:hover {
  color: var(--brand);
}

.dv__node-sub {
  font-size: 12px;
  color: var(--text3);
  margin-top: 2px;
  display: flex;
  align-items: center;
  gap: 2px;
  flex-wrap: wrap;
}
.dv__node-sub :deep(.lb-time) {
  font-size: 12px;
}

.dv__node-tags {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
  justify-content: flex-end;
}

.dv__node-quota {
  min-width: 0;
}

.dv__deploys {
  display: flex;
  flex-direction: column;
}

.dv__deploy {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr) auto;
  align-items: start;
  gap: 12px;
  padding: 14px 20px;
}

.dv__deploy + .dv__deploy {
  border-top: 1px solid var(--sep2);
}

.dv__deploy-body {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.dv__deploy-title {
  display: flex;
  align-items: baseline;
  gap: 8px;
  font-size: 13.5px;
  font-weight: 600;
}

.dv__deploy-rev {
  font-size: 11.5px;
  font-weight: 400;
  color: var(--text3);
}

.dv__deploy-msg {
  font-size: 12.5px;
  line-height: 1.6;
  color: var(--text2);
}

.dv__deploy-rb {
  font-size: 11.5px;
  color: var(--warn);
}

@media (max-width: 1023px) {
  .dv__node {
    grid-template-columns: minmax(0, 1fr) auto;
    row-gap: 10px;
  }
  .dv__node-quota {
    grid-column: 1 / -1;
  }
}

@media (max-width: 767px) {
  .dv__trend {
    padding: 16px 16px 14px;
  }
  .dv__alert {
    flex-wrap: wrap;
  }
  .dv__node {
    padding: 14px 16px;
  }
}
</style>
