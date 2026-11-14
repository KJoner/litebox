<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import {
  api,
  ApiError,
  type NextHopPreview,
  type TargetCandidate,
  type TargetCandidates,
  type TopoEndpoint,
} from '@/api/client'
import { regionFlag } from '@/components/asset/assetModel'
import {
  endpointText,
  entryTitle,
  familyText,
  kindLabel,
  PENDING_TEXT,
  protocolLabel,
} from './topoText'

/**
 * 统一的「选择目标入口 → 选择连接地址 → 查看完整路径与影响」(v23.3)。
 *
 * 单跳出口、Mieru 出口、转发规则的「设置转发目标」、快速组链与画布菜单都用这一个组件:
 * 候选与禁用原因来自后端(与保存时的校验是同一个函数),这里不另写一套判断 ——
 * 两处各写一份的话,迟早出现「看着能选、保存时被拒」或者反过来。
 *
 * 列表按机器分组、每个入口一行,写出种类、协议、启停与部署状态、地址条数;不能只列机器名让人猜是哪个入口。
 * 选中入口之后列出它全部可连接的地址:多条时必须明确选一条,只有一条时预选,但确认区照样写出实际目标。
 */
export interface NextHopValue {
  kind: 'INBOUND' | 'EXTERNAL' | 'RELAY' | 'ADDRESS'
  id: number
  endpoint_id: number
  host?: string
  port?: number
}

const props = withDefaults(
  defineProps<{
    /** 已有的来源入口(SINGBOX:12 / MIERU:3 / NGINX:5 / REALM:6) */
    from?: string
    /** 还没建出来的来源:种类与将要所在的机器 */
    newKind?: 'SINGBOX' | 'NGINX' | 'REALM'
    newNode?: number
    modelValue: NextHopValue | null
    /** 转发规则可以指向手填的地址(不进订阅、不拨测) */
    allowAddress?: boolean
  }>(),
  { from: '', newKind: undefined, newNode: 0, allowAddress: false },
)

const emit = defineEmits<{
  'update:modelValue': [NextHopValue | null]
  preview: [NextHopPreview | null]
}>()

const data = ref<TargetCandidates | null>(null)
const loading = ref(false)
const loadError = ref('')
const query = ref('')
const selectedKey = ref('')
const endpointID = ref(0)
const manualHost = ref('')
const manualPort = ref<number | null>(null)
const preview = ref<NextHopPreview | null>(null)
const previewing = ref(false)
const previewError = ref('')

const MANUAL = 'ADDRESS:manual'

function hopKind(c: Pick<TargetCandidate, 'kind'>): NextHopValue['kind'] {
  if (c.kind === 'SINGBOX') return 'INBOUND'
  if (c.kind === 'EXTERNAL') return 'EXTERNAL'
  return 'RELAY'
}

function matches(c: TargetCandidate, v: NextHopValue | null): boolean {
  if (!v || v.kind === 'ADDRESS') return false
  return hopKind(c) === v.kind && c.id === v.id
}

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    data.value = props.from
      ? await api.topologyTargets(props.from)
      : await api.topologyTargetsNew(props.newKind ?? 'SINGBOX', props.newNode)
    const v = props.modelValue
    if (v?.kind === 'ADDRESS') {
      selectedKey.value = MANUAL
      manualHost.value = v.host ?? ''
      manualPort.value = v.port ?? null
    } else {
      const hit = allCandidates.value.find((c) => matches(c, v))
      selectedKey.value = hit?.key ?? ''
      endpointID.value = v?.endpoint_id ?? 0
      if (hit && !endpointID.value) preselectEndpoint(hit)
    }
  } catch (e) {
    loadError.value = e instanceof ApiError ? e.message : '读取候选失败'
    data.value = null
  } finally {
    loading.value = false
  }
}

watch(() => [props.from, props.newKind, props.newNode], () => void load(), { immediate: true })

const allCandidates = computed<TargetCandidate[]>(() =>
  (data.value?.groups ?? []).flatMap((g) => g.candidates),
)

