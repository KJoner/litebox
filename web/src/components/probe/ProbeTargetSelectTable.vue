<script setup lang="ts">
import { computed, watch } from 'vue'
import type { NodePublicProbe, PublicProbeTarget } from '@/api/client'
import { familyText, ispText, metricLabel, originText } from '@/components/asset/probeView'
import ProbeTargetFilterBar from './ProbeTargetFilterBar.vue'
import { useTargetFilter } from './targetFilter'

/**
 * 可勾选的目标列表(带筛选):节点详情的「选择公共目标」与节点列表的「应用探测目标」共用。
 * 传进来的是 NodePublicProbe 时多一列「本机」,写出这台机器上它现在的状态。
 */
const props = defineProps<{
  items: (PublicProbeTarget | NodePublicProbe)[]
  modelValue: number[]
  /** 显示「本机」那一列(节点详情用)。 */
  nodeState?: boolean
}>()
const emit = defineEmits<{ (e: 'update:modelValue', v: number[]): void }>()

const list = computed(() => props.items as PublicProbeTarget[])
const { filter, filtered, options, active, reset } = useTargetFilter(list)
watch(filter, () => emit('update:modelValue', []), { deep: true })

const allShownPicked = computed(() => filtered.value.length > 0 && filtered.value.every((p) => props.modelValue.includes(p.id)))

function toggle(id: number, on: boolean) {
  emit('update:modelValue', on ? [...new Set([...props.modelValue, id])] : props.modelValue.filter((x) => x !== id))
}
function toggleAll(on: boolean) {
  const ids = filtered.value.map((p) => p.id)
  emit('update:modelValue', on ? [...new Set([...props.modelValue, ...ids])] : props.modelValue.filter((x) => !ids.includes(x)))
}

function nodeText(p: PublicProbeTarget | NodePublicProbe): string {
  const np = p as NodePublicProbe
  if (np.mode === undefined) return ''
  if (np.node_pause_reason) return np.node_pause_reason
  if (np.effective) return np.mode === 'ON' ? '本机启用' : '跟随全局:在测'
  if (np.mode === 'OFF') return '本机停用'
  return '不测'
}
</script>

<template>
  <div class="pst">
    <ProbeTargetFilterBar
      :filter="filter"
      :options="options"
      :shown="filtered.length"
      :total="items.length"
      :active="active"
      hide-status
      @reset="reset"
    />
    <div class="pst__table">
      <table>
        <thead>
          <tr>
            <th class="pst__check">
              <a-checkbox :checked="allShownPicked" aria-label="勾选显示的全部目标" @update:checked="toggleAll" />
            </th>
            <th>目标</th>
            <th>方式</th>
            <th>地区 / 运营商</th>
            <th>来源</th>
            <th v-if="nodeState">本机</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="p in filtered" :key="p.id" :data-testid="`pick-${p.id}`">
            <td class="pst__check">
              <a-checkbox :checked="modelValue.includes(p.id)" @update:checked="(v: boolean) => toggle(p.id, v)" />
            </td>
            <td>
              <div class="pst__name">
                {{ p.name }}
                <span v-if="p.role === 'BACKUP'" class="lb-chip">备用</span>
              </div>
              <div class="lb-mono pst__muted">{{ p.host }}<template v-if="p.protocol === 'TCP'">:{{ p.port }}</template></div>
            </td>
            <td>{{ metricLabel(p.metric) }} · {{ familyText(p.family) }}</td>
            <td>{{ [p.province, p.city, ispText(p.isp)].filter(Boolean).join(' ') || p.region || '—' }}</td>
            <td>
              {{ originText(p.source) }}
              <span v-if="p.third_party && p.pause_reason.includes('授权')" class="pst__warn">· 未授权</span>
            </td>
            <td v-if="nodeState" :class="{ pst__warn: !!(p as NodePublicProbe).node_pause_reason }">{{ nodeText(p) }}</td>
          </tr>
          <tr v-if="!filtered.length">
            <td :colspan="nodeState ? 6 : 5" class="pst__muted pst__empty">没有符合筛选的目标</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<style scoped>
.pst {
  display: flex;
  flex-direction: column;
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
  overflow: hidden;
}
.pst__table {
  max-height: 360px;
  overflow: auto;
}
.pst__table table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--fs-xs);
}
.pst__table th,
.pst__table td {
  padding: 6px 8px;
  border-bottom: 1px solid var(--sep2);
  text-align: left;
  vertical-align: top;
}
.pst__table th {
  position: sticky;
  top: 0;
  z-index: 1;
  background: var(--surface2);
  font-weight: 600;
  color: var(--text2);
}
.pst__check {
  width: 32px;
}
.pst__name {
  font-weight: 600;
  color: var(--text);
}
.pst__muted {
  color: var(--text3);
}
.pst__warn {
  color: var(--warn);
}
.pst__empty {
  text-align: center;
  padding: var(--sp-4);
}
</style>
