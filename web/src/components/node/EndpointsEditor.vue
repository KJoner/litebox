<script setup lang="ts">
import { computed } from 'vue'
import type { InboundEndpointInput, NodeAddress } from '@/api/client'

/**
 * 入口订阅地址条目编辑器(V16)。一个入口可以在订阅里下发多条地址:管理 IP、
 * 若干额外 IPv4、若干 IPv6,每条各自的公网端口与订阅显示名互不相干。
 *
 * 一条都不加时,订阅按「管理 IP + 跟随端口 + 跟随名」下发一条 —— 所以父组件
 * 在列表为空时给一条默认的 host 行,让管理员看得见当前会下发什么。
 *
 * 端口留空(0)= 跟随入口的监听端口;名字留空 = 跟随入口名(V6 条目加 -IPV6)。
 * 面板一次都不解析这些地址,填错发现不了、只有用户连不上。
 */
const props = defineProps<{
  modelValue: InboundEndpointInput[]
  addresses: NodeAddress[]
  host: string
  isMieru: boolean
  nameHint: string
}>()
const emit = defineEmits<{ 'update:modelValue': [InboundEndpointInput[]] }>()

const options = computed(() => [
  { value: null as number | null, label: `管理 IP · ${props.host || '(管理地址)'}` },
  ...props.addresses.map((a) => ({ value: a.id, label: `${a.family === 'V6' ? 'IPv6' : 'IPv4'} · ${a.address}` })),
])

function familyOf(addressID: number | null): 'V4' | 'V6' {
  if (addressID == null) return 'V4'
  return props.addresses.find((a) => a.id === addressID)?.family ?? 'V4'
}
function namePlaceholder(addressID: number | null): string {
  const base = props.nameHint || '入口名'
  return familyOf(addressID) === 'V6' ? `${base}-IPV6` : base
}

function patch(i: number, part: Partial<InboundEndpointInput>) {
  const next = props.modelValue.slice()
  next[i] = { ...next[i], ...part }
  emit('update:modelValue', next)
}
function removeAt(i: number) {
  const next = props.modelValue.slice()
  next.splice(i, 1)
  emit('update:modelValue', next)
}
function add() {
  emit('update:modelValue', [
    ...props.modelValue,
    { address_id: null, public_port: 0, public_port_end: 0, display_name: '' },
  ])
}
</script>

<template>
  <div class="ee">
    <div v-if="!modelValue.length" class="ee__empty">
      还没有地址条目 —— 订阅会按「管理 IP + 跟随端口 + 跟随名」下发一条。点下面「添加地址」可以细分。
    </div>

    <div v-for="(row, i) in modelValue" :key="i" class="ee__row">
      <a-select
        :value="row.address_id"
        :options="options"
        size="small"
        class="ee__addr"
        @update:value="(v: number | null) => patch(i, { address_id: v })"
      />
      <template v-if="isMieru">
        <a-input-number
          :value="row.public_port || null"
          :min="0"
          :max="65535"
          size="small"
          placeholder="起"
          class="ee__port"
          @update:value="(v: number | null) => patch(i, { public_port: v ?? 0 })"
        />
        <span class="ee__dash">–</span>
        <a-input-number
          :value="row.public_port_end || null"
          :min="0"
          :max="65535"
          size="small"
          placeholder="止"
          class="ee__port"
          @update:value="(v: number | null) => patch(i, { public_port_end: v ?? 0 })"
        />
      </template>
      <a-input-number
        v-else
        :value="row.public_port || null"
        :min="0"
        :max="65535"
        size="small"
        placeholder="跟随监听"
        class="ee__port"
        @update:value="(v: number | null) => patch(i, { public_port: v ?? 0 })"
      />
      <a-input
        :value="row.display_name"
        size="small"
        :placeholder="namePlaceholder(row.address_id)"
        class="ee__name"
        @update:value="(v: string) => patch(i, { display_name: v })"
      />
      <a-button size="small" type="text" class="ee__del" @click="removeAt(i)">删除</a-button>
    </div>

    <a-button size="small" class="ee__add" @click="add">添加地址</a-button>
  </div>
</template>

<style scoped>
.ee {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.ee__empty {
  font-size: 11.5px;
  line-height: 1.6;
  color: #6b7480;
  padding: 2px 0;
}
.ee__row {
  display: flex;
  gap: 6px;
  align-items: center;
}
.ee__addr {
  flex: 1.4;
  min-width: 0;
}
.ee__port {
  width: 92px;
}
.ee__dash {
  color: #8a94a6;
}
.ee__name {
  flex: 1;
  min-width: 0;
}
.ee__del {
  color: #b4291d;
  padding: 0 6px;
}
.ee__add {
  align-self: flex-start;
}
</style>
