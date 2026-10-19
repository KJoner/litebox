<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type CardMode, type CardPick, type CardPrefs, type ProbeSummary } from '@/api/client'
import { LbIcon, LbInfoTip } from '@/components/lb'
import { protoChip, rateName, rateOf, rateText, rttText } from '@/components/asset/probeView'

/**
 * 探针卡片展示设置:这台节点的卡片上入站、出站各显示哪几项(各 0~3 项、有序、不重复)。
 *
 * **只改展示,不改检测**:没选上的序列照样在测、照样在下面的完整列表里;保存只写一张偏好表,
 * 不会创建 Boce 任务,也不会多花一个波点。自动推荐优先挑在测且有效的、尽量覆盖不同运营商 / 地区。
 */
const props = defineProps<{
  open: boolean
  nodeId: number
  prefs: CardPrefs
  pick: CardPick
  /** 入站的全部序列(主控 + Boce)与出站序列。 */
  ingress: ProbeSummary[]
  outbound: ProbeSummary[]
}>()
const emit = defineEmits<{ (e: 'close'): void; (e: 'saved'): void }>()

const MAX = 3
const form = reactive({ ingress_mode: 'AUTO' as CardMode, ingress_ids: [] as number[], outbound_mode: 'AUTO' as CardMode, outbound_ids: [] as number[] })
const saving = ref(false)
const error = ref('')

watch(
  () => props.open,
  (open) => {
    if (!open) return
    error.value = ''
    form.ingress_mode = props.prefs.ingress_mode
    form.outbound_mode = props.prefs.outbound_mode
    // 手动模式从当前的选择开始;自动模式切过去时从自动推荐的那几项开始改。
    form.ingress_ids = props.prefs.ingress_mode === 'MANUAL' ? [...props.prefs.ingress_ids] : [...props.pick.auto_ingress]
    form.outbound_ids = props.prefs.outbound_mode === 'MANUAL' ? [...props.prefs.outbound_ids] : [...props.pick.auto_outbound]
  },
  { immediate: true },
)

const byID = computed(() => {
  const m = new Map<number, ProbeSummary>()
  for (const s of [...props.ingress, ...props.outbound]) m.set(s.id, s)
  return m
})

function itemText(s: ProbeSummary): string {
  if (s.source === 'BOCE') {
    return `Boce ${s.metric === 'BOCE_PING' ? 'Ping' : 'TCPing'} · ${s.boce?.vantage_name ?? s.label} · ${s.boce?.task_name ?? ''}`
  }
  if (s.direction === 'INGRESS') return `主控 · ${s.label} · ${protoChip(s)}`
  return `${s.label} · ${protoChip(s)}`
}
function itemValue(s: ProbeSummary): string {
  const rtt = s.last && (s.last.status === 'OK' || s.last.status === 'LOSS') ? rttText(s.last.rtt_ms) : (s.last?.label ?? '暂无数据')
  return `${rtt} · 24h ${rateName(s.metric)} ${rateText(rateOf(s.h24, s.metric))}`
}

type Dir = 'ingress' | 'outbound'
function ids(dir: Dir): number[] {
  return dir === 'ingress' ? form.ingress_ids : form.outbound_ids
}
function setIDs(dir: Dir, v: number[]) {
  if (dir === 'ingress') form.ingress_ids = v
  else form.outbound_ids = v
}
function mode(dir: Dir): CardMode {
  return dir === 'ingress' ? form.ingress_mode : form.outbound_mode
}
function setMode(dir: Dir, m: CardMode) {
  if (dir === 'ingress') form.ingress_mode = m
  else form.outbound_mode = m
}
function candidates(dir: Dir): ProbeSummary[] {
  const list = dir === 'ingress' ? props.ingress : props.outbound
  const used = new Set(ids(dir))
  return list.filter((s) => !used.has(s.id))
}
function autoList(dir: Dir): number[] {
  return dir === 'ingress' ? props.pick.auto_ingress : props.pick.auto_outbound
}
function invalidCount(dir: Dir): number {
  return dir === 'ingress' ? props.pick.invalid_ingress.length : props.pick.invalid_outbound.length
}

