<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type ExternalProxy, type LinearChainPlan, type Node } from '@/api/client'
import { lbDangerConfirm } from '@/components/lb'

/**
 * 线性多跳编辑(复核批次三,CHAIN03)。
 *
 * 一张卡片一跳,方向从上到下:用户连第一张卡片的入口,每一跳的出口是下一张卡片,最后一跳的出口是「落地」。
 * 卡片可以拖动排序,也可以用每张卡片上的按钮上移、下移、在后面插入、换成别的入口、删掉 —— 按钮都能用键盘点到。
 * 最多四台机器(含落地)。这是**拓扑**:与入口列表上调订阅先后的拖动排序是两件事。
 *
 * 「预览」逐段校验(与单跳组链同一份规则),给出从落地到入口的下发顺序;「应用」整条路径一次写入,
 * 再按那个顺序逐台下发 —— 每一台都会重启 sing-box,确认框里逐台写出来。
 * 逐台下发**不是跨机器的事务**(F02):某一台失败时逐台显示「已下发 / 失败 / 没下发」,
 * 不说「入口没动、旧路径不受影响」—— 中间跳先换了出口的话,旧路径的出口也跟着变了。
 */
const props = defineProps<{
  open: boolean
  entry: { id: number; name: string; nodeId: number; nodeName: string } | null
  nodes: Node[]
  externals: ExternalProxy[]
}>()
const emit = defineEmits<{ 'update:open': [boolean]; changed: [] }>()

interface Option {
  value: number
  label: string
  nodeId: number
}
const MAX_MACHINES = 4

/** 能当中间跳 / 落地的 sing-box 入口:另一台机器上、部署过、协议能当链式出口。 */
const inboundOptions = computed<Option[]>(() =>
  props.nodes.flatMap((n) =>
    (n.inbounds ?? [])
      .filter((i) => i.deployed_protocol === 'VLESS_REALITY' || i.deployed_protocol === 'SHADOWSOCKS')
      .map((i) => ({ value: i.id, label: `${n.display_name || n.name} / ${i.display_name}`, nodeId: n.id })),
  ),
)
const externalOptions = computed(() =>
  props.externals.filter((p) => p.dialable_by_node).map((p) => ({ value: p.id, label: p.display_name })),
)

const middle = ref<number[]>([])
const landingKind = ref<'INBOUND' | 'EXTERNAL'>('INBOUND')
const landingId = ref<number | null>(null)
const plan = ref<LinearChainPlan | null>(null)
/** 应用之后(逐台下发停在中途时)每一台的状态。 */
const applied = ref<LinearChainPlan | null>(null)
const stateText: Record<string, string> = { DEPLOYED: '已下发', FAILED: '失败', NOT_RUN: '没有下发' }
const busy = ref(false)
const dragFrom = ref<number | null>(null)

watch(
  () => props.open,
  (o) => {
    if (!o) return
    middle.value = []
    landingKind.value = 'INBOUND'
    landingId.value = null
    plan.value = null
    applied.value = null
  },
)
watch([middle, landingKind, landingId], () => (plan.value = null), { deep: true })

const machineCount = computed(() => 1 + middle.value.length + (landingKind.value === 'INBOUND' && landingId.value ? 1 : 0))
const canAddHop = computed(() => machineCount.value < MAX_MACHINES - (landingKind.value === 'INBOUND' && !landingId.value ? 1 : 0))

function insertAfter(i: number) {
  if (!canAddHop.value) return
  const first = inboundOptions.value[0]?.value
  if (first === undefined) return
  middle.value.splice(i + 1, 0, first)
}
function move(i: number, d: -1 | 1) {
  const j = i + d
  if (j < 0 || j >= middle.value.length) return
  const arr = middle.value
  ;[arr[i], arr[j]] = [arr[j], arr[i]]
}
function remove(i: number) {
  middle.value.splice(i, 1)
}
function onDrop(i: number) {
  const from = dragFrom.value
  dragFrom.value = null
  if (from === null || from === i) return
  const [x] = middle.value.splice(from, 1)
  middle.value.splice(i, 0, x)
}

