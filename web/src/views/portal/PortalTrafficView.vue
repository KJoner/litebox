<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { portalApi, ApiError, type PortalDashboard, type PortalTraffic } from '@/api/client'
import { formatBytes, formatQuota } from '@/utils/format'
import { LbEmptyState, LbQuotaBar, LbSparkline, LbTimeText, type LbPoint } from '@/components/lb'

/**
 * 我的流量。只有 7 与 30 两档 —— 后端也只接受这两个值,
 * 前端多给一档只会拿到回落后的 30 天,用户会以为页面坏了。
 *
 * 额度卡来自另一个接口(概览),所以图表读不到时它照常显示。
 */
const data = ref<PortalTraffic | null>(null)
const quota = ref<PortalDashboard | null>(null)
const loading = ref(true)
const chartError = ref('')
const days = ref(30)

async function loadChart() {
  loading.value = true
  chartError.value = ''
  try {
    data.value = await portalApi.traffic(days.value)
  } catch (err) {
    chartError.value = err instanceof ApiError ? err.message : '暂时读不到流量数据'
    data.value = null
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  // 额度卡与图表分开取:图表挂了额度还得看得见。
  portalApi
    .dashboard()
    .then((d) => (quota.value = d))
    .catch(() => (quota.value = null))
  await loadChart()
})

// 切换 7 / 30 天时只换柱子,标题与合计数字保留上一次的值,不整块闪白。
watch(days, loadChart)

const warningLevel = computed(() => {
  const q = quota.value
  if (!q) return undefined
  if (q.quota_bytes <= 0) return 'UNLIMITED' as const
  const p = q.used_percent ?? 0
  if (p >= 100) return 'EXCEEDED' as const
  if (p >= 95) return 'DANGER' as const
  if (p >= 80) return 'WARNING' as const
  return 'NORMAL' as const
})

/**
 * 缺失的日子传 null:空心柱表示未知,不是 0。
 * 补 0 会让当天看起来「没人用」,而那一天的流量其实也没有计入额度。
 */
const points = computed<LbPoint[]>(() => {
  const byDay = new Map(data.value?.daily.map((d) => [d.day, d.total]) ?? [])
  const out: LbPoint[] = []
  for (let i = days.value - 1; i >= 0; i--) {
    const key = new Date(Date.now() - i * 86400000).toISOString().slice(0, 10)
    out.push({ at: key, value: byDay.has(key) ? (byDay.get(key) as number) : null })
  }
  return out
})

const hasGap = computed(() => points.value.some((p) => p.value === null))

const axisLabels = computed(() => {
  const p = points.value
  if (p.length < 2) return []
  return [0, Math.floor(p.length / 2), p.length - 1].map((i) => p[i].at.slice(5))
})

const maxShare = computed(() =>
  Math.max(1, ...(data.value?.by_node.map((n) => n.percent) ?? [0])),
)
</script>