function move(dir: Dir, i: number, delta: number) {
  const list = [...ids(dir)]
  const j = i + delta
  if (j < 0 || j >= list.length) return
  ;[list[i], list[j]] = [list[j]!, list[i]!]
  setIDs(dir, list)
}
function removeAt(dir: Dir, i: number) {
  setIDs(dir, ids(dir).filter((_, k) => k !== i))
}
function add(dir: Dir, id: unknown) {
  const n = Number(id)
  if (!n || ids(dir).length >= MAX || ids(dir).includes(n)) return
  setIDs(dir, [...ids(dir), n])
}

function restoreAuto() {
  form.ingress_mode = 'AUTO'
  form.outbound_mode = 'AUTO'
  form.ingress_ids = [...props.pick.auto_ingress]
  form.outbound_ids = [...props.pick.auto_outbound]
}

async function save() {
  saving.value = true
  error.value = ''
  try {
    await api.setCardPrefs(props.nodeId, {
      ingress_mode: form.ingress_mode,
      ingress_ids: form.ingress_mode === 'MANUAL' ? form.ingress_ids : [],
      outbound_mode: form.outbound_mode,
      outbound_ids: form.outbound_mode === 'MANUAL' ? form.outbound_ids : [],
    })
    message.success('卡片展示已保存(只改展示,不影响检测)')
    emit('saved')
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    saving.value = false
  }
}

const sections: { dir: Dir; title: string; empty: string }[] = [
  { dir: 'ingress', title: '入站(主控 / Boce)', empty: '这台节点还没有入站序列(主控入口连通性或 Boce 拨测任务)' },
  { dir: 'outbound', title: '出站(节点本机)', empty: '这台节点还没有出站序列(在目标库里选择公共目标)' },
]
</script>

