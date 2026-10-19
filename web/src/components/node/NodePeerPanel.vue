<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { Modal, message } from 'ant-design-vue'
import { api, ApiError, type PeerSourceOption, type ProbeCandidate, type ProbeSummary } from '@/api/client'
import { LbIcon, LbInfoTip, LbStatusTag, type LbStatusMeta } from '@/components/lb'
import ProbeDots from '@/components/asset/ProbeDots.vue'
import { familyText, probeMuted, probeValue, rateName, rateOf, rateText, rateTitle } from '@/components/asset/probeView'

/**
 * 节点详情「网络监测」里的自建节点拨测(PEER,V22.6):在别的自建 VPS 上测这台机器的订阅入口。
 *
 * 每条序列 = 一台来源机器 × 一种测法(Ping 只测地址,TCPing 测地址与端口),与主控、Boce 的序列各自独立。
 * 执行在来源机器上(经已有的 SSH 连接临时启动辅助程序,与它自己的出站探测并进同一次执行),
 * 间隔跟随入口检测的全局间隔。来源机器连不上记「采集失败」,不进这台机器的任何比例,也不告警。
 * 目标只能从这台机器的订阅入口里选(不含管理通道),不收自由输入。
 */
const props = defineProps<{
  nodeId: number
  series: ProbeSummary[]
  /** 这台机器的入口候选(来自 GET /api/nodes/{id}/probe,只用对外服务那几个)。 */
  candidates: ProbeCandidate[]
  sources: PeerSourceOption[]
  selected: number | null
  keyword: string
  intervalSec: number
  timeoutMs: number
  stateMeta: (t: ProbeSummary) => LbStatusMeta
}>()
const emit = defineEmits<{ (e: 'select', id: number): void; (e: 'changed'): void }>()

const service = computed(() => props.candidates.filter((c) => c.kind === 'SERVICE'))
const candKey = (c: ProbeCandidate) => `${c.ref}|${c.family}`

const form = reactive({
  target: undefined as string | undefined,
  sources: [] as number[],
  ping: true,
  tcping: true,
  periodic: true,
})
const adding = ref(false)
const formOpen = ref(false)

async function create() {
  const c = service.value.find((x) => candKey(x) === form.target)
  if (!c) return
  const methods: ('ICMP' | 'TCP')[] = []
  if (form.ping) methods.push('ICMP')
  if (form.tcping) methods.push('TCP')
  adding.value = true
  try {
    const res = await api.createProbePeers(props.nodeId, {
      ref: c.ref,
      family: c.family,
      source_node_ids: form.sources,
      methods,
      enabled: form.periodic,
    })
    const dup = res.duplicates ? `,另有 ${res.duplicates} 条已经存在` : ''
    message.success(`已新增 ${res.created.length} 条自建节点拨测${dup}${form.periodic ? ',下一轮开始测' : '(只手动检测)'}`)
    form.sources = []
    formOpen.value = false
    if (res.created[0]) emit('select', res.created[0].id)
    emit('changed')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '新增失败')
  } finally {
    adding.value = false
  }
}
const canCreate = computed(() => !!form.target && form.sources.length > 0 && (form.ping || form.tcping))

const shown = computed(() => {
  const k = props.keyword.trim().toLowerCase()
  return props.series.filter(
    (t) => !k || [t.label, t.resolved_host, t.peer_source?.name, t.peer_source?.method].join(' ').toLowerCase().includes(k),
  )
})

// ---------- 立即检测 / 暂停 / 删除 ----------

const running = ref<Set<number>>(new Set())
async function runNow(ids: number[]) {
  if (!ids.length) return
  for (const id of ids) running.value.add(id)
  try {
    const res = await api.runProbePeers(props.nodeId, ids)
    const busy = res.results.filter((r) => !r.recorded).length
    const done = res.results.length - busy
    if (busy) message.warning(`${done} 条已检测并记入历史;${busy} 条的来源机器正忙(部署 / 同步 / 巡检),这一次没有测,稍后再试`)
    else message.success(`已检测 ${done} 条,结果记入历史`)
    emit('changed')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '检测失败')
  } finally {
    for (const id of ids) running.value.delete(id)
  }
}

async function toggle(t: ProbeSummary) {
  try {
    await api.updateProbeTarget(t.id, { enabled: !t.enabled })
    message.success(t.enabled ? '已暂停周期检测(历史保留,仍可手动检测)' : '已恢复周期检测')
    emit('changed')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '操作失败')
  }
}

