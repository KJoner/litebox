<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError } from '@/api/client'
import { assignSortValues, moveKeys, type SortRow } from './sortMode'

/**
 * 排序模式(VNext UI06 / A19):多选、拖动、上移 / 下移 / 置顶 / 置底,一次提交整组。
 *
 * 几条规矩:
 *   一、这里永远是**完整顺序**(这个范围里的全部条目),不受页面筛选影响 —— 筛选状态下只挪看得见的几项,
 *       看不见的被挤到哪里去是一件谁都说不清的事。
 *   二、提交带每一项的旧值;期间有人改过其中任何一项,后端整组拒绝(409),这里提示刷新,不覆盖别人的改动。
 *   三、保存失败时列表回到打开时的顺序(回退),不留一个"看起来已经排好了"的假象。
 *   四、纯排序不进节点配置、不部署 —— 只影响订阅与门户里的先后。
 */
const props = defineProps<{
  open: boolean
  title: string
  /** 一句话说明这个范围是什么(哪台机器的入口 / 全部机器)。 */
  scope: string
  rows: SortRow[]
  min: number
  max: number
}>()
const emit = defineEmits<{ 'update:open': [boolean]; saved: [] }>()

const list = ref<SortRow[]>([])
const selected = ref<Set<string>>(new Set())
const saving = ref(false)
const dragKey = ref<string | null>(null)
const overIndex = ref<number | null>(null)

watch(
  () => props.open,
  (open) => {
    if (!open) return
    list.value = [...props.rows]
    selected.value = new Set()
  },
  { immediate: true },
)

const dirty = computed(() => list.value.some((r, i) => r.key !== props.rows[i]?.key))

function toggle(key: string, on: boolean) {
  const next = new Set(selected.value)
  if (on) next.add(key)
  else next.delete(key)
  selected.value = next
}

function targetKeys(key?: string): string[] {
  // 拖动或操作的是一个已选中的行时,带上全部选中的行;否则只动这一行。
  if (key && !selected.value.has(key)) return [key]
  return list.value.filter((r) => selected.value.has(r.key)).map((r) => r.key)
}

function move(where: 'up' | 'down' | 'top' | 'bottom', key?: string) {
  const keys = targetKeys(key)
  if (!keys.length) return
  list.value = moveKeys(list.value, keys, where)
}

function onDragStart(key: string, e: DragEvent) {
  dragKey.value = key
  e.dataTransfer?.setData('text/plain', key)
}
function onDrop(index: number) {
  if (dragKey.value) list.value = moveKeys(list.value, targetKeys(dragKey.value), index)
  dragKey.value = null
  overIndex.value = null
}

function onKey(e: KeyboardEvent, key: string) {
  if (!e.altKey) return
  if (e.key === 'ArrowUp') {
    e.preventDefault()
    move('up', key)
  } else if (e.key === 'ArrowDown') {
    e.preventDefault()
    move('down', key)
  }
}

async function save() {
  const plan = assignSortValues(props.rows, list.value, props.min, props.max)
  if ('error' in plan) {
    message.error(plan.error)
    return
  }
  if (!plan.items.length) {
    emit('update:open', false)
    return
  }
  saving.value = true
  try {
    await api.sortBatch(plan.items)
    message.success(`已保存 ${plan.items.length} 项的新顺序(只影响订阅与门户里的先后,不下发任何机器)`)
    emit('saved')
    emit('update:open', false)
  } catch (e) {
    // 回退到打开时的顺序。
    list.value = [...props.rows]
    if (e instanceof ApiError && e.status === 409) {
      message.error(e.message || '有条目在你调整期间被改过,这次一项都没保存;请刷新后重新调整')
      emit('saved')
    } else {
      message.error(e instanceof ApiError ? e.message : '保存失败,顺序已回退')
    }
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <a-modal
    :open="open"
    :title="title"
    width="560px"
    :confirm-loading="saving"
    ok-text="保存顺序"
    cancel-text="取消"
    :ok-button-props="{ disabled: !dirty }"
    @ok="save"
    @cancel="emit('update:open', false)"
  >
    <div class="sm">
      <div class="sm__scope">{{ scope }}。这里是完整顺序,不受页面上的筛选影响;勾选多项后一起移动。</div>
      <div class="sm__bar">
        <a-button size="small" :disabled="!selected.size" @click="move('top')">置顶</a-button>
        <a-button size="small" :disabled="!selected.size" @click="move('up')">上移</a-button>
        <a-button size="small" :disabled="!selected.size" @click="move('down')">下移</a-button>
        <a-button size="small" :disabled="!selected.size" @click="move('bottom')">置底</a-button>
        <span class="sm__hint">也可以拖动,或聚焦一行后按 Alt + ↑ / ↓</span>
      </div>
      <ol class="sm__list" aria-label="排序列表">
        <li
          v-for="(r, i) in list"
          :key="r.key"
          class="sm__row"
          :class="{ 'sm__row--sel': selected.has(r.key), 'sm__row--over': overIndex === i }"
          draggable="true"
          tabindex="0"
          :aria-label="`第 ${i + 1} 位:${r.label}`"
          @dragstart="onDragStart(r.key, $event)"
          @dragover.prevent="overIndex = i"
          @dragleave="overIndex = null"
          @drop.prevent="onDrop(i)"
          @keydown="onKey($event, r.key)"
        >
          <span class="sm__grip" aria-hidden="true">⋮⋮</span>
          <a-checkbox :checked="selected.has(r.key)" @change="(e: any) => toggle(r.key, e.target.checked)" />
          <span class="sm__no lb-tabular">{{ i + 1 }}</span>
          <span class="sm__label">{{ r.label }}</span>
          <span v-if="r.sub" class="sm__sub">{{ r.sub }}</span>
        </li>
      </ol>
    </div>
  </a-modal>
</template>

<style scoped>
.sm {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.sm__scope,
.sm__hint {
  font-size: var(--fs-xs);
  color: var(--text2);
}
.sm__bar {
  display: flex;
  gap: var(--sp-2);
  align-items: center;
  flex-wrap: wrap;
}
.sm__list {
  list-style: none;
  margin: 0;
  padding: 0;
  max-height: 420px;
  overflow-y: auto;
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
}
.sm__row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: 6px var(--sp-3);
  border-bottom: 1px solid var(--sep2);
  background: var(--surface);
  cursor: grab;
}
.sm__row:last-child {
  border-bottom: none;
}
.sm__row--sel {
  background: var(--brand-bg);
}
.sm__row--over {
  box-shadow: inset 0 2px 0 var(--brand);
}
.sm__row:focus-visible {
  outline: 2px solid var(--brand);
  outline-offset: -2px;
}
.sm__grip {
  color: var(--text3);
  letter-spacing: -2px;
}
.sm__no {
  width: 24px;
  color: var(--text3);
  font-size: var(--fs-xs);
}
.sm__label {
  font-size: var(--fs-sm);
}
.sm__sub {
  margin-left: auto;
  font-size: var(--fs-xs);
  color: var(--text3);
}
</style>