/** 搜索入口名、机器名、地址、端口、协议与引擎。 */
const groups = computed(() => {
  const q = query.value.trim().toLowerCase()
  const hit = (c: TargetCandidate) => {
    if (!q) return true
    const hay = [
      c.name,
      c.node_name ?? '',
      c.source ?? '',
      kindLabel(c.kind),
      protocolLabel(c.protocol),
      ...c.endpoints.map((e) => endpointText(e)),
    ]
      .join(' ')
      .toLowerCase()
    return hay.includes(q)
  }
  return (data.value?.groups ?? [])
    .map((g) => ({ ...g, candidates: g.candidates.filter(hit) }))
    .filter((g) => g.candidates.length > 0)
})

const selected = computed(() => allCandidates.value.find((c) => c.key === selectedKey.value) ?? null)

/** 外部代理只有一个地址,不用选;入站与转发规则列出全部条目。 */
const endpointChoices = computed<TopoEndpoint[]>(() => {
  const c = selected.value
  if (!c || c.kind === 'EXTERNAL') return []
  return c.endpoints
})

const fromLegacy = computed(() => {
  const n = data.value?.from.next
  return n?.addr_mode === 'LEGACY' ? n.dial ?? '' : ''
})

function preselectEndpoint(c: TargetCandidate) {
  // 只有一条(或只有默认地址)时预选;多条时必须由人选,不按列表顺序替他定。
  const real = c.endpoints.filter((e) => !e.default)
  endpointID.value = real.length === 1 ? real[0].id : 0
}

function choose(c: TargetCandidate) {
  if (c.disabled) return
  selectedKey.value = c.key
  preselectEndpoint(c)
  emitValue()
}

function chooseManual() {
  selectedKey.value = MANUAL
  emitValue()
}

const value = computed<NextHopValue | null>(() => {
  if (selectedKey.value === MANUAL) {
    if (!manualHost.value.trim() || !manualPort.value) return null
    return { kind: 'ADDRESS', id: 0, endpoint_id: 0, host: manualHost.value.trim(), port: manualPort.value }
  }
  const c = selected.value
  if (!c) return null
  const needsEndpoint = endpointChoices.value.filter((e) => !e.default).length > 1
  if (needsEndpoint && !endpointID.value) return null
  return { kind: hopKind(c), id: c.id, endpoint_id: endpointID.value }
})

/** 选择是否完整:多条地址时没选连哪一条就不完整。 */
const needEndpointPick = computed(
  () => !!selected.value && endpointChoices.value.filter((e) => !e.default).length > 1 && !endpointID.value,
)

function emitValue() {
  emit('update:modelValue', value.value)
}

watch([endpointID, manualHost, manualPort], emitValue)

let previewSeq = 0
watch(
  value,
  async (v) => {
    preview.value = null
    previewError.value = ''
    emit('preview', null)
    if (!v || !props.from) return
    const seq = ++previewSeq
    previewing.value = true
    try {
      const pv = await api.topologyPreview({
        from: props.from,
        target_kind: v.kind,
        target_id: v.id,
        target_endpoint_id: v.endpoint_id,
        target_host: v.host,
        target_port: v.port ?? 0,
      })
      if (seq !== previewSeq) return
      preview.value = pv
      emit('preview', pv)
    } catch (e) {
      if (seq !== previewSeq) return
      previewError.value = e instanceof ApiError ? e.message : '预览失败'
    } finally {
      if (seq === previewSeq) previewing.value = false
    }
  },
  { deep: true },
)

function statusBits(c: TargetCandidate): string[] {
  const out: string[] = []
  if (!c.enabled) out.push('已停用')
  if (!c.deployed && c.kind !== 'EXTERNAL') out.push('未部署')
  if (c.pending) out.push(PENDING_TEXT[c.pending] ?? '待下发')
  return out
}

function endpointCount(c: TargetCandidate): string {
  const real = c.endpoints.filter((e) => !e.default).length
  return real ? `${real} 条地址` : '默认地址'
}

defineExpose({ reload: load, revision: computed(() => preview.value?.revision ?? data.value?.revision ?? '') })
</script>

