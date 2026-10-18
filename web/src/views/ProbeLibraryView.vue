<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { message, Modal } from 'ant-design-vue'
import { api, ApiError, type ProbeOrigin, type PublicProbeLibrary, type PublicProbeTarget } from '@/api/client'
import { LbBatchBar, LbEmptyState, LbIcon, LbInfoTip, LbRowCard, LbTimeText } from '@/components/lb'
import { useNarrow } from '@/composables/useNarrow'
import { familyText, ispText, metricLabel, originText } from '@/components/asset/probeView'
import ProbeSourceCards from '@/components/probe/ProbeSourceCards.vue'
import ProbeLimitsCard from '@/components/probe/ProbeLimitsCard.vue'
import ProbeTargetFilterBar from '@/components/probe/ProbeTargetFilterBar.vue'
import PublicTargetFormModal from '@/components/probe/PublicTargetFormModal.vue'
import ProbeScopeModal from '@/components/probe/ProbeScopeModal.vue'
import ProbeImportModal from '@/components/probe/ProbeImportModal.vue'
import SourceImportModal from '@/components/probe/SourceImportModal.vue'
import { useTargetFilter } from '@/components/probe/targetFilter'

/**
 * 公共探测目标库:出站网络质量的目标(每台自建 VPS 在本机测它们)。
 *
 * 目标来自三处:自定义(管理员自己登记或批量导入)、Zstatic 预置与目录、TcpQuality 同步。
 * 登记与启用是两步:进了目标库不等于有哪台机器在测;对节点启用要另外选范围,
 * 第三方来源还要先确认授权。所有第三方目标默认不对任何节点生效。
 */
const data = ref<PublicProbeLibrary | null>(null)
const loading = ref(false)
const loadError = ref('')
const narrow = useNarrow()

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    data.value = await api.publicProbeTargets()
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '读取目标库失败'
  } finally {
    loading.value = false
  }
}
onMounted(load)

const items = computed(() => data.value?.targets ?? [])
const { filter, filtered, options, active, reset } = useTargetFilter(items)
const selected = ref<number[]>([])
watch(filter, () => (selected.value = []), { deep: true })
const selectedTargets = computed(() => items.value.filter((t) => selected.value.includes(t.id)))

const rowSelection = computed(() => ({
  selectedRowKeys: selected.value,
  onChange: (keys: (string | number)[]) => (selected.value = keys.map(Number)),
}))

const missingHelpers = computed(() => {
  const h = data.value?.probe.helpers ?? {}
  return ['amd64', 'arm64'].filter((a) => !h[a])
})

const summary = computed(() => {
  const list = items.value
  return {
    total: list.length,
    active: list.filter((p) => p.node_count > 0).length,
    third: list.filter((p) => p.third_party).length,
    idle: list.filter((p) => !p.node_count && !p.pause_reason).length,
  }
})

// ---------- 单行 ----------

const formOpen = ref(false)
const editing = ref<PublicProbeTarget | null>(null)
function openCreate() {
  editing.value = null
  formOpen.value = true
}
function openEdit(p: PublicProbeTarget) {
  editing.value = p
  formOpen.value = true
}
async function onSaved(p: PublicProbeTarget, created: boolean) {
  formOpen.value = false
  await load()
  if (created && !p.third_party) openScope([items.value.find((x) => x.id === p.id) ?? p])
}

const scopeOpen = ref(false)
const scopeTargets = ref<PublicProbeTarget[]>([])
function openScope(list: PublicProbeTarget[]) {
  scopeTargets.value = list
  scopeOpen.value = true
}
async function onScopeSaved() {
  scopeOpen.value = false
  selected.value = []
  await load()
}

// ---------- 批量 ----------

const batchBusy = ref(false)
async function batch(action: 'pause' | 'resume') {
  batchBusy.value = true
  try {
    const r = await api.publicProbeBatch({ ids: selected.value, action })
    message.success(action === 'pause' ? `已暂停 ${r.affected} 个(哪台都不再测,历史保留)` : `已恢复 ${r.affected} 个`)
    selected.value = []
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '操作失败')
  } finally {
    batchBusy.value = false
  }
}

function batchDelete() {
  const n = selected.value.length
  Modal.confirm({
    title: `删除选中的 ${n} 个目标?`,
    content: '它们在各台机器上的出站探测历史(明细与小时汇总)会一起删除,节点上单独设过的开关也会清掉。只想暂时停下用「暂停」,历史会保留。',
    okText: '删除',
    okType: 'danger',
    cancelText: '取消',
    onOk: () => {
      void (async () => {
        try {
          const r = await api.publicProbeBatch({ ids: selected.value, action: 'delete' })
          message.success(`已删除 ${r.affected} 个目标`)
          selected.value = []
          await load()
        } catch (err) {
          message.error(err instanceof ApiError ? err.message : '删除失败')
        }
      })()
    },
  })
}

