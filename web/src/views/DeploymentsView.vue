<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { api, ApiError, type DeploymentRecord, type Node } from '@/api/client'
import { formatDuration } from '@/utils/format'
import DeployStepList from '@/components/DeployStepList.vue'
import {
  LbEmptyState,
  LbFilterBar,
  LbMetricCard,
  LbRowCard,
  LbStatusTag,
  LbTimeText,
} from '@/components/lb'
import { useNarrow } from '@/composables/useNarrow'
import { usePagination } from '@/composables/usePagination'

/**
 * 部署记录:按对象回溯 —— 某台机器上发生了什么。
 *
 * 「结论」列取代了原来那一整段 error_message + word-break: break-all。
 * 一条 SSH 错误能把行撑到四行高,而读完还是不知道现在节点上跑的是哪个版本 ——
 * 那才是管理员唯一想知道的事。改成固定两行:
 *   第一行 失败在哪一步 + 一句原因
 *   第二行 回滚结果与当前生效版本
 * 哈希退到展开区,它只在做配置比对时才有用。
 */
const narrow = useNarrow()
/** 窄屏下用弹窗展示步骤时间线,替代桌面的展开行。 */
const stepsOf = ref<DeploymentRecord | null>(null)
const records = ref<DeploymentRecord[]>([])
const nodes = ref<Node[]>([])
const loading = ref(true)
const loadError = ref<{ message: string; status?: number; at: string } | null>(null)

const nodeName = (id: number) => {
  const n = nodes.value.find((x) => x.id === id)
  return n ? n.display_name || n.name : `节点 ${id}`
}
const nodeHost = (id: number) => nodes.value.find((x) => x.id === id)?.host ?? ''

// ---------- 筛选 ----------

const blankFilters = {
  nodeID: undefined as number | undefined,
  status: undefined as string | undefined,
  days: undefined as number | undefined,
  onlyFailed: false,
}
const filters = reactive({ ...blankFilters })

const activeFilterCount = computed(
  () =>
    (filters.nodeID !== undefined ? 1 : 0) +
    (filters.status !== undefined ? 1 : 0) +
    (filters.days !== undefined ? 1 : 0) +
    (filters.onlyFailed ? 1 : 0),
)

function clearFilters() {
  Object.assign(filters, blankFilters)
}

const failed = (r: DeploymentRecord) => r.status === 'FAILED' || r.status === 'ROLLED_BACK'

const visible = computed(() =>
  records.value.filter((r) => {
    if (filters.nodeID !== undefined && r.node_id !== filters.nodeID) return false
    if (filters.status !== undefined && r.status !== filters.status) return false
    if (filters.onlyFailed && !failed(r)) return false
    if (filters.days !== undefined) {
      const since = Date.now() - filters.days * 86400000
      if (new Date(r.started_at).getTime() < since) return false
    }
    return true
  }),
)

// ---------- 指标 ----------

const durationMs = (r: DeploymentRecord) =>
  r.finished_at ? new Date(r.finished_at).getTime() - new Date(r.started_at).getTime() : null

const stats = computed(() => {
  const since = Date.now() - 7 * 86400000
  const w = records.value.filter((r) => new Date(r.started_at).getTime() >= since)
  const f = w.filter(failed)
  // 回滚成功率的分母只算「已经替换过配置」的那些失败 ——
  // 步骤 1~3 就失败的根本没有可回滚的东西,算进去会把成功率稀释成一个假数字。
  const rollbackable = f.filter((r) => r.rollback_result !== '')
  const ok = rollbackable.filter((r) => r.rollback_result.includes('成功'))
  const durations = w
    .map(durationMs)
    .filter((d): d is number => d !== null)
    .sort((a, b) => a - b)
  return {
    total: w.length,
    failed: f.length,
    rollbackOK: ok.length,
    rollbackTotal: rollbackable.length,
    median: durations.length ? durations[Math.floor(durations.length / 2)] : null,
  }
})

const metricState = computed(() =>
  loadError.value ? 'error' : loading.value ? 'loading' : records.value.length ? 'ready' : 'empty',
)

// ---------- 结论 ----------

interface Verdict {
  line1: string
  line2?: string
  tone?: 'warn' | 'error'
}

/**
 * 失败要分两种说法:
 *   步骤 4 之后失败 → 已经替换过配置,必然伴随回滚,第二行写回滚结果;
 *   步骤 1~3 失败   → 没有可回滚的东西,必须写明「未替换配置,无需回滚」,
 *                    否则会被当成回滚漏了。
 */
