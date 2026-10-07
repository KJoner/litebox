<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type OrderPlanItem, type OrderScheme } from '@/api/client'
import {
  ENTRY_SORT_MAX,
  ENTRY_SORT_MIN,
  GLOBAL_STRIDE,
  NODE_SORT_MAX,
  NODE_SORT_MIN,
} from './entryOrder'

/**
 * 调整一条入口的排序(V20)。
 *
 * 自建入口有两个数:**节点排序号**(改它会连带这台机器上全部入口一起挪)与
 * **节点内的入口序号**;外部代理只有一个自带的全局排序值。两种情形必须在
 * 界面上分开说清楚 —— 管理员改完才知道挪动的是一台机器还是一个入口。
 *
 * 排序只影响订阅与门户里的先后:不进节点配置、不部署、不重启服务,
 * 也不改访问权限、凭据、订阅名称与协议参数。
 */
export interface SortTarget {
  kind: OrderPlanItem['kind']
  id: number
  name: string
  /** 自建入口所属的机器;外部代理没有 */
  nodeId?: number
  nodeName?: string
  nodeSort?: number
  /** 自建入口的节点内序号,或外部代理的全局排序值 */
  sort: number
}

const props = defineProps<{
  open: boolean
  target: SortTarget | null
  scheme: OrderScheme
}>()
const emit = defineEmits<{
  (e: 'update:open', v: boolean): void
  (e: 'saved'): void
}>()

const form = reactive({ nodeSort: 0, sort: 0 })
const saving = ref(false)
const isExternal = computed(() => props.target?.kind === 'EXTERNAL')
const isGlobal = computed(() => props.scheme === 'GLOBAL')

watch(
  () => [props.open, props.target] as const,
  ([open, t]) => {
    if (!open || !t) return
    form.nodeSort = t.nodeSort ?? 0
    form.sort = t.sort
  },
  { immediate: true },
)

/** 改完之后这一条的全局排序值(只在 GLOBAL 方案下有意义)。 */
const previewGlobal = computed(() =>
  isExternal.value ? form.sort : form.nodeSort * GLOBAL_STRIDE + form.sort,
)

const nodeRange = computed(() => (isGlobal.value ? `${NODE_SORT_MIN}~${NODE_SORT_MAX}` : '≥ 0'))
const entryRange = computed(() => (isGlobal.value ? `${ENTRY_SORT_MIN}~${ENTRY_SORT_MAX}` : '≥ 0'))

function validate(): string {
  if (isExternal.value) {
    if (!Number.isInteger(form.sort) || form.sort < 0) return '全局排序值必须是非负整数'
    return ''
  }
  if (isGlobal.value) {
    if (form.nodeSort < NODE_SORT_MIN || form.nodeSort > NODE_SORT_MAX) return `节点排序号必须在 ${nodeRange.value} 内`
    if (form.sort < ENTRY_SORT_MIN || form.sort > ENTRY_SORT_MAX) return `入口序号必须在 ${entryRange.value} 内`
  } else if (form.nodeSort < 0 || form.sort < 0) {
    return '排序号不能是负数'
  }
  return ''
}

async function save() {
  const t = props.target
  if (!t) return
  const err = validate()
  if (err) {
    message.error(err)
    return
  }
  saving.value = true
  try {
    const done: string[] = []
    if (!isExternal.value && t.nodeId && form.nodeSort !== (t.nodeSort ?? 0)) {
      await api.setEntrySortOrder('NODE', t.nodeId, form.nodeSort)
      done.push(`机器「${t.nodeName ?? t.nodeId}」的排序号改为 ${form.nodeSort}(它上面的入口一起挪)`)
    }
    if (form.sort !== t.sort) {
      await api.setEntrySortOrder(t.kind, t.id, form.sort)
      done.push(isExternal.value ? `全局排序值改为 ${form.sort}` : `入口序号改为 ${form.sort}`)
    }
    if (!done.length) {
      message.info('没有改动')
    } else {
      message.success(done.join(';'))
      emit('saved')
    }
    emit('update:open', false)
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '保存排序失败')
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <a-modal
    :open="open"
    :title="target ? `调整排序 · ${target.name}` : '调整排序'"
    :width="520"
    ok-text="保存"
    cancel-text="取消"
    :confirm-loading="saving"
    @ok="save"
    @cancel="emit('update:open', false)"
  >
    <template v-if="target">
      <div v-if="isExternal" class="es__block">
        <div class="es__label">全局排序值(非负整数)</div>
        <a-input-number v-model:value="form.sort" :min="0" :precision="0" style="width: 180px" />
        <div class="es__hint">
          <template v-if="isGlobal">
            自建入口的全局排序值 = 节点排序号 × {{ GLOBAL_STRIDE }} + 入口序号。节点 1 的入口在 1000~1999,
            填 1500 就落在节点 1 与节点 2 之间;与某个自建入口同值时排在它后面。
          </template>
          <template v-else>
            当前是旧排序方案:外部代理整块排在自建节点之前或之后,这个数只决定外部代理之间的先后。
            到「系统设置 → 订阅排序」迁移到全局排序之后,才能把它插到某台机器之间。
          </template>
        </div>
      </div>

      <template v-else>
        <div class="es__block">
          <div class="es__label">
            节点排序号({{ nodeRange }})
            <span class="es__warn">—— 改它会把机器「{{ target.nodeName }}」上的全部入口一起挪</span>
          </div>
          <a-input-number
            v-model:value="form.nodeSort"
            :min="isGlobal ? NODE_SORT_MIN : 0"
            :max="isGlobal ? NODE_SORT_MAX : undefined"
            :precision="0"
            style="width: 180px"
          />
        </div>
        <div class="es__block">
          <div class="es__label">节点内入口序号({{ entryRange }})—— 只挪这一个入口</div>
          <a-input-number
            v-model:value="form.sort"
            :min="0"
            :max="isGlobal ? ENTRY_SORT_MAX : undefined"
            :precision="0"
            style="width: 180px"
          />
        </div>
        <div class="es__hint">
          <template v-if="isGlobal">
            保存后这一条的全局排序值是 <b class="lb-tabular">{{ previewGlobal }}</b>
            (节点排序号 × {{ GLOBAL_STRIDE }} + 入口序号)。同一台机器上序号相同的入口按种类与 id 先后。
          </template>
          <template v-else>
            当前是旧排序方案:先按机器的排序号,再按入口序号;外部代理不参与。
          </template>
        </div>
      </template>

      <div class="es__foot">
        排序只影响订阅与门户里的先后:不进节点配置、不部署、不重启服务,也不改访问权限、凭据与协议参数。
      </div>
    </template>
  </a-modal>
</template>

<style scoped>
.es__block {
  margin-bottom: 14px;
}
.es__label {
  margin-bottom: 6px;
  font-size: 13px;
  color: var(--text2);
}
.es__warn {
  color: var(--warn);
}
.es__hint {
  margin-top: 6px;
  font-size: 12.5px;
  line-height: 1.6;
  color: var(--text3);
}
.es__foot {
  margin-top: 10px;
  padding-top: 10px;
  border-top: 1px solid var(--sep2);
  font-size: 12px;
  color: var(--text3);
}
</style>
