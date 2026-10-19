<script setup lang="ts">
import { computed, ref } from 'vue'
import type { BoceVantage } from '@/api/client'
import { LbIcon } from '@/components/lb'
import { ISP_QUICK, ispGroup, vantageLabel } from './boceText'

/**
 * Boce 拨测点选择器:中国大陆 / 海外、三网快捷筛选、关键词搜索、多选。
 *
 * 列表来自面板缓存的节点列表(系统设置里同步),打开这里不请求 Boce。地区与运营商只显示接口原样给的值;
 * 已经从上游列表消失的拨测点标出来:原来选着的可以留着(看得见它不在了),新加的不收 ——
 * 不会悄悄换成另一个付费拨测点。
 */
const props = withDefaults(
  defineProps<{
    vantages: BoceVantage[]
    modelValue: number[]
    /** 这些 ID 即使已消失也允许保留(编辑任务时原来选着的)。 */
    keep?: number[]
    max?: number
    /** 每个拨测点每次的估算波点(显示「每轮约 N 波点」)。 */
    pointsPerCheck?: number
  }>(),
  { keep: () => [], max: 50, pointsPerCheck: 1 },
)
const emit = defineEmits<{ (e: 'update:modelValue', v: number[]): void }>()

const area = ref<'CN' | 'OVERSEA'>('CN')
const isp = ref<string>('')
const q = ref('')

const byID = computed(() => {
  const m = new Map<number, BoceVantage>()
  for (const v of props.vantages) m.set(v.id, v)
  return m
})
const selected = computed(() => new Set(props.modelValue))
const keepSet = computed(() => new Set(props.keep))

/** 这个大区里真实出现过的运营商分组:没有的快捷项不列。 */
const ispOptions = computed(() => {
  const groups = new Set(props.vantages.filter((v) => v.area === area.value).map((v) => ispGroup(v.isp_name)))
  const out = ISP_QUICK.filter((x) => groups.has(x.key)).map((x) => ({ key: x.key as string, label: x.label as string }))
  if (groups.has('OTHER')) out.push({ key: 'OTHER', label: area.value === 'OVERSEA' ? '未标运营商 / 其他' : '其他' })
  return out
})

const shown = computed(() => {
  const kw = q.value.trim().toLowerCase()
  return props.vantages.filter((v) => {
    if (v.area !== area.value) return false
    if (isp.value && ispGroup(v.isp_name) !== isp.value) return false
    if (kw && !`${v.node_name} ${v.isp_name} ${v.id}`.toLowerCase().includes(kw)) return false
    return true
  })
})

function disabled(v: BoceVantage): boolean {
  if (selected.value.has(v.id)) return false
  if (v.missing_since && !keepSet.value.has(v.id)) return true
  return props.modelValue.length >= props.max
}

function toggle(v: BoceVantage) {
  if (selected.value.has(v.id)) {
    emit('update:modelValue', props.modelValue.filter((id) => id !== v.id))
    return
  }
  if (disabled(v)) return
  emit('update:modelValue', [...props.modelValue, v.id])
}

function remove(id: number) {
  emit('update:modelValue', props.modelValue.filter((x) => x !== id))
}

function labelOf(id: number): string {
  const v = byID.value.get(id)
  return v ? vantageLabel(v) : `拨测点 #${id}`
}
function missing(id: number): boolean {
  const v = byID.value.get(id)
  return !v || !!v.missing_since
}
</script>