function verdict(r: DeploymentRecord): Verdict {
  const steps = r.steps ?? []
  const idx = steps.findIndex((s) => s.status === 'FAILED')
  const failedStep = idx >= 0 ? steps[idx] : null

  if (r.status === 'SUCCESS') {
    return { line1: `${steps.length} 步全部通过 · 新配置已生效` }
  }
  if (r.status === 'RUNNING') {
    const done = steps.filter((s) => s.status === 'SUCCESS').length
    return { line1: `步骤 ${done + 1} / ${steps.length || '?'} 进行中` }
  }

  // 后端的 error_message 常常已经带上了「步骤 N ...」前缀,再拼一次会得到
  // 「步骤 6 三步健康检查 · 步骤 6 三步健康检查失败:…」。去掉重复的那一段。
  const raw = (r.error_message || failedStep?.detail || '失败').split('\n')[0]
  const reason = failedStep ? raw.replace(/^步骤\s*\d+\s*/, '').replace(failedStep.name, '').replace(/^[\s:：·-]+/, '') : raw
  const head = failedStep
    ? `步骤 ${idx + 1} ${failedStep.name} · ${reason || '失败'}`
    : raw

  if (r.rollback_result) {
    return { line1: head, line2: `${r.rollback_result},节点在跑旧配置`, tone: 'warn' }
  }
  // 配置替换发生在第 4 步。在此之前失败的,节点上什么都没动过。
  const beforeReplace = idx >= 0 && idx < 3
  return {
    line1: head,
    line2: beforeReplace ? '未替换配置,节点保持原版本 · 无需回滚' : undefined,
    tone: 'error',
  }
}

// ---------- 取数 ----------

async function load() {
  loading.value = true
  loadError.value = null
  try {
    const [d, n] = await Promise.all([api.deployments(200), api.nodes()])
    records.value = d.items
    nodes.value = n.items
  } catch (err) {
    loadError.value = {
      message: err instanceof ApiError ? err.message : '加载部署记录失败',
      status: err instanceof ApiError ? err.status : undefined,
      at: new Date().toLocaleTimeString(),
    }
    records.value = []
  } finally {
    loading.value = false
  }
}

onMounted(load)

const pager = usePagination('deployments', () => visible.value.length)

const columns = [
  { title: '节点', key: 'node', width: 190 },
  { title: 'rev', key: 'rev', width: 70 },
  { title: '结果', key: 'status', width: 120 },
  { title: '开始时间', key: 'time', width: 150 },
  { title: '耗时', key: 'cost', width: 80 },
  { title: '结论', key: 'verdict' },
]
</script>

