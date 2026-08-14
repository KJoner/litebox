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
import { formatBytes } from '@/utils/format'
import {
  LbEmptyState,
  LbMetricCard,
  LbQuotaBar,
  LbSparkline,
  LbStatusTag,
  LbTimeText,
  configStatusMeta,
  type LbPoint,
} from '@/components/lb'
import { configState, nodeBadges } from '@/components/lb/derive'
import { threshold } from '@/theme/tokens'

/**
 * 仪表盘要回答的只有一句话:今天有没有事。
 *
 * 因此四块内容各自独立取数、各自降级 —— 现有实现是一个 Promise.all 加一个
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
  const vals = daily.value.map((d) => d.total)
  return vals.length ? Math.max(...vals) : 0
})

const monthStart = computed(() => {
  const d = new Date()
  return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, '0')}-01`
})

const nodeColumns = [
  { title: '节点', key: 'node', width: 230 },
  { title: '运行状态', key: 'run', width: 150 },
  { title: '配置状态', key: 'config', width: 160 },
  { title: '最后同步', key: 'sync', width: 110 },
  { title: '本周期流量', key: 'cycle', width: 180 },
]

/** 部署记录的一行结论。失败要分两种说法 —— 见 DeploymentsView 里的说明。 */
function conclusion(d: DeploymentRecord): string {
  if (d.status === 'SUCCESS') return `成功 · ${d.steps?.length ?? 0} 步`
  if (d.status === 'RUNNING') {
    const done = (d.steps ?? []).filter((s) => s.status !== 'SKIPPED').length
    return `部署中 · 步骤 ${done}/${d.steps?.length ?? '?'}`
  }
  return d.error_message || '失败'
}
</script>

<template>
  <div class="dv">
    <div class="dv__head">
      <div>
        <h2 class="dv__title">仪表盘</h2>
        <div class="dv__sub">
          周期边界均为 UTC 00:00 · 本月自 {{ monthStart }} 起 · 节点额度只预警,不会停服
        </div>
      </div>
      <a-button :loading="loading" @click="load">刷新</a-button>
    </div>

    <!-- 系统告警。0 条时整块不出现 —— 不要给「今天没事」也占一块版面。 -->
    <section v-if="alerts.length" class="dv__alerts">
      <div class="dv__alerts-head">系统告警 · {{ alerts.length }} 条待处理</div>
      <div
        v-for="(a, i) in alerts"
        :key="i"
        class="dv__alert"
        :class="a.level === 'error' ? 'dv__alert--error' : 'dv__alert--warn'"
      >
        <span class="dv__alert-cat">{{ a.category === 'user' ? '用户' : '节点' }}</span>
        <span class="dv__alert-target">{{ a.target }}</span>
        <span class="dv__alert-msg">{{ a.message }}</span>
        <a class="dv__alert-go" @click="router.push(a.category === 'user' ? '/users' : '/nodes')">
          {{ a.category === 'user' ? '查看用户' : '查看节点' }}
        </a>
      </div>
    </section>

    <div class="dv__metrics">
      <LbMetricCard
        label="有效用户"
        :state="metricState"
        :value="summary?.user_active"
        :total="summary?.user_total"
        :hint="
          summary
            ? `已过期与超额共 ${summary.quota_exceeded + summary.expiring_soon} 人待处理`
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
          <LbStatusTag
            v-if="subOffCount"
            :meta="{ text: '停发订阅', shape: 'pause', fg: '#5F52A0', bg: '#F0EEF9', bd: '#D6D0EE' }"
            :suffix="String(subOffCount)"
          />
          <span v-if="!offlineCount && !subOffCount">全部正常运行</span>
        </template>
      </LbMetricCard>
      <LbMetricCard
        label="本月流量"
        :state="metricState"
        :value="summary ? formatBytes(summary.traffic_month) : undefined"
        :hint="summary ? `今日 ${formatBytes(summary.traffic_today)} · 按 UTC 日` : undefined"
      />
      <LbMetricCard
        label="失败部署(近 7 天)"
        :state="deployError ? 'error' : loading ? 'loading' : 'ready'"
        :value="failedDeploys.length"
        :total="`共 ${recentDeploys.length} 次`"
        :tone="failedDeploys.length ? 'danger' : 'default'"
      >
        <template #foot>
          <a v-if="failedDeploys.length" @click="router.push('/deployments')">查看部署记录</a>
          <span v-else>近 7 天没有失败的部署</span>
        </template>
      </LbMetricCard>
    </div>

    <a-card :body-style="{ padding: '16px' }">
      <template #title>
        <span class="dv__card-title">{{ range }} 天流量趋势</span>
        <span class="dv__card-note">按 UTC 日聚合 · 全站上下行合计</span>
      </template>
      <template #extra>
        <!-- 范围切换用分段控件,不用下拉:三个选项摊开比藏起来快。 -->
        <a-segmented
          v-model:value="range"
          :options="[
            { label: '7 天', value: 7 },
            { label: '30 天', value: 30 },
            { label: '90 天', value: 90 },
          ]"
          size="small"
        />
      </template>

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
        <LbSparkline :points="points" type="line" :height="140" />
        <div class="dv__axis lb-mono">
          <span v-for="(l, i) in axisLabels" :key="i">{{ l }}</span>
        </div>
        <div class="dv__axis-note">
          峰值 {{ formatBytes(peak) }} · 悬停查看当日流量 · 虚线跨过的日子没有记录,不补 0 也不插值
        </div>
      </template>
    </a-card>

    <div class="dv__cols">
      <a-card :body-style="{ padding: 0 }">
        <template #title><span class="dv__card-title">节点健康</span></template>
        <template #extra><a @click="router.push('/nodes')">全部节点 →</a></template>

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
        <a-table
          v-else
          :columns="nodeColumns"
          :data-source="nodes"
          :loading="loading"
          :pagination="false"
          row-key="id"
          size="small"
          :scroll="{ x: 830 }"
        >
          <template #bodyCell="{ column, record }">
            <template v-if="column.key === 'node'">
              <a @click="router.push('/nodes')">{{ record.display_name }}</a>
              <div class="dv__node-sub lb-mono">
                {{ record.host }} · {{ record.access_tier_name }}
              </div>
            </template>

            <!-- 运行与配置分两列:一台在跑旧配置、部署失败的机器,
                 挤在一格里只显示「部署失败」,看不出它其实还在服务用户。 -->
            <template v-else-if="column.key === 'run'">
              <div class="dv__stack">
                <LbStatusTag kind="node" :status="record.status" />
                <LbStatusTag
                  v-for="(b, i) in nodeBadges(record, metrics[record.id]?.collected_at)"
                  :key="i"
                  :meta="b"
                />
              </div>
            </template>

            <template v-else-if="column.key === 'config'">
              <LbStatusTag
                :meta="configStatusMeta[configState(record)]"
                :suffix="`rev ${record.config_revision}`"
              />
            </template>

            <template v-else-if="column.key === 'sync'">
              <LbTimeText
                :value="record.last_heartbeat_at"
                :warn-after-ms="threshold.metricsStaleMs"
                :danger-after-ms="threshold.metricsStaleMs * 6"
              />
            </template>

            <template v-else-if="column.key === 'cycle'">
              <LbQuotaBar
                :used-bytes="cycles[record.id]?.used_bytes ?? null"
                :quota-bytes="cycles[record.id]?.quota_bytes ?? record.traffic_quota_bytes"
                :warning-level="cycles[record.id]?.warning_level"
              />
            </template>
          </template>
        </a-table>
      </a-card>

      <a-card :body-style="{ padding: 0 }">
        <template #title><span class="dv__card-title">最近部署</span></template>
        <template #extra><a @click="router.push('/deployments')">全部记录 →</a></template>

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
                <span class="lb-mono dv__deploy-rev">rev {{ d.revision }}</span>
              </div>
              <div class="dv__deploy-msg lb-clamp-2">{{ conclusion(d) }}</div>
              <div v-if="d.rollback_result" class="dv__deploy-rb">{{ d.rollback_result }}</div>
            </div>
            <LbTimeText :value="d.started_at" />
          </div>
        </div>
      </a-card>
    </div>
  </div>