<template>
  <a-modal :open="open" title="探针卡片展示设置" :width="680" :mask-closable="false" @cancel="emit('close')">
    <div class="cdm" data-testid="card-display">
      <div class="cdm__muted">
        仪表盘卡片上入站、出站各最多显示 3 项。这里只决定<b>显示哪几项</b>:没选上的照样在测、照样在节点详情里看得到;
        保存不会创建 Boce 任务,也不会多花一个波点。
      </div>
      <div v-if="error" class="lb-error-strip">{{ error }}</div>
      <section v-for="sec in sections" :key="sec.dir" class="cdm__sec" :data-testid="`card-display-${sec.dir}`">
        <div class="cdm__sec-head">
          <span class="cdm__sec-title">{{ sec.title }}({{ mode(sec.dir) === 'AUTO' ? autoList(sec.dir).length : ids(sec.dir).length }}/{{ MAX }})</span>
          <div class="lb-seg lb-seg--sm" role="radiogroup" :aria-label="`${sec.title}的展示方式`">
            <button
              v-for="m in (['AUTO', 'MANUAL'] as const)"
              :key="m"
              type="button"
              role="radio"
              class="lb-seg__item"
              :class="{ 'lb-seg__item--on': mode(sec.dir) === m }"
              :aria-checked="mode(sec.dir) === m"
              @click="setMode(sec.dir, m)"
            >
              {{ m === 'AUTO' ? '自动推荐' : '手动选择' }}
            </button>
          </div>
        </div>
        <div v-if="invalidCount(sec.dir)" class="cdm__warn">
          <LbIcon name="alert-triangle" :size="13" />有 {{ invalidCount(sec.dir) }} 个已选项目对应的序列已经删除:卡片上不再显示它们,也不会自动换成别的测点。保存一次即可清掉。
        </div>
        <div v-if="!(sec.dir === 'ingress' ? ingress : outbound).length" class="cdm__muted">{{ sec.empty }}</div>
        <template v-else-if="mode(sec.dir) === 'AUTO'">
          <div class="cdm__muted">
            自动推荐:优先在测且有效的,尽量覆盖不同运营商 / 地区。
            <LbInfoTip :width="280" text="目标列表变化时自动推荐会跟着变;想固定下来就切到「手动选择」。" />
          </div>
          <ol class="cdm__list">
            <li v-for="id in autoList(sec.dir)" :key="id" class="cdm__item cdm__item--ro">
              <span class="cdm__name lb-ellipsis">{{ byID.get(id) ? itemText(byID.get(id)!) : `#${id}` }}</span>
              <span v-if="byID.get(id)" class="cdm__val lb-tabular">{{ itemValue(byID.get(id)!) }}</span>
            </li>
          </ol>
        </template>
        <template v-else>
          <ol class="cdm__list">
            <li v-for="(id, i) in ids(sec.dir)" :key="id" class="cdm__item">
              <span class="cdm__name lb-ellipsis">{{ byID.get(id) ? itemText(byID.get(id)!) : `已失效 #${id}` }}</span>
              <span v-if="byID.get(id)" class="cdm__val lb-tabular">{{ itemValue(byID.get(id)!) }}</span>
              <span class="cdm__ops">
                <button type="button" class="cdm__icon" :disabled="i === 0" :aria-label="`上移第 ${i + 1} 项`" @click="move(sec.dir, i, -1)">
                  <LbIcon name="arrow-up" :size="13" />
                </button>
                <button
                  type="button"
                  class="cdm__icon"
                  :disabled="i === ids(sec.dir).length - 1"
                  :aria-label="`下移第 ${i + 1} 项`"
                  @click="move(sec.dir, i, 1)"
                >
                  <LbIcon name="arrow-down" :size="13" />
                </button>
                <button type="button" class="cdm__icon" :aria-label="`不显示第 ${i + 1} 项`" @click="removeAt(sec.dir, i)">
                  <LbIcon name="x" :size="13" />
                </button>
              </span>
            </li>
          </ol>
          <div v-if="!ids(sec.dir).length" class="cdm__muted">一项都不显示(允许)</div>
          <a-select
            :value="undefined"
            :disabled="ids(sec.dir).length >= MAX || !candidates(sec.dir).length"
            :placeholder="ids(sec.dir).length >= MAX ? `已选满 ${MAX} 项` : '添加一项'"
            class="cdm__add"
            :aria-label="`添加${sec.title}展示项`"
            show-search
            option-filter-prop="label"
            @update:value="(v: unknown) => add(sec.dir, v)"
          >
            <a-select-option v-for="s in candidates(sec.dir)" :key="s.id" :value="s.id" :label="itemText(s)">
              {{ itemText(s) }}<template v-if="!s.enabled">(未在测)</template>
            </a-select-option>
          </a-select>
        </template>
      </section>
    </div>
    <template #footer>
      <div class="cdm__foot">
        <a-button data-testid="card-display-auto" @click="restoreAuto">恢复自动推荐</a-button>
        <span class="cdm__spacer" />
        <a-button @click="emit('close')">取消</a-button>
        <a-button type="primary" :loading="saving" data-testid="card-display-save" @click="save">保存</a-button>
      </div>
    </template>
  </a-modal>
</template>

<style scoped>
.cdm {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.cdm__sec {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-3);
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
}
.cdm__sec-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
}
.cdm__sec-title {
  font-weight: 600;
}
.cdm__list {
  list-style: decimal inside;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.cdm__item {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
  padding: 6px var(--sp-2);
  border-radius: var(--r-sm);
  background: var(--surface2);
  font-size: var(--fs-sm);
}
.cdm__name {
  flex: 1 1 200px;
  min-width: 0;
}
.cdm__val {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.cdm__ops {
  display: inline-flex;
  gap: 2px;
}
.cdm__icon {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 26px;
  height: 26px;
  border: 1px solid var(--sep);
  border-radius: var(--r-sm);
  background: var(--surface);
  color: var(--text2);
  cursor: pointer;
}
.cdm__icon:disabled {
  opacity: 0.4;
  cursor: default;
}
.cdm__add {
  width: 100%;
}
.cdm__warn {
  display: flex;
  align-items: flex-start;
  gap: 6px;
  font-size: var(--fs-xs);
  color: var(--warn);
}
.cdm__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
  line-height: 1.6;
}
.cdm__foot {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
}
.cdm__spacer {
  flex: 1;
}
</style>