function remove(t: ProbeSummary) {
  Modal.confirm({
    title: `删除自建节点拨测「${t.peer_source?.name ?? ''} · ${t.peer_source?.method ?? ''}」?`,
    content: '这条拨测的历史(明细与小时汇总)会一起删除。只想暂时停下用「暂停」,历史会保留。',
    okText: '删除',
    okType: 'danger',
    cancelText: '取消',
    onOk: () => {
      void (async () => {
        try {
          await api.deleteProbeTarget(t.id)
          message.success('已删除')
          emit('changed')
        } catch (err) {
          message.error(err instanceof ApiError ? err.message : '删除失败')
        }
      })()
    },
  })
}

function where(t: ProbeSummary): string {
  return t.protocol === 'ICMP' ? t.resolved_host : `${t.resolved_host}:${t.resolved_port}`
}
function sourceLabel(o: PeerSourceOption): string {
  return o.display_name && o.display_name !== o.name ? `${o.display_name}(${o.name})` : o.name
}
const runnable = computed(() => props.series.filter((t) => t.state !== 'INVALID').map((t) => t.id))
</script>

<template>
  <div class="np" data-testid="probe-peer">
    <div class="np__head">
      <h3 class="np__title">
        自建节点拨测
        <LbInfoTip
          :width="380"
          text="在别的自建 VPS 上测这台机器的订阅入口:Ping 只测地址(ICMP 丢包率),TCPing 测地址与端口(TCP 连接失败率)。面板经已有的 SSH 连接在来源机器上临时启动辅助程序,与它自己的出站探测并进同一次执行;来源机器正忙时那一轮让路。来源机器连不上记「采集失败」、辅助程序不可用记「工具不可用」,都不计入这台机器的任何比例,也不告警。各来源的序列与主控、Boce 分开统计,不合并。"
        />
      </h3>
      <span class="np__muted">间隔跟随入口检测 · 每 {{ intervalSec }} 秒 · 超时 {{ timeoutMs }} ms</span>
      <span class="np__spacer" />
      <a-button size="small" :disabled="!runnable.length" data-testid="peer-run-all" @click="runNow(runnable)">
        <template #icon><LbIcon name="play" :size="14" /></template>
        全部立即检测
      </a-button>
      <a-button size="small" data-testid="peer-add-open" @click="formOpen = !formOpen">
        <template #icon><LbIcon name="plus" :size="14" /></template>
        新增拨测
      </a-button>
    </div>

    <div v-if="formOpen" class="np__form" data-testid="peer-form">
      <label class="np__field">
        <span>被测入口</span>
        <a-select
          v-model:value="form.target"
          placeholder="选择这台机器的订阅入口"
          :disabled="!service.length"
          aria-label="被测入口"
          show-search
          option-filter-prop="label"
          data-testid="peer-target"
        >
          <a-select-option
            v-for="c in service"
            :key="candKey(c)"
            :value="candKey(c)"
            :label="`${c.label} ${familyText(c.family)} ${c.host}:${c.port}`"
          >
            {{ c.label }} · {{ familyText(c.family) }} · {{ c.host }}:{{ c.port }}
          </a-select-option>
        </a-select>
      </label>
      <label class="np__field">
        <span>拨测来源(可多选)</span>
        <a-select
          v-model:value="form.sources"
          mode="multiple"
          placeholder="选择别的自建节点"
          :disabled="!sources.length"
          aria-label="拨测来源"
          option-filter-prop="label"
          data-testid="peer-sources"
        >
          <a-select-option v-for="o in sources" :key="o.node_id" :value="o.node_id" :label="sourceLabel(o)">
            {{ sourceLabel(o) }}<span v-if="o.note" class="np__muted">({{ o.note }})</span>
          </a-select-option>
        </a-select>
      </label>
      <div class="np__field">
        <span>测法</span>
        <div class="np__checks">
          <a-checkbox v-model:checked="form.ping" data-testid="peer-ping">Ping(只测地址)</a-checkbox>
          <a-checkbox v-model:checked="form.tcping" data-testid="peer-tcping">TCPing(地址与端口)</a-checkbox>
        </div>
      </div>
      <label class="np__field np__field--inline">
        <a-switch v-model:checked="form.periodic" size="small" />
        <span>周期检测</span>
        <span class="np__muted">关掉 = 只建、不按间隔测,用「立即检测」手动测</span>
      </label>
      <div v-if="!sources.length" class="np__note">
        <LbIcon name="info" :size="13" />面板里只有这一台机器:至少要有另一台自建节点才能当拨测来源。
      </div>
      <div class="np__form-ops">
        <a-button size="small" @click="formOpen = false">取消</a-button>
        <a-button type="primary" size="small" :loading="adding" :disabled="!canCreate" data-testid="peer-create" @click="create">
          添加(每个来源 × 测法一条)
        </a-button>
      </div>
    </div>

    <div v-if="!series.length" class="np__muted np__empty">
      还没有自建节点拨测。点「新增拨测」选别的自建机器来测这台的入口;升级不会替任何机器自动建拨测。
    </div>
    <ul v-else class="np__list">
      <li
        v-for="t in shown"
        :key="t.id"
        class="np__row"
        :class="{ 'np__row--on': t.id === selected }"
        :data-testid="`peer-row-${t.id}`"
      >
        <button type="button" class="np__sel" :aria-pressed="t.id === selected" @click="emit('select', t.id)">
          <span class="np__label lb-ellipsis">{{ t.peer_source?.name ?? `#${t.peer_node_id}` }}</span>
          <span class="lb-chip lb-chip--brand">{{ t.peer_source?.method ?? (t.protocol === 'ICMP' ? 'Ping' : 'TCPing') }}</span>
          <span class="lb-chip">{{ familyText(t.family) }}</span>
          <span class="np__muted lb-ellipsis">→ {{ t.label }}</span>
          <span class="np__addr lb-mono">{{ where(t) }}</span>
        </button>
        <div class="np__state">
          <LbStatusTag :meta="stateMeta(t)" />
          <span class="np__rtt lb-tabular" :title="t.state_text">{{ probeValue(t) }}</span>
          <span class="np__muted lb-tabular" :title="rateTitle(t.h1, t.metric, '近 1 小时')">
            1h {{ rateName(t.metric) }} {{ rateText(rateOf(t.h1, t.metric)) }} · 24h {{ rateText(rateOf(t.h24, t.metric)) }}
          </span>
          <span v-if="t.state !== 'OK'" class="np__muted">{{ t.state_text }}</span>
        </div>
        <ProbeDots :dots="t.dots" :seconds="t.dot_seconds" :metric="t.metric" :muted="probeMuted(t.state)" />
        <div class="np__ops">
          <button
            type="button"
            class="lb-linkbtn"
            :disabled="running.has(t.id) || t.state === 'INVALID'"
            :data-testid="`peer-run-${t.id}`"
            @click="runNow([t.id])"
          >{{ running.has(t.id) ? '检测中…' : '立即检测' }}</button>
          <button type="button" class="lb-linkbtn" @click="toggle(t)">{{ t.enabled ? '暂停周期检测' : '恢复周期检测' }}</button>
          <button type="button" class="lb-linkbtn np__danger" @click="remove(t)">删除</button>
        </div>
      </li>
    </ul>
  </div>