</template>

<style scoped>
.dv {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.dv__head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
}

.dv__title {
  margin: 0;
  font-size: 19px;
  font-weight: 600;
}

.dv__sub {
  margin-top: 3px;
  font-size: 12.5px;
  color: #6b7480;
}

.dv__alerts {
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
  overflow: hidden;
}

.dv__alerts-head {
  padding: 11px 16px;
  border-bottom: 1px solid #edeff2;
  font-size: 13px;
  font-weight: 600;
}

.dv__alert {
  display: flex;
  align-items: baseline;
  gap: 10px;
  padding: 10px 16px;
  font-size: 12.5px;
}

.dv__alert + .dv__alert {
  border-top: 1px solid #edeff2;
}

.dv__alert-cat {
  flex: none;
  padding: 1px 6px;
  border-radius: 3px;
  font-size: 11px;
  font-weight: 500;
}

.dv__alert--error .dv__alert-cat {
  background: #fdecea;
  color: #b4291d;
}

.dv__alert--warn .dv__alert-cat {
  background: #fcf3e3;
  color: #92610a;
}

.dv__alert-target {
  flex: none;
  font-weight: 500;
}

.dv__alert-msg {
  flex: 1;
  min-width: 0;
  color: #576070;
  line-height: 1.6;
}

.dv__alert-go {
  flex: none;
  font-size: 12px;
}

.dv__metrics {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 16px;
}

.dv__card-title {
  font-size: 13px;
  font-weight: 600;
}

.dv__card-note {
  margin-left: 10px;
  font-size: 11.5px;
  font-weight: 400;
  color: #6b7480;
}

.dv__axis {
  display: flex;
  justify-content: space-between;
  margin-top: 4px;
  font-size: 10.5px;
  color: #6b7480;
}

.dv__axis-note {
  margin-top: 6px;
  font-size: 11px;
  color: #6b7480;
}

.dv__cols {
  display: grid;
  grid-template-columns: minmax(0, 1.6fr) minmax(0, 1fr);
  gap: 16px;
  align-items: start;
}

.dv__node-sub {
  font-size: 11px;
  color: #6b7480;
}

.dv__stack {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 3px;
}

.dv__deploys {
  display: flex;
  flex-direction: column;
}

.dv__deploy {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr) auto;
  align-items: start;
  gap: 10px;
  padding: 11px 16px;
}

.dv__deploy + .dv__deploy {
  border-top: 1px solid #edeff2;
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
  font-size: 12.5px;
  font-weight: 500;
}

.dv__deploy-rev {
  font-size: 11px;
  font-weight: 400;
  color: #6b7480;
}

.dv__deploy-msg {
  font-size: 11.5px;
  line-height: 1.6;
  color: #576070;
}

.dv__deploy-rb {
  font-size: 11px;
  color: #92610a;
}

@media (max-width: 1279px) {
  .dv__metrics {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }

  .dv__cols {
    grid-template-columns: minmax(0, 1fr);
  }
}

@media (max-width: 767px) {
  .dv__metrics {
    grid-template-columns: minmax(0, 1fr);
  }

  .dv__alert {
    flex-wrap: wrap;
  }
}
</style>
