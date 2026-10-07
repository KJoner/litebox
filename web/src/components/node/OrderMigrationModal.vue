<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type OrderPlan, type OrderPlanItem } from '@/api/client'
import { LbEmptyState, lbDangerConfirm } from '@/components/lb'

/**
 * 旧排序 → 全局排序值的迁移预览(V20)。
 *
 * 预览与执行用的是后端同一份计划:这里看到的每一行,执行时写进去的就是它。
 * 计划里有放不下的数据(节点超过 1000 台、一台机器上入口超过 1000 个、
 * 排在前面的外部代理超过 1000 条)时执行按钮禁用,一行都不改。
 */
const props = defineProps<{ open: boolean }>()
const emit = defineEmits<{
  (e: 'update:open', v: boolean): void
  (e: 'applied'): void
}>()

const plan = ref<OrderPlan | null>(null)
const loading = ref(false)
const error = ref('')
const applying = ref(false)

async function load() {
  loading.value = true
  error.value = ''
  try {
    plan.value = await api.orderMigrationPlan()
  } catch (e) {
    plan.value = null
    error.value = e instanceof ApiError ? e.message : '读取迁移计划失败'
  } finally {
    loading.value = false
  }
}

watch(
  () => props.open,
  (v) => {
    if (v) void load()
  },
  { immediate: true },
)

const kindLabel: Record<OrderPlanItem['kind'], string> = {
  NODE: '节点',
  SINGBOX: 'sing-box',
  MIERU: 'Mieru',
  NGINX: 'nginx 转发',
  REALM: 'realm 转发',
  EXTERNAL: '外部代理',
}

const changedOnly = ref(false)
function rowsOf(list: OrderPlanItem[]): OrderPlanItem[] {
  return changedOnly.value ? list.filter((i) => i.old_sort !== i.new_sort || i.note) : list
}
const nodeRows = computed(() => rowsOf(plan.value?.nodes ?? []))
const entryRows = computed(() => rowsOf(plan.value?.entries ?? []))
const externalRows = computed(() => rowsOf(plan.value?.externals ?? []))

const blocked = computed(() => !!plan.value && plan.value.errors.length > 0)
const alreadyGlobal = computed(() => plan.value?.scheme === 'GLOBAL')

function apply() {
  if (!plan.value || blocked.value) return
  lbDangerConfirm({
    title: alreadyGlobal.value ? '重新密排全部排序值?' : '迁移到全局排序方案?',
    okText: alreadyGlobal.value ? '重新密排' : '执行迁移',
    okType: 'primary',
    impacts: [
      `改写 ${plan.value.changed} 行排序值(节点 ${plan.value.nodes.length} 台、入口 ${plan.value.entries.length} 个、外部代理 ${plan.value.externals.length} 条),相对顺序逐条保持`,
      '只改排序号:不进节点配置、不部署、不重启任何服务,也不改权限、凭据与订阅名称',
      alreadyGlobal.value
        ? '当前已是全局方案,这一下只把手工改乱的号重新排紧'
        : '订阅排序方案切换为 GLOBAL;之后「外部代理位置」这项设置不再起作用,外部代理各自带一个全局排序值',
      '可以在系统设置里切回旧方案;密排后的值在旧方案下顺序相同',
    ],
    onOk: () => {
      void doApply()
    },
  })
}

async function doApply() {
  applying.value = true
  try {
    const r = await api.applyOrderMigration()
    plan.value = r
    message.success(`已切换到全局排序,改写了 ${r.changed} 行`)
    emit('applied')
    emit('update:open', false)
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '迁移失败')
  } finally {
    applying.value = false
  }
}

const columns = [
  { title: '对象', key: 'name' },
  { title: '原值', key: 'old', width: 90 },
  { title: '新值', key: 'new', width: 90 },
  { title: '全局排序值', key: 'global', width: 110 },
  { title: '说明', key: 'note' },
]
</script>