<template>
  <div class="nhp">
    <a-input
      v-model:value="query"
      size="small"
      allow-clear
      placeholder="搜索入口名、机器、地址、端口、协议或引擎"
      class="nhp__search"
      aria-label="搜索候选入口"
    />
    <div v-if="fromLegacy" class="nhp__legacy" role="note">
      现在连的是升级前留下的兼容地址 <span class="lb-mono">{{ fromLegacy }}</span>,
      目标入口上没有唯一对应的地址条目 —— 请在下面明确选一条。
    </div>
    <div v-if="loading" class="nhp__hint">正在读取候选…</div>
    <div v-else-if="loadError" class="nhp__bad">{{ loadError }}</div>
    <div v-else class="nhp__list" role="listbox" aria-label="下一跳候选">
      <div v-for="g in groups" :key="g.host_id" class="nhp__group">
        <div class="nhp__ghead">
          <span v-if="regionFlag(g.region)" class="lb-flag" aria-hidden="true">{{ regionFlag(g.region) }}</span>
          <span>{{ g.host_id ? g.host_name : '外部代理' }}</span>
        </div>
        <button
          v-for="c in g.candidates"
          :key="c.key"
          type="button"
          role="option"
          class="nhp__row"
          :class="{ 'is-on': c.key === selectedKey, 'is-off': c.disabled }"
          :aria-selected="c.key === selectedKey"
          :aria-disabled="c.disabled"
          :data-testid="`cand-${c.key}`"
          @click="choose(c)"
        >
          <span class="nhp__name">{{ c.name }}</span>
          <span class="nhp__meta">
            <span class="nhp__chip">{{ kindLabel(c.kind) }}</span>
            <span v-if="c.protocol" class="nhp__chip">{{ protocolLabel(c.protocol) }}</span>
            <span class="nhp__dim">{{ endpointCount(c) }}</span>
            <span v-for="s in statusBits(c)" :key="s" class="nhp__warn">{{ s }}</span>
            <span v-if="c.current" class="nhp__cur">当前</span>
          </span>
          <span v-if="c.disabled && c.reason" class="nhp__reason">{{ c.reason }}</span>
        </button>
      </div>
      <button
        v-if="allowAddress"
        type="button"
        role="option"
        class="nhp__row"
        :class="{ 'is-on': selectedKey === MANUAL }"
        :aria-selected="selectedKey === MANUAL"
        data-testid="cand-manual"
        @click="chooseManual"
      >
        <span class="nhp__name">指定地址(手填 IP 或域名 + 端口)</span>
        <span class="nhp__reason">纯端口转发:不进订阅、不拨测,面板不知道它背后是什么</span>
      </button>
      <div v-if="!groups.length && !allowAddress" class="nhp__hint">没有匹配的入口</div>
    </div>

    <div v-if="selectedKey === MANUAL" class="nhp__manual">
      <a-input v-model:value="manualHost" size="small" placeholder="例如 203.0.113.9 或 landing.example.com" />
      <a-input-number v-model:value="manualPort" size="small" :min="1" :max="65535" placeholder="端口" />
    </div>

    <div v-if="endpointChoices.length" class="nhp__eps">
      <div class="nhp__label">
        连接「{{ selected ? entryTitle(selected) : '' }}」的哪一条地址
        <span v-if="needEndpointPick" class="nhp__warn">这个入口有多条地址,请选一条</span>
      </div>
      <a-radio-group v-model:value="endpointID" class="nhp__epgroup">
        <a-radio
          v-for="ep in endpointChoices"
          :key="ep.id"
          :value="ep.id"
          class="nhp__ep"
          :data-testid="`ep-${ep.id}`"
        >
          <span class="lb-mono">{{ endpointText(ep) }}</span>
          <span class="nhp__dim">{{ familyText(ep.family) }}</span>
          <span v-if="ep.label" class="nhp__dim">{{ ep.label }}</span>
        </a-radio>
      </a-radio-group>
    </div>

    <div v-if="previewing" class="nhp__hint">正在预览完整路径…</div>
    <div v-else-if="previewError" class="nhp__bad">{{ previewError }}</div>
    <div v-else-if="preview" class="nhp__pv" data-testid="nexthop-preview">
      <div class="nhp__pvrow"><span class="nhp__k">实际连接</span><span class="lb-mono">{{ preview.dial || '—' }}</span></div>
      <div class="nhp__pvrow"><span class="nhp__k">在这里认证</span><span>{{ preview.auth }}</span></div>
      <div v-if="preview.transparent.length" class="nhp__pvrow">
        <span class="nhp__k">经过的透明转发</span><span>{{ preview.transparent.join(' → ') }}</span>
      </div>
      <div v-if="preview.path" class="nhp__pvrow">
        <span class="nhp__k">完整路径</span>
        <span>
          <template v-if="preview.path.hops.length">
            {{ preview.path.hops.map((h) => (h.node_name ? h.node_name + ' / ' : '') + h.name).join(' → ') }};
          </template>
          {{ preview.path.egress }}
        </span>
      </div>
      <div v-if="preview.deploys.length" class="nhp__pvrow">
        <span class="nhp__k">保存后要下发</span>
        <ol class="nhp__ol">
          <li v-for="(d, i) in preview.deploys" :key="i">{{ d.node_name }} 的 {{ d.component }}:{{ d.why }}</li>
        </ol>
      </div>
      <div v-if="preview.affected.length" class="nhp__pvrow">
        <span class="nhp__k">一起受影响的上游</span>
        <span>{{ preview.affected.map((r) => r.node_name + ' / ' + r.name).join(';') }}</span>
      </div>
      <p v-for="w in preview.warnings" :key="w" class="nhp__warnline">{{ w }}</p>
      <p v-for="p in preview.problems" :key="p" class="nhp__bad">{{ p }}</p>
      <p v-for="n in preview.notes" :key="n" class="nhp__hint">{{ n }}</p>
    </div>
  </div>