const verifying = ref(false)
async function batchVerify() {
  verifying.value = true
  try {
    const r = await api.verifyPublicProbes(selected.value)
    const bad = r.results.filter((x) => x.state === 'FAILED').length
    const warn = r.results.filter((x) => x.state === 'WARN').length
    message.info(`主控解析检查完成:${r.results.length - bad - warn} 个正常,${warn} 个需留意,${bad} 个失败(节点上能否测通请在节点详情里试测)`)
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '验证失败')
  } finally {
    verifying.value = false
  }
}

// ---------- 导入 ----------

const importOpen = ref(false)
const sourceOpen = ref(false)
const sourceKind = ref<ProbeOrigin>('ZSTATIC')
function openSource(kind: ProbeOrigin) {
  sourceKind.value = kind
  sourceOpen.value = true
}
const sourceInfo = computed(() => data.value?.sources.find((s) => s.kind === sourceKind.value) ?? null)

// ---------- 展示 ----------

function scopeText(p: PublicProbeTarget): string {
  if (p.pause_reason) return p.pause_reason
  if (p.apply_all) return p.off_count ? `全部节点,${p.off_count} 台单独停用` : `全部节点(${p.node_count} 台)`
  if (p.node_count) return `${p.node_count} 台节点`
  return '未应用到节点'
}

function placeText(p: PublicProbeTarget): string {
  return [p.region, p.province, p.city, ispText(p.isp)].filter(Boolean).join(' ') || '—'
}

const verifyText: Record<string, string> = { UNVERIFIED: '未验证', OK: '正常', WARN: '需留意', FAILED: '失败' }

const columns = [
  { title: '目标', key: 'name', width: 230 },
  { title: '方式', key: 'method', width: 150 },
  { title: '地区 / 运营商', key: 'place', width: 150 },
  { title: '来源', key: 'source', width: 96 },
  { title: '生效', key: 'scope', width: 170 },
  { title: '间隔', key: 'interval', width: 72 },
  { title: '验证', key: 'verify', width: 80 },
  { title: '', key: 'ops', width: 150, fixed: 'right' as const },
]
</script>

