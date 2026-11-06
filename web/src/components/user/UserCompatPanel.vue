<script setup lang="ts">
import { ref, watch } from 'vue'
import { api, ApiError, type CompatClient, type CompatReport } from '@/api/client'

/**
 * 客户端兼容预览(VNext CLIENT02):这个用户在某个客户端里能拿到哪几条、少了哪几条、为什么。
 * 与真正下发走同一份条目(权限过滤已做完),格式不支持的线路不会悄悄变成直连,也不会无声消失。
 * Surge / Loon / Quantumult X / Stash 是实验性的:写得出来,但没有在真实客户端上人工验证过。
 */
const props = defineProps<{ userId: number }>()
const client = ref<CompatClient>('clash')
const report = ref<CompatReport | null>(null)
const error = ref('')
const loading = ref(false)

const clients: { value: CompatClient; label: string }[] = [
  { value: 'clash', label: 'Clash / mihomo' },
  { value: 'sing-box', label: 'sing-box' },
  { value: 'stash', label: 'Stash(实验性)' },
  { value: 'surge', label: 'Surge(实验性)' },
  { value: 'loon', label: 'Loon(实验性)' },
  { value: 'quanx', label: 'Quantumult X(实验性)' },
]

async function load() {
  loading.value = true
  error.value = ''
  try {
    report.value = await api.userCompat(props.userId, client.value)
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : '读不到兼容预览'
  } finally {
    loading.value = false
  }
}
watch([client, () => props.userId], load, { immediate: true })
</script>

<template>
  <div class="uc">
    <a-select v-model:value="client" :options="clients" size="small" class="uc__select" />
    <div v-if="error" class="uc__bad">{{ error }}</div>
    <a-skeleton v-else-if="loading && !report" active :paragraph="{ rows: 3 }" />
    <template v-else-if="report">
      <div v-if="!report.verified" class="uc__warn">
        {{ report.label }} 的输出是实验性的:写得出来,但没有在真实客户端上人工验证过。要给用户用之前先自己导入试一次。
      </div>
      <div class="uc__head">能导出 {{ report.exported.length }} 条</div>
      <div class="uc__list">
        <span v-for="e in report.exported" :key="e.name" class="uc__chip">{{ e.name }}</span>
        <span v-if="!report.exported.length" class="uc__bad">一条都导不出来 —— 这个客户端的订阅会直接报错,不会给一份空配置。</span>
      </div>
      <template v-if="report.skipped.length">
        <div class="uc__head">跳过 {{ report.skipped.length }} 条</div>
        <ul class="uc__skipped">
          <li v-for="e in report.skipped" :key="e.name"><b>{{ e.name }}</b>:{{ e.reason }}</li>
        </ul>
      </template>
    </template>
  </div>
</template>

<style scoped>
.uc {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  font-size: var(--fs-xs);
}
.uc__select {
  width: 220px;
}
.uc__head {
  font-weight: 600;
  color: var(--text2);
}
.uc__list {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.uc__chip {
  padding: 0 6px;
  border-radius: var(--r-xs);
  background: var(--surface2);
}
.uc__skipped {
  margin: 0;
  padding-left: 18px;
  color: var(--text2);
}
.uc__warn {
  color: var(--warn);
}
.uc__bad {
  color: var(--bad);
}
</style>