<template>
  <div class="dp">
    <div class="dp__head">
      <div>
        <h2 class="dp__title">部署记录</h2>
        <div class="dp__sub">最近 200 次 · 时间为本地时区,悬停显示 UTC · 点击行展开步骤时间线</div>
      </div>
      <a-button :loading="loading" @click="load">刷新</a-button>
    </div>

    <div class="dp__metrics">
      <LbMetricCard label="近 7 天部署" :state="metricState" :value="stats.total" unit="次" />
      <LbMetricCard
        label="失败"
        :state="metricState"
        :value="stats.failed"
        :tone="stats.failed ? 'danger' : 'default'"
      >
        <template #action>
          <a v-if="stats.failed" @click="filters.onlyFailed = true">只看失败</a>
        </template>
      </LbMetricCard>
      <LbMetricCard
        label="自动回滚成功率"
        :state="metricState"
        :value="stats.rollbackOK"
        :total="stats.rollbackTotal"
        hint="分母只算已替换过配置的失败"
      />
      <LbMetricCard
        label="中位耗时"
        :state="metricState"
        :value="stats.median === null ? '—' : (stats.median / 1000).toFixed(1)"
        unit="秒"
      />
    </div>

    <a-card :body-style="{ padding: 0 }">
      <LbFilterBar
        :active-count="activeFilterCount"
        :filtered="visible.length"
        :total="records.length"
        @clear="clearFilters"
      >
        <a-select v-model:value="filters.nodeID" placeholder="节点" allow-clear style="width: 160px">
          <a-select-option v-for="n in nodes" :key="n.id" :value="n.id">
            {{ n.display_name || n.name }}
          </a-select-option>
        </a-select>
        <a-select v-model:value="filters.status" placeholder="结果" allow-clear style="width: 130px">
          <a-select-option value="SUCCESS">成功</a-select-option>
          <a-select-option value="FAILED">失败</a-select-option>
          <a-select-option value="ROLLED_BACK">已回滚</a-select-option>
          <a-select-option value="RUNNING">进行中</a-select-option>
        </a-select>
        <a-select v-model:value="filters.days" placeholder="时间范围" allow-clear style="width: 130px">
          <a-select-option :value="1">近 24 小时</a-select-option>
          <a-select-option :value="7">近 7 天</a-select-option>
          <a-select-option :value="30">近 30 天</a-select-option>
        </a-select>
        <a-checkbox v-model:checked="filters.onlyFailed">只看失败</a-checkbox>
      </LbFilterBar>

      <LbEmptyState
        v-if="loadError"
        variant="error"
        :title="loadError.message"
        description="分页器与筛选条保持可见,重试后回到原来那一页。"
        :http-status="loadError.status"
        :occurred-at="loadError.at"
        @retry="load"
      />
      <LbEmptyState
        v-else-if="!loading && records.length === 0"
        variant="empty"
        title="还没有部署记录"
        description="添加节点并执行第一次部署后,这里会记录每一步的结果。"
      />
      <LbEmptyState
        v-else-if="!loading && visible.length === 0"
        variant="filtered"
        title="没有符合条件的记录"
        :description="`当前有 ${activeFilterCount} 项筛选生效,${records.length} 条记录被筛掉。`"
        @clear="clearFilters"
      />

      <!--
        <768 换卡片。「展开行」在小屏改成整页跳转的「查看步骤」——
        手风琴会把上下文推到看不见的地方。这里用弹窗代替跳转:
        步骤时间线是一次性看完就走的东西,跳一页还要再退回来。
      -->
      <div v-else-if="narrow" class="dp__cards">
        <LbRowCard v-for="r in pager.slice(visible)" :key="r.id" :danger="failed(r)">
          <template #head>
            <span class="dp__card-name">{{ nodeName(r.node_id) }}</span>
            <span class="lb-mono dp__card-rev">rev {{ r.revision }}</span>
            <LbStatusTag kind="deploy" :status="r.status" />
          </template>

          <div class="dp__v1 lb-clamp-2">{{ verdict(r).line1 }}</div>
          <div
            v-if="verdict(r).line2"
            class="dp__v2"
            :class="verdict(r).tone === 'warn' ? 'dp__v2--warn' : 'dp__v2--error'"
          >
            {{ verdict(r).line2 }}
          </div>
          <div class="dp__host">
            <LbTimeText :value="r.started_at" mode="both" /> ·
            {{ durationMs(r) === null ? '—' : formatDuration(durationMs(r) as number) }}
          </div>

          <template #foot>
            <a-button @click="stepsOf = r">查看步骤</a-button>
          </template>
        </LbRowCard>

        <a-pagination
          v-if="visible.length > pager.pageSize.value"
          v-model:current="pager.current.value"
          :page-size="pager.pageSize.value"
          :total="visible.length"
          :show-size-changer="false"
          simple
          class="dp__pager"
        />
      </div>

      <a-table
        v-else
        :columns="columns"
        :data-source="visible"
        :loading="loading"
        row-key="id"
        size="small"
        :pagination="pager.options.value"
        :scroll="{ x: 1000 }"
      >
        <template #expandedRowRender="{ record }">
          <DeployStepList :record="record" />
        </template>

        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'node'">
            <div>{{ nodeName(record.node_id) }}</div>
            <div class="dp__host lb-mono">{{ nodeHost(record.node_id) }}</div>
          </template>
          <template v-else-if="column.key === 'rev'">
            <span class="lb-mono">{{ record.revision }}</span>
          </template>
          <template v-else-if="column.key === 'status'">
            <LbStatusTag kind="deploy" :status="record.status" />
          </template>
          <template v-else-if="column.key === 'time'">
            <LbTimeText :value="record.started_at" mode="both" />
          </template>
          <template v-else-if="column.key === 'cost'">
            <span class="lb-mono">
              {{ durationMs(record) === null ? '—' : formatDuration(durationMs(record) as number) }}
            </span>
          </template>
          <template v-else-if="column.key === 'verdict'">
            <div class="dp__v1 lb-clamp-2">{{ verdict(record).line1 }}</div>
            <div
              v-if="verdict(record).line2"
              class="dp__v2"
              :class="verdict(record).tone === 'warn' ? 'dp__v2--warn' : 'dp__v2--error'"
            >
              {{ verdict(record).line2 }}
            </div>
          </template>
        </template>
      </a-table>
    </a-card>

    <a-modal
      :open="stepsOf !== null"
      :title="stepsOf ? `${nodeName(stepsOf.node_id)} · rev ${stepsOf.revision}` : ''"
      :width="560"
      :footer="null"
      @update:open="(v: boolean) => { if (!v) stepsOf = null }"
    >
      <DeployStepList v-if="stepsOf" :record="stepsOf" />
    </a-modal>
  </div>
</template>

<style scoped>
.dp {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.dp__head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
}

.dp__title {
  margin: 0;
  font-size: 19px;
  font-weight: 600;
}

.dp__sub {
  margin-top: 3px;
  font-size: 12.5px;
  color: #6b7480;
}

.dp__metrics {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 16px;
}

.dp__host {
  font-size: 11px;
  color: #6b7480;
}

.dp__cards {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 12px;
}

.dp__card-name {
  flex: 1;
  min-width: 0;
  font-size: 14px;
  font-weight: 600;
}

.dp__card-rev {
  font-size: 11.5px;
  color: #6b7480;
}

.dp__pager {
  align-self: center;
  padding: 4px 0 2px;
}

.dp__v1 {
  font-size: 12.5px;
  line-height: 1.6;
}

.dp__v2 {
  margin-top: 2px;
  font-size: 11px;
}

.dp__v2--warn {
  color: #92610a;
}

.dp__v2--error {
  color: #6b7480;
}

@media (max-width: 1279px) {
  .dp__metrics {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}

@media (max-width: 767px) {
  .dp__metrics {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