<template>
  <div class="lb-page plv">
    <div class="lb-page__head">
      <div class="lb-page__title-wrap">
        <h1 class="lb-page__title">
          <span>探测目标库</span>
          <LbInfoTip :width="380">
            出站网络质量的目标:每台自建 VPS 在本机测它们(ICMP 丢包率、TCP 连接失败率,或 TCP 握手近似的握手超时无应答率)。
            <b>登记与启用是两步</b>:进了目标库不等于有哪台机器在测;第三方目标默认不对任何节点生效,对节点启用前来源要先确认授权。
            单台节点的开关、间隔覆盖与试测在节点详情的「网络监测」里;批量应用到多台机器也可以在「自建节点」列表里勾选后操作。
          </LbInfoTip>
        </h1>
        <div v-if="data" class="lb-page__summary lb-tabular">
          {{ summary.total }} 个目标:{{ summary.active }} 个有节点在测,{{ summary.idle }} 个未应用到节点;其中第三方 {{ summary.third }} 个。
        </div>
      </div>
      <div class="lb-page__actions">
        <a-button @click="importOpen = true">
          <template #icon><LbIcon name="upload" :size="14" /></template>
          批量导入
        </a-button>
        <a-button @click="openSource('ZSTATIC')">Zstatic 预置</a-button>
        <a-button @click="openSource('TCPQUALITY')">TcpQuality 同步</a-button>
        <a-button type="primary" data-testid="public-target-add" @click="openCreate">
          <template #icon><LbIcon name="plus" :size="14" /></template>
          新增目标
        </a-button>
      </div>
    </div>

    <div v-if="loadError" class="lb-card"><div class="lb-error-strip">{{ loadError }}</div></div>
    <template v-else-if="data">
      <div v-if="!data.probe.enabled" class="lb-notice">
        <span class="lb-notice__icon lb-notice__icon--warn"><LbIcon name="circle-alert" :size="18" /></span>
        <div class="lb-notice__body">
          <div class="lb-notice__title">网络探测已在系统设置里全局关闭</div>
          <div class="lb-notice__text">全局关闭对每一台机器都生效,任何节点都不能自行开启;目标、历史与开关都保留。</div>
        </div>
        <div class="lb-notice__actions"><RouterLink to="/settings#tcpprobe">去设置</RouterLink></div>
      </div>
      <div class="plv__status lb-tabular">
        <span v-if="!data.probe.outbound_available">这个面板没有接入出站探测引擎</span>
        <template v-else>
          <span>
            出站探测
            <template v-if="data.probe.outbound_last_run.last_run_at">
              上一拍 <LbTimeText :value="data.probe.outbound_last_run.last_run_at" /> · 执行 {{ data.probe.outbound_last_run.nodes }} 台
              <template v-if="data.probe.outbound_last_run.busy">· 正忙让路 {{ data.probe.outbound_last_run.busy }} 台</template>
              <template v-if="data.probe.outbound_last_run.ssh_errors">· 采集失败 {{ data.probe.outbound_last_run.ssh_errors }}</template>
              <template v-if="data.probe.outbound_last_run.tool_errors">· 工具不可用 {{ data.probe.outbound_last_run.tool_errors }}</template>
            </template>
            <template v-else>还没有跑过</template>
          </span>
          <span v-if="missingHelpers.length" class="plv__warn">
            <LbIcon name="circle-alert" :size="13" />主控缺少 {{ missingHelpers.join('、') }} 的辅助程序(执行 scripts/build-probe.sh)
          </span>
          <span v-else>辅助程序:amd64 · arm64 已就绪</span>
        </template>
      </div>

      <ProbeSourceCards :sources="data.sources" @changed="load" @import="openSource" />
      <ProbeLimitsCard
        :limits="data.limits.limits"
        :ranges="data.limits.ranges"
        :last-run="data.probe.outbound_available ? data.probe.outbound_last_run : null"
        @saved="load"
      />

      <div class="lb-card lb-card--flush">
        <ProbeTargetFilterBar
          :filter="filter"
          :options="options"
          :shown="filtered.length"
          :total="items.length"
          :active="active"
          @reset="reset"
        />
        <LbBatchBar :selected-count="selected.length" :filtered-total="filtered.length" :total="items.length" unit="个" @clear="selected = []">
          <a-button size="small" type="primary" @click="openScope(selectedTargets)">应用到节点…</a-button>
          <a-button size="small" :loading="batchBusy" @click="batch('resume')">恢复</a-button>
          <a-button size="small" :loading="batchBusy" @click="batch('pause')">暂停</a-button>
          <a-button size="small" :loading="verifying" @click="batchVerify">验证解析</a-button>
          <a-button size="small" danger @click="batchDelete">删除</a-button>
        </LbBatchBar>

        <LbEmptyState
          v-if="!items.length"
          variant="empty"
          title="目标库还是空的"
          description="新增一个自己的目标,或者从 Zstatic 预置、TcpQuality 同步里挑几个。导入只是登记,不会让任何节点开始探测;升级也不会替任何机器自动开启。"
        />
        <LbEmptyState v-else-if="!filtered.length" variant="filtered" title="没有符合筛选的目标" @clear="reset" />
        <template v-else-if="narrow">
          <div class="plv__cards">
            <LbRowCard v-for="p in filtered" :key="p.id">
              <template #head>
                <a-checkbox
                  :checked="selected.includes(p.id)"
                  @update:checked="(v: boolean) => (selected = v ? [...selected, p.id] : selected.filter((x) => x !== p.id))"
                />
                <span class="plv__name lb-ellipsis">{{ p.name }}</span>
              </template>
              <div class="plv__card-body">
                <span class="lb-mono plv__host">{{ p.host }}<template v-if="p.protocol === 'TCP'">:{{ p.port }}</template></span>
                <span>{{ metricLabel(p.metric) }} · {{ familyText(p.family) }} · {{ originText(p.source) }}</span>
                <span>{{ placeText(p) }}</span>
                <span :class="{ plv__warn: !!p.pause_reason }">{{ scopeText(p) }}</span>
              </div>
              <template #foot>
                <a-button size="small" @click="openScope([p])">启用节点</a-button>
                <a-button size="small" @click="openEdit(p)">编辑</a-button>
              </template>
            </LbRowCard>
          </div>
        </template>
        <a-table
          v-else
          :columns="columns"
          :data-source="filtered"
          :row-key="(p: PublicProbeTarget) => p.id"
          :row-selection="rowSelection"
          :pagination="filtered.length > 50 ? { pageSize: 50, showSizeChanger: false } : false"
          size="small"
          :scroll="{ x: 1100 }"
          data-testid="probe-library-table"
        >
          <template #bodyCell="{ column, record }">
            <template v-if="column.key === 'name'">
              <div class="plv__cell">
                <span class="plv__name lb-ellipsis" :title="record.name">
                  {{ record.name }}
                  <span v-if="record.role === 'BACKUP'" class="lb-chip">备用</span>
                  <span v-if="record.upstream_missing_since" class="lb-chip lb-chip--warn" title="上游列表里已经不见了(只标记,不删除、不停测)">上游已消失</span>
                </span>
                <span class="lb-mono plv__host">{{ record.host }}<template v-if="record.protocol === 'TCP'">:{{ record.port }}</template></span>
              </div>
            </template>
            <template v-else-if="column.key === 'method'">
              <div class="plv__cell">
                <span>{{ metricLabel(record.metric) }}</span>
                <span class="plv__muted">
                  {{ familyText(record.family) }}<template v-if="record.metric === 'LOSS'"> · 每轮 {{ record.count }} 个包</template>
                  <template v-else-if="record.metric === 'SYN'"> · 每轮 {{ record.count }} 次握手</template>
                </span>
              </div>
            </template>
            <template v-else-if="column.key === 'place'">{{ placeText(record) }}</template>
            <template v-else-if="column.key === 'source'">
              <span :class="{ plv__muted: record.source === 'MANUAL' }">{{ originText(record.source) }}</span>
            </template>
            <template v-else-if="column.key === 'scope'">
              <span :class="{ plv__warn: !!record.pause_reason, plv__muted: !record.node_count && !record.pause_reason }">
                {{ scopeText(record) }}
              </span>
            </template>
            <template v-else-if="column.key === 'interval'">
              <span class="lb-tabular" :title="record.effective_interval_sec !== record.interval_sec ? `设置 ${record.interval_sec} 秒,按来源下限 ${record.effective_interval_sec} 秒` : ''">
                {{ record.effective_interval_sec }} 秒
              </span>
            </template>
            <template v-else-if="column.key === 'verify'">
              <span
                :class="{ plv__ok: record.verify_state === 'OK', plv__warn: record.verify_state === 'WARN', plv__bad: record.verify_state === 'FAILED', plv__muted: record.verify_state === 'UNVERIFIED' }"
                :title="record.verify_detail ? `${record.verify_detail}(${record.verified_at})` : '还没有验证过'"
              >
                {{ verifyText[record.verify_state] }}
              </span>
            </template>
            <template v-else-if="column.key === 'ops'">
              <div class="plv__ops">
                <button type="button" class="lb-linkbtn" @click="openScope([record])">启用节点</button>
                <button type="button" class="lb-linkbtn" @click="openEdit(record)">编辑</button>
              </div>
            </template>
          </template>
        </a-table>
      </div>
    </template>
    <div v-else class="lb-card"><a-skeleton active :paragraph="{ rows: 6 }" /></div>

    <PublicTargetFormModal
      :open="formOpen"
      :target="editing"
      :provinces="data?.provinces ?? []"
      :sources="data?.sources ?? []"
      :next-sort="items.length"
      @close="formOpen = false"
      @saved="onSaved"
    />
    <ProbeScopeModal :open="scopeOpen" :targets="scopeTargets" @close="scopeOpen = false" @saved="onScopeSaved" />
    <ProbeImportModal :open="importOpen" @close="importOpen = false" @imported="() => { importOpen = false; void load() }" />
    <SourceImportModal
      :open="sourceOpen"
      :source="sourceKind"
      :info="sourceInfo"
      :provinces="data?.provinces ?? []"
      @close="sourceOpen = false"
      @imported="() => { sourceOpen = false; void load() }"
    />
  </div>
</template>

<style scoped>
.plv {
  display: flex;
  flex-direction: column;
  gap: var(--sp-4);
}
.plv__status {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-1) var(--sp-3);
  font-size: var(--fs-xs);
  color: var(--text3);
}
.plv__cell {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.plv__name {
  font-weight: 600;
  color: var(--text);
}
.plv__host {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.plv__muted {
  color: var(--text3);
  font-size: var(--fs-xs);
}
.plv__warn {
  color: var(--warn);
}
.plv__bad {
  color: var(--bad);
}
.plv__ok {
  color: var(--ok);
}
.plv__ops {
  display: flex;
  gap: var(--sp-3);
}
.plv__cards {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-3);
}
.plv__card-body {
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: var(--fs-xs);
  color: var(--text2);
}
</style>