<template>
  <a-modal
    :open="open"
    title="迁移到全局排序:预览"
    :width="860"
    :footer="null"
    :mask-closable="!applying"
    :keyboard="!applying"
    @cancel="emit('update:open', false)"
  >
    <a-skeleton v-if="loading" active :paragraph="{ rows: 4 }" />
    <LbEmptyState v-else-if="error" variant="error" :title="error" @retry="load" />
    <template v-else-if="plan">
      <div class="om__sum">
        <div>
          当前方案 <b>{{ plan.scheme === 'GLOBAL' ? '全局排序(GLOBAL)' : '旧方案(LEGACY)' }}</b>
          <template v-if="plan.scheme !== 'GLOBAL'">
            · 外部代理整块排在自建节点{{ plan.external_position === 'BEFORE' ? '之前' : '之后' }}
          </template>
          · 要改写 <b class="lb-tabular">{{ plan.changed }}</b> 行
        </div>
        <div class="om__rule">
          节点按现在的先后密排成 1、2、3…;每台机器上的入口密排成 0、1、2…;外部代理整块放到它们现在所在的那一侧。
          相对顺序逐条不变 —— 原值是 0、负数、重复都不影响结果,只在右边的说明里写出来。
        </div>
        <div v-for="(e, i) in plan.errors" :key="`e${i}`" class="om__err">✕ {{ e }}</div>
        <div v-for="(w, i) in plan.warnings" :key="`w${i}`" class="om__warn">△ {{ w }}</div>
      </div>

      <label class="om__toggle">
        <a-switch v-model:checked="changedOnly" size="small" />
        只看有变化或有说明的行
      </label>

      <template v-for="group in [
        { title: `节点(${plan.nodes.length})`, rows: nodeRows },
        { title: `入口(${plan.entries.length})`, rows: entryRows },
        { title: `外部代理(${plan.externals.length})`, rows: externalRows },
      ]" :key="group.title">
        <div class="om__title">{{ group.title }}</div>
        <a-table
          :columns="columns"
          :data-source="group.rows"
          :row-key="(r: OrderPlanItem) => `${r.kind}-${r.id}`"
          size="small"
          :pagination="{ pageSize: 8, hideOnSinglePage: true, size: 'small', showSizeChanger: false }"
        >
          <template #bodyCell="{ column, record }">
            <template v-if="column.key === 'name'">
              <span class="om__kind">{{ kindLabel[(record as OrderPlanItem).kind] }}</span>
              {{ (record as OrderPlanItem).name }}
              <span v-if="(record as OrderPlanItem).node_name" class="om__dim">@ {{ (record as OrderPlanItem).node_name }}</span>
            </template>
            <template v-else-if="column.key === 'old'">
              <span class="lb-tabular">{{ (record as OrderPlanItem).old_sort }}</span>
            </template>
            <template v-else-if="column.key === 'new'">
              <b class="lb-tabular" :class="{ om__changed: (record as OrderPlanItem).old_sort !== (record as OrderPlanItem).new_sort }">
                {{ (record as OrderPlanItem).new_sort }}
              </b>
            </template>
            <template v-else-if="column.key === 'global'">
              <span class="lb-tabular">{{ (record as OrderPlanItem).global }}</span>
            </template>
            <template v-else-if="column.key === 'note'">
              <span class="om__dim">{{ (record as OrderPlanItem).note || '' }}</span>
            </template>
          </template>
        </a-table>
      </template>

      <div class="om__foot">
        <a-button @click="emit('update:open', false)">关闭</a-button>
        <a-button type="primary" :disabled="blocked" :loading="applying" @click="apply">
          {{ alreadyGlobal ? '重新密排' : '执行迁移并切换到全局排序' }}
        </a-button>
      </div>
    </template>
  </a-modal>
</template>

<style scoped>
.om__sum {
  margin-bottom: 12px;
  font-size: 13px;
  line-height: 1.7;
  color: var(--text2);
}
.om__rule {
  font-size: 12.5px;
  color: var(--text3);
}
.om__err {
  margin-top: 6px;
  color: var(--bad);
}
.om__warn {
  margin-top: 6px;
  color: var(--warn);
}
.om__toggle {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
  font-size: 12.5px;
  color: var(--text2);
}
.om__title {
  margin: 10px 0 6px;
  font-size: 13.5px;
  font-weight: 600;
}
.om__kind {
  display: inline-block;
  margin-right: 6px;
  padding: 0 6px;
  border-radius: 6px;
  background: var(--fill);
  font-size: 11.5px;
  color: var(--text3);
}
.om__dim {
  font-size: 12px;
  color: var(--text3);
}
.om__changed {
  color: var(--brand);
}
.om__foot {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  margin-top: 14px;
}
</style>