</template>

<style scoped>
.nhp {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.nhp__list {
  max-height: 280px;
  overflow: auto;
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
  padding: var(--sp-1);
  background: var(--surface2);
}
.nhp__group + .nhp__group {
  margin-top: var(--sp-2);
}
.nhp__ghead {
  display: flex;
  gap: var(--sp-1);
  align-items: center;
  padding: var(--sp-1) var(--sp-2);
  font-size: var(--fs-xs);
  color: var(--text3);
}
.nhp__row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-1) var(--sp-2);
  width: 100%;
  text-align: left;
  padding: var(--sp-2);
  border: 1px solid transparent;
  border-radius: var(--r-sm);
  background: transparent;
  color: var(--text);
  font: inherit;
  cursor: pointer;
  transition: background var(--dur-fast) var(--ease), border-color var(--dur-fast) var(--ease);
}
.nhp__row:hover {
  background: var(--fill);
}
.nhp__row:focus-visible {
  outline: none;
  box-shadow: var(--focus-ring);
}
.nhp__row.is-on {
  border-color: var(--brand-border);
  background: var(--brand-bg);
}
.nhp__row.is-off {
  cursor: not-allowed;
  opacity: 0.62;
}
.nhp__name {
  font-size: var(--fs-sm);
  font-weight: 500;
}
.nhp__meta {
  display: inline-flex;
  flex-wrap: wrap;
  gap: var(--sp-1);
  align-items: center;
}
.nhp__chip {
  padding: 0 6px;
  border-radius: var(--r-xs);
  background: var(--fill);
  font-size: var(--fs-2xs);
  color: var(--text2);
}
.nhp__dim {
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.nhp__warn {
  font-size: var(--fs-2xs);
  color: var(--warn);
}
.nhp__cur {
  font-size: var(--fs-2xs);
  color: var(--brand);
}
.nhp__reason {
  flex-basis: 100%;
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.nhp__eps,
.nhp__pv {
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
  padding: var(--sp-2) var(--sp-3);
}
.nhp__label {
  font-size: var(--fs-xs);
  color: var(--text2);
  margin-bottom: var(--sp-1);
  display: flex;
  gap: var(--sp-2);
}
.nhp__epgroup {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.nhp__ep :deep(span) {
  margin-right: var(--sp-2);
}
.nhp__manual {
  display: flex;
  gap: var(--sp-2);
}
.nhp__pvrow {
  display: flex;
  gap: var(--sp-2);
  font-size: var(--fs-xs);
  line-height: 1.6;
}
.nhp__k {
  flex: 0 0 96px;
  color: var(--text3);
}
.nhp__ol {
  margin: 0;
  padding-left: 18px;
}
.nhp__warnline {
  margin: var(--sp-1) 0 0;
  font-size: var(--fs-xs);
  color: var(--warn);
}
.nhp__bad {
  margin: var(--sp-1) 0 0;
  font-size: var(--fs-xs);
  color: var(--bad);
}
.nhp__hint {
  margin: 0;
  font-size: var(--fs-xs);
  color: var(--text3);
}
.nhp__legacy {
  padding: var(--sp-2) var(--sp-3);
  border-radius: var(--r-group);
  background: var(--warn-bg);
  color: var(--warn);
  font-size: var(--fs-xs);
}
</style>