<template>
  <div class="bvp" data-testid="boce-vantage-picker">
    <div class="bvp__chosen">
      <span class="bvp__muted lb-tabular">
        已选 {{ modelValue.length }} 个 · 每轮约 {{ modelValue.length * pointsPerCheck }} 波点
      </span>
      <span
        v-for="id in modelValue"
        :key="id"
        class="lb-chip bvp__tag"
        :class="{ 'bvp__tag--missing': missing(id) }"
        :title="missing(id) ? '这个拨测点已经从 Boce 节点列表里消失:可以留着,但它可能不再出结果' : ''"
      >
        {{ labelOf(id) }}<template v-if="missing(id)"> · 已消失</template>
        <button type="button" class="bvp__x" :aria-label="`移除 ${labelOf(id)}`" @click="remove(id)">
          <LbIcon name="x" :size="11" />
        </button>
      </span>
    </div>
    <div class="lb-filter bvp__bar">
      <div class="lb-seg lb-seg--sm" role="tablist" aria-label="大区">
        <button
          v-for="a in (['CN', 'OVERSEA'] as const)"
          :key="a"
          type="button"
          role="tab"
          class="lb-seg__item"
          :class="{ 'lb-seg__item--on': area === a }"
          :aria-selected="area === a"
          @click="(area = a), (isp = '')"
        >
          {{ a === 'CN' ? '中国大陆' : '海外' }}
        </button>
      </div>
      <div class="lb-seg lb-seg--sm" role="radiogroup" aria-label="运营商">
        <button type="button" role="radio" class="lb-seg__item" :class="{ 'lb-seg__item--on': !isp }" :aria-checked="!isp" @click="isp = ''">
          全部
        </button>
        <button
          v-for="o in ispOptions"
          :key="o.key"
          type="button"
          role="radio"
          class="lb-seg__item"
          :class="{ 'lb-seg__item--on': isp === o.key }"
          :aria-checked="isp === o.key"
          @click="isp = o.key"
        >
          {{ o.label }}
        </button>
      </div>
      <a-input v-model:value="q" allow-clear size="small" placeholder="搜索地区 / 运营商" aria-label="搜索拨测点" class="bvp__q">
        <template #prefix><LbIcon name="search" :size="13" /></template>
      </a-input>
    </div>
    <div v-if="!vantages.length" class="bvp__empty">
      还没有拨测点列表:先在「系统设置 → Boce 外部拨测」里同步一次节点列表(免费接口)。
    </div>
    <div v-else-if="!shown.length" class="bvp__empty">没有符合条件的拨测点</div>
    <ul v-else class="bvp__list" role="listbox" aria-multiselectable="true" aria-label="拨测点">
      <li v-for="v in shown" :key="v.id">
        <label class="bvp__item" :class="{ 'bvp__item--off': disabled(v) }">
          <a-checkbox :checked="selected.has(v.id)" :disabled="disabled(v)" @change="toggle(v)" />
          <span class="bvp__name">{{ v.node_name }}</span>
          <span v-if="v.isp_name" class="lb-chip">{{ v.isp_name }}</span>
          <span v-if="v.missing_since" class="lb-chip bvp__tag--missing">已从列表消失</span>
          <span class="bvp__id lb-tabular">#{{ v.id }}</span>
        </label>
      </li>
    </ul>
  </div>
</template>

<style scoped>
.bvp {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.bvp__chosen {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
  min-height: 24px;
}
.bvp__tag {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
.bvp__tag--missing {
  color: var(--warn);
  background: var(--warn-bg);
}
.bvp__x {
  display: inline-flex;
  padding: 0;
  border: none;
  background: transparent;
  color: var(--text3);
  cursor: pointer;
}
.bvp__bar {
  flex-wrap: wrap;
  gap: var(--sp-2);
}
.bvp__q {
  width: 180px;
}
.bvp__list {
  list-style: none;
  margin: 0;
  padding: 4px;
  max-height: 240px;
  overflow: auto;
  border: 1px solid var(--sep);
  border-radius: var(--r-sm);
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
  gap: 2px var(--sp-2);
}
.bvp__item {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 4px 6px;
  border-radius: var(--r-xs, 4px);
  cursor: pointer;
  font-size: var(--fs-sm);
}
.bvp__item:hover {
  background: var(--fill);
}
.bvp__item--off {
  cursor: not-allowed;
  color: var(--text3);
}
.bvp__name {
  min-width: 0;
}
.bvp__id {
  margin-left: auto;
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.bvp__muted,
.bvp__empty {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.bvp__empty {
  padding: var(--sp-2) 0;
}
</style>
