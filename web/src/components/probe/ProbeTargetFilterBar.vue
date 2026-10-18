<script setup lang="ts">
import { LbIcon } from '@/components/lb'
import type { TargetFilter } from './targetFilter'

/**
 * 公共目标的筛选工具条。筛选状态由调用方的 useTargetFilter 持有,这里只渲染控件。
 * 下拉的可选项来自目标库里真实出现过的值:没有的选项不列,省得筛出一个必然为空的结果。
 */
type Opt = { value: string; label: string }
const props = defineProps<{
  filter: TargetFilter
  options: { source: Opt[]; region: Opt[]; province: Opt[]; city: Opt[]; isp: Opt[]; metric: Opt[]; family: Opt[] }
  shown: number
  total: number
  active: boolean
  /** 节点选择器里不需要「状态」那一项(那里另有本机的开关状态)。 */
  hideStatus?: boolean
}>()
const emit = defineEmits<{ (e: 'reset'): void }>()

const statusOptions = [
  { value: 'active', label: '有节点在测' },
  { value: 'idle', label: '未应用到节点' },
  { value: 'paused', label: '暂停 / 未授权' },
  { value: 'missing', label: '上游已消失' },
]

function onProvince(v: string) {
  // 换了省份,原来选的城市多半不在新省份里:清掉,免得筛出一个必然为空的结果。
  props.filter.province = v
  props.filter.city = ''
}
</script>

<template>
  <div class="lb-filter ptf">
    <a-input v-model:value="filter.q" allow-clear placeholder="搜索名称 / 主机 / 地区" aria-label="搜索目标">
      <template #prefix><LbIcon name="search" :size="14" /></template>
    </a-input>
    <a-select v-model:value="filter.source" class="ptf__sel" aria-label="来源" :dropdown-match-select-width="false">
      <a-select-option value="">全部来源</a-select-option>
      <a-select-option v-for="o in options.source" :key="o.value" :value="o.value">{{ o.label }}</a-select-option>
    </a-select>
    <a-select v-if="options.region.length" v-model:value="filter.region" class="ptf__sel" aria-label="地区" :dropdown-match-select-width="false">
      <a-select-option value="">全部地区</a-select-option>
      <a-select-option v-for="o in options.region" :key="o.value" :value="o.value">{{ o.label }}</a-select-option>
    </a-select>
    <a-select
      v-if="options.province.length"
      :value="filter.province"
      class="ptf__sel"
      aria-label="省份"
      show-search
      :dropdown-match-select-width="false"
      @update:value="(v: unknown) => onProvince(String(v ?? ''))"
    >
      <a-select-option value="">全部省份</a-select-option>
      <a-select-option v-for="o in options.province" :key="o.value" :value="o.value">{{ o.label }}</a-select-option>
    </a-select>
    <a-select v-if="options.city.length" v-model:value="filter.city" class="ptf__sel" aria-label="城市" show-search :dropdown-match-select-width="false">
      <a-select-option value="">全部城市</a-select-option>
      <a-select-option v-for="o in options.city" :key="o.value" :value="o.value">{{ o.label }}</a-select-option>
    </a-select>
    <a-select v-if="options.isp.length" v-model:value="filter.isp" class="ptf__sel" aria-label="运营商" :dropdown-match-select-width="false">
      <a-select-option value="">全部运营商</a-select-option>
      <a-select-option v-for="o in options.isp" :key="o.value" :value="o.value">{{ o.label }}</a-select-option>
    </a-select>
    <a-select v-model:value="filter.metric" class="ptf__sel" aria-label="协议方式" :dropdown-match-select-width="false">
      <a-select-option value="">全部方式</a-select-option>
      <a-select-option v-for="o in options.metric" :key="o.value" :value="o.value">{{ o.label }}</a-select-option>
    </a-select>
    <a-select v-if="options.family.length > 1" v-model:value="filter.family" class="ptf__sel" aria-label="地址族" :dropdown-match-select-width="false">
      <a-select-option value="">IPv4 / IPv6</a-select-option>
      <a-select-option v-for="o in options.family" :key="o.value" :value="o.value">{{ o.label }}</a-select-option>
    </a-select>
    <a-select v-if="!hideStatus" v-model:value="filter.status" class="ptf__sel" aria-label="状态" :dropdown-match-select-width="false">
      <a-select-option value="">全部状态</a-select-option>
      <a-select-option v-for="o in statusOptions" :key="o.value" :value="o.value">{{ o.label }}</a-select-option>
    </a-select>
    <a-button v-if="active" size="small" type="link" @click="emit('reset')">清除筛选</a-button>
    <span class="ptf__count lb-tabular">{{ shown }} / {{ total }}</span>
  </div>
</template>

<style scoped>
.ptf__sel {
  min-width: 108px;
}
.ptf__count {
  margin-left: auto;
  font-size: var(--fs-sm);
  color: var(--text3);
}
</style>