const body = computed(() =>
  props.entry && landingId.value
    ? { entry_kind: 'SINGBOX' as const, hops: [props.entry.id, ...middle.value], landing_kind: landingKind.value, landing_id: landingId.value }
    : null,
)

async function preview() {
  if (!body.value) return
  busy.value = true
  try {
    plan.value = await api.linearChainPreview(body.value)
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '预览失败')
  } finally {
    busy.value = false
  }
}

function apply() {
  const p = plan.value
  if (!p?.ok || !body.value) return
  lbDangerConfirm({
    title: '应用这条多跳路径?',
    okText: '写入并逐台下发',
    okType: 'danger',
    impacts: [
      `**按这个顺序逐台下发,每一台都会重启 sing-box、断开它上面全部入口的在线连接**:${p.deploy_order.map((s) => s.node_name).join(' → ')}`,
      '某一台下发失败就停下:它之前的机器已经换上新配置,它与之后的还没有。这不是跨机器的事务 —— 中间跳先换了出口的话,还没下发的入口的旧路径出口也跟着变了',
      ...(p.warnings ?? []),
      p.note,
    ],
    onOk: () => {
      void doApply()
    },
  })
}

async function doApply() {
  if (!body.value) return
  busy.value = true
  try {
    await api.linearChainApply(body.value)
    message.success('多跳路径已写入并逐台下发')
    emit('changed')
    emit('update:open', false)
  } catch (e) {
    if (e instanceof ApiError && e.status === 409 && e.payload && typeof e.payload === 'object') {
      plan.value = e.payload as LinearChainPlan
      message.warning('路径通不过,一段都没有写入')
    } else if (e instanceof ApiError && e.status === 502 && e.payload && typeof e.payload === 'object' && 'plan' in e.payload) {
      // 路径已经写进库,逐台下发停在中途:把每一台的状态摆出来,修好之后再应用一次同一条路径。
      applied.value = (e.payload as { plan: LinearChainPlan }).plan
      message.error('逐台下发停在中途,每台机器的状态见下面')
      emit('changed')
    } else {
      message.error(e instanceof ApiError ? e.message : '应用失败')
    }
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <a-modal
    :open="open"
    :title="entry ? `多跳路径 · 从 ${entry.nodeName} / ${entry.name} 出发` : '多跳路径'"
    width="680px"
    :confirm-loading="busy"
    ok-text="应用"
    cancel-text="取消"
    :ok-button-props="{ disabled: !plan?.ok }"
    @ok="apply"
    @cancel="emit('update:open', false)"
  >
    <div v-if="entry" class="lc">
      <div class="lc__hint">
        从上到下依次经过:用户连第一张卡片,每一跳的出口是下一张。最多 {{ MAX_MACHINES }} 台机器(含落地),现在 {{ machineCount }} 台。
      </div>
      <ol class="lc__list">
        <li class="lc__card lc__card--fixed">
          <span class="lc__no">1</span>
          <span class="lc__name">{{ entry.nodeName }} / {{ entry.name }}</span>
          <span class="lc__tag">入口</span>
          <a-button size="small" :disabled="!canAddHop" @click="insertAfter(-1)">后面插一跳</a-button>
        </li>
        <li
          v-for="(id, i) in middle"
          :key="i + '-' + id"
          class="lc__card"
          draggable="true"
          @dragstart="dragFrom = i"
          @dragover.prevent
          @drop="onDrop(i)"
        >
          <span class="lc__no" aria-hidden="true">⠿ {{ i + 2 }}</span>
          <a-select
            v-model:value="middle[i]"
            :options="inboundOptions"
            size="small"
            show-search
            option-filter-prop="label"
            class="lc__select"
            :aria-label="`第 ${i + 2} 跳`"
          />
          <a-button size="small" :disabled="i === 0" :aria-label="`第 ${i + 2} 跳上移`" @click="move(i, -1)">↑</a-button>
          <a-button size="small" :disabled="i === middle.length - 1" :aria-label="`第 ${i + 2} 跳下移`" @click="move(i, 1)">↓</a-button>
          <a-button size="small" :disabled="!canAddHop" @click="insertAfter(i)">后面插一跳</a-button>
          <a-button size="small" danger @click="remove(i)">删掉</a-button>
        </li>
        <li class="lc__card lc__card--landing">
          <span class="lc__no">落地</span>
          <a-radio-group v-model:value="landingKind" size="small" @change="landingId = null">
            <a-radio-button value="INBOUND">另一台机器的入口</a-radio-button>
            <a-radio-button value="EXTERNAL">外部代理</a-radio-button>
          </a-radio-group>
          <a-select
            v-model:value="landingId"
            :options="landingKind === 'INBOUND' ? inboundOptions : externalOptions"
            size="small"
            show-search
            option-filter-prop="label"
            placeholder="选择最后一跳的出口"
            class="lc__select"
            aria-label="落地"
          />
        </li>
      </ol>

      <div class="lc__foot">
        <a-button size="small" :disabled="!body" :loading="busy" @click="preview">预览</a-button>
        <span v-if="!body" class="lc__hint">选好落地之后预览</span>
        <span v-else-if="!plan" class="lc__hint">应用按钮在预览通过之后才能点</span>
      </div>

      <template v-if="plan">
        <ul class="lc__segs">
          <li v-for="(s, i) in plan.segments" :key="i" :class="{ 'lc__bad': s.error }">
            第 {{ i + 1 }} 段:{{ s.from_node }} / {{ s.from_name }} → {{ s.to_name }}
            <span v-if="s.error">—— {{ s.error }}</span>
          </li>
        </ul>
        <div v-for="p in plan.problems.filter((x) => !plan!.segments.some((s) => s.error && x.includes(s.error)))" :key="p" class="lc__bad">
          {{ p }}
        </div>
        <div v-for="w in plan.warnings ?? []" :key="w" class="lc__warn">{{ w }}</div>
        <div v-if="plan.ok" class="lc__ok">
          可以应用。下发顺序(从落地到入口):{{ plan.deploy_order.map((s) => s.node_name).join(' → ') }}。{{ plan.note }}
        </div>
      </template>
      <template v-if="applied">
        <div class="lc__hint">路径已经写入。逐台下发的结果(从落地到入口):</div>
        <ul class="lc__segs">
          <li v-for="s in applied.deploy_order" :key="s.node_id" :class="{ lc__bad: s.state === 'FAILED' }">
            {{ s.node_name }}:{{ stateText[s.state ?? ''] ?? '未知' }}<span v-if="s.error">({{ s.error }})</span>
          </li>
        </ul>
        <div class="lc__warn">
          已下发的机器上是新配置;没下发的入口仍把流量送往它原来的下一跳 —— 那一跳要是已经下发,旧路径的出口也变了。
          修好之后再应用一次同一条路径。
        </div>
      </template>
    </div>
  </a-modal>
</template>

<style scoped>
.lc {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.lc__list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.lc__card {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  flex-wrap: wrap;
  padding: var(--sp-2) var(--sp-3);
  border: 1px solid var(--border);
  border-radius: var(--r-md);
  background: var(--surface);
}
.lc__card--fixed,
.lc__card--landing {
  background: var(--surface2);
}
.lc__no {
  min-width: 36px;
  font-size: var(--fs-xs);
  color: var(--text2);
  cursor: grab;
}
.lc__name {
  flex: 1;
  min-width: 0;
}
.lc__tag {
  font-size: var(--fs-xs);
  color: var(--text2);
}
.lc__select {
  flex: 1;
  min-width: 200px;
}
.lc__hint {
  font-size: var(--fs-xs);
  color: var(--text2);
}
.lc__foot {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
}
.lc__segs {
  margin: 0;
  padding-left: var(--sp-4);
  font-size: var(--fs-sm);
}
.lc__bad {
  color: var(--bad);
  font-size: var(--fs-sm);
}
.lc__ok {
  color: var(--ok);
  font-size: var(--fs-sm);
}
.lc__warn {
  color: var(--warn);
  font-size: var(--fs-sm);
}
</style>