</template>

<style scoped>
.np {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.np__head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
}
.np__title {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  margin: 0;
  font-size: var(--fs-md);
  font-weight: 600;
  color: var(--text);
}
.np__spacer {
  flex: 1;
}
.np__form {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
  gap: var(--sp-2) var(--sp-3);
  padding: var(--sp-3);
  border-radius: var(--r-sm);
  background: var(--surface2);
}
.np__field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
  font-size: var(--fs-xs);
  color: var(--text2);
}
.np__field--inline {
  flex-direction: row;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
}
.np__checks {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-3);
}
.np__form-ops {
  display: flex;
  justify-content: flex-end;
  gap: var(--sp-2);
  grid-column: 1 / -1;
}
.np__note {
  display: flex;
  align-items: flex-start;
  gap: 6px;
  grid-column: 1 / -1;
  font-size: var(--fs-xs);
  color: var(--warn);
}
.np__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.np__empty {
  padding: var(--sp-2) 0;
}
.np__list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.np__row {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: var(--sp-3);
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
}
.np__row--on {
  border-color: var(--brand-border);
  background: var(--brand-bg);
}
.np__sel {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
  padding: 0;
  border: none;
  background: transparent;
  text-align: left;
  cursor: pointer;
  color: inherit;
  font: inherit;
  min-width: 0;
}
.np__label {
  font-weight: 600;
  color: var(--text);
  max-width: 100%;
}
.np__addr {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.np__state {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
}
.np__rtt {
  font-weight: 600;
  font-size: var(--fs-sm);
}
.np__ops {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-3);
  font-size: var(--fs-sm);
}
.np__danger {
  color: var(--bad);
}
</style>