<template>
  <div class="pt">
    <div class="pt__head">
      <div>
        <h2 class="pt__title">我的流量</h2>
        <div class="pt__sub">
          按 UTC 日统计<template v-if="quota?.next_reset_at">
            · 额度在 <LbTimeText :value="quota.next_reset_at" mode="cycle" /> 重置</template
          >
        </div>
      </div>
      <a-segmented
        v-model:value="days"
        :options="[
          { label: '最近 7 天', value: 7 },
          { label: '最近 30 天', value: 30 },
        ]"
        size="small"
      />
    </div>

    <div
      v-if="quota && quota.used_percent !== null && quota.used_percent >= 80"
      class="pt__alert"
      :class="quota.used_percent >= 100 ? 'pt__alert--error' : 'pt__alert--warn'"
    >
      已用 {{ formatBytes(quota.used_total) }},占额度的 {{ Math.round(quota.used_percent) }}%。
      <template v-if="quota.used_percent >= 100">
        账号已自动停用,客户端会连不上。
      </template>
      <template v-else> 用满后账号会自动停用,届时客户端会连不上。 </template>
      <template v-if="quota.next_reset_at">
        额度将在 <LbTimeText :value="quota.next_reset_at" mode="cycle" /> 重置并自动恢复。
      </template>
      需要提前增加额度请联系管理员。
    </div>

    <section v-if="quota" class="pt__card">
      <div class="pt__card-body">
        <div class="pt__totals">
          <div class="pt__total">
            <span>合计</span>
            <b class="lb-mono">{{ formatBytes(quota.used_total) }}</b>
            <em>/ {{ formatQuota(quota.quota_bytes) }}</em>
          </div>
          <div class="pt__total">
            <span>上行</span>
            <b class="lb-mono">{{ formatBytes(quota.used_uplink) }}</b>
          </div>
          <div class="pt__total">
            <span>下行</span>
            <b class="lb-mono">{{ formatBytes(quota.used_downlink) }}</b>
          </div>
        </div>
        <LbQuotaBar
          :used-bytes="quota.used_total"
          :quota-bytes="quota.quota_bytes"
          :warning-level="warningLevel"
          size="md"
          :show-value="false"
        />
        <div class="pt__quota-foot">
          <span v-if="quota.used_percent === null">不限量</span>
          <span v-else>剩余 {{ formatBytes(quota.remaining) }}</span>
          <span v-if="quota.next_reset_at">
            · <LbTimeText :value="quota.next_reset_at" mode="cycle" /> 重置
          </span>
        </div>
      </div>
    </section>

    <section class="pt__card">
      <div class="pt__card-head">
        <span>每日用量</span>
        <span class="pt__card-note">按 UTC 日</span>
      </div>
      <div class="pt__card-body">
        <LbEmptyState
          v-if="chartError"
          variant="error"
          title="暂时读不到流量数据"
          @retry="loadChart"
        />
        <LbEmptyState
          v-else-if="!loading && data && data.total === 0"
          variant="empty"
          title="这段时间没有流量记录"
          description="订阅还没有被任何客户端拉取过,或者你还没有开始使用。"
        />
        <template v-else>
          <LbSparkline :points="points" type="bar" :height="130" />
          <div class="pt__axis lb-mono">
            <span v-for="(l, i) in axisLabels" :key="i">{{ l }}</span>
          </div>
          <div class="pt__gap-note">把鼠标放到柱子上,可以看到那一天用了多少。</div>
          <div v-if="hasGap" class="pt__gap-note">
            空心柱表示那天没有统计记录(不是 0)。这些天的流量也没有计入你的额度。
          </div>
        </template>
      </div>
    </section>

    <section class="pt__card">
      <div class="pt__card-head"><span>按节点</span></div>
      <div class="pt__card-body">
        <LbEmptyState
          v-if="!data || data.by_node.length === 0"
          variant="empty"
          title="还没有按节点的流量"
          description="连上任意一个节点之后,这里会按节点分开统计。"
        />
        <!-- 五列表在 390 宽下横向滚动会把「占比」推出屏幕,所以每节点一块。 -->
        <div v-else class="pt__nodes">
          <div v-for="n in data.by_node" :key="n.node_id" class="pt__node">
            <div class="pt__node-head">
              <span class="lb-ellipsis">{{ n.display_name }}</span>
              <span class="lb-mono pt__node-total">{{ formatBytes(n.total) }}</span>
              <span class="lb-mono pt__node-pct">{{ n.percent.toFixed(0) }}%</span>
            </div>
            <div class="pt__node-track">
              <div class="pt__node-fill" :style="{ width: (n.percent / maxShare) * 100 + '%' }" />
            </div>
            <div class="pt__node-dir lb-mono">
              ↑ {{ formatBytes(n.uplink) }} · ↓ {{ formatBytes(n.downlink) }}
            </div>
          </div>
        </div>
      </div>
    </section>
  </div>
</template>

<style scoped>
.pt {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.pt__head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}

.pt__title {
  margin: 0;
  font-size: 19px;
  font-weight: 600;
}

.pt__sub {
  margin-top: 3px;
  font-size: 12.5px;
  color: #6b7480;
}

.pt__alert {
  padding: 11px 14px;
  border: 1px solid;
  border-radius: 8px;
  font-size: 12.5px;
  line-height: 1.8;
}

.pt__alert--warn {
  background: #fcf3e3;
  border-color: #efdcb4;
  color: #5c4405;
}

.pt__alert--error {
  background: #fdecea;
  border-color: #f3cfc9;
  color: #8e2117;
}

.pt__card {
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

.pt__card-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 16px;
  border-bottom: 1px solid #edeff2;
  font-size: 13px;
  font-weight: 600;
}

.pt__card-note {
  font-size: 11.5px;
  font-weight: 400;
  color: #6b7480;
}

.pt__card-body {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 16px;
}

.pt__totals {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 12px;
}

.pt__total {
  display: flex;
  flex-direction: column;
  gap: 3px;
  min-width: 0;
}

.pt__total span {
  font-size: 11.5px;
  color: #6b7480;
}

.pt__total b {
  font-size: 20px;
  font-weight: 600;
  letter-spacing: -0.01em;
}

.pt__total em {
  font-style: normal;
  font-size: 11.5px;
  color: #6b7480;
}

.pt__quota-foot {
  font-size: 11.5px;
  color: #6b7480;
}

.pt__axis {
  display: flex;
  justify-content: space-between;
  font-size: 10.5px;
  color: #6b7480;
}

.pt__gap-note {
  font-size: 11.5px;
  line-height: 1.7;
  color: #6b7480;
}

.pt__nodes {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.pt__node {
  display: flex;
  flex-direction: column;
  gap: 5px;
}

.pt__node-head {
  display: flex;
  align-items: baseline;
  gap: 10px;
  font-size: 12.5px;
}

.pt__node-head > span:first-child {
  flex: 1;
  min-width: 0;
}

.pt__node-total {
  font-weight: 500;
}

.pt__node-pct {
  width: 40px;
  text-align: right;
  color: #6b7480;
}

.pt__node-track {
  height: 6px;
  background: #edeff2;
  border-radius: 2px;
  overflow: hidden;
}

.pt__node-fill {
  height: 6px;
  background: #2563b8;
  border-radius: 2px;
}

.pt__node-dir {
  font-size: 11px;
  color: #6b7480;
}

@media (max-width: 767px) {
  .pt__totals {
    grid-template-columns: minmax(0, 1fr);
    gap: 8px;
  }

  .pt__total {
    flex-direction: row;
    align-items: baseline;
    gap: 8px;
  }

  .pt__total b {
    font-size: 15px;
  }
}
</style>
