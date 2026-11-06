<script setup lang="ts">
import { ref } from 'vue'
import { api, ApiError, type CapabilityReport } from '@/api/client'
import { LbEmptyState } from '@/components/lb'

/**
 * 能力矩阵与版本(VNext §14.1 / PROTO03)。「可以导入」「可以当出站」「可以自建」「可按用户计量」
 * 「某客户端能导出」分开列。每一格由后端拿代表性线路走真正下发的那一套判据现算,前端只渲染。
 * ✓ = 支持且经过真实客户端 / 真机验证;○ = 写得出来但没有人工验证过(实验性);✕ = 不支持,悬停看原因。
 */
const data = ref<CapabilityReport | null>(null)
const error = ref('')
const loading = ref(false)

async function load() {
  loading.value = true
  error.value = ''
  try {
    data.value = await api.capabilities()
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : '读不到能力矩阵'
  } finally {
    loading.value = false
  }
}

function mark(c: { ok: boolean; verified: boolean } | undefined) {
  if (!c) return '—'
  if (!c.ok) return '✕'
  return c.verified ? '✓' : '○'
}
</script>

<template>
  <div class="cm">
    <div v-if="!data && !loading && !error">
      <a-button size="small" @click="load">读取能力矩阵</a-button>
    </div>
    <a-skeleton v-else-if="loading" active :paragraph="{ rows: 4 }" />
    <LbEmptyState v-else-if="error" variant="error" title="能力矩阵读不到" :description="error" @retry="load" />
    <template v-else-if="data">
      <div class="cm__legend">✓ 支持并经过真实验证 · ○ 写得出来但没有人工验证(实验性) · ✕ 不支持(悬停看原因)</div>
      <div class="cm__scroll">
        <table class="cm__table">
          <thead>
            <tr>
              <th>协议</th>
              <th v-for="c in data.columns" :key="c.key">{{ c.label }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in data.rows" :key="r.key">
              <th :title="r.comments || ''">{{ r.label }}</th>
              <td
                v-for="c in data.columns"
                :key="c.key"
                :class="{ 'cm__ok': r.columns[c.key]?.ok && r.columns[c.key]?.verified, 'cm__exp': r.columns[c.key]?.ok && !r.columns[c.key]?.verified, 'cm__no': !r.columns[c.key]?.ok }"
                :title="r.columns[c.key]?.note || ''"
              >
                {{ mark(r.columns[c.key]) }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div class="cm__versions">
        面板 {{ data.versions.panel || 'dev' }} · 探测辅助程序协议 v{{ data.versions.probe_protocol }} · Agent 上报协议 v{{ data.versions.agent_protocol }}
        <template v-if="data.manifest_loaded"> · 发布 {{ data.versions.release }}</template>
        <template v-else> · 没有发布清单(开发环境自己构建,分发前不核对)</template>
      </div>
      <ul v-if="data.components.length" class="cm__comps">
        <li v-for="c in data.components" :key="c.component + c.arch">
          {{ c.component }} {{ c.version }}({{ c.arch }}){{ c.capabilities?.length ? ` · ${c.capabilities.join(', ')}` : '' }}
        </li>
      </ul>
    </template>
  </div>
</template>

<style scoped>
.cm {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  font-size: var(--fs-xs);
}
.cm__legend,
.cm__versions {
  color: var(--text2);
}
.cm__scroll {
  overflow-x: auto;
}
.cm__table {
  border-collapse: collapse;
  min-width: 860px;
}
.cm__table th,
.cm__table td {
  border-bottom: 1px solid var(--sep2);
  padding: 4px 8px;
  text-align: center;
  white-space: nowrap;
}
.cm__table tbody th {
  text-align: left;
  font-weight: 500;
}
.cm__ok {
  color: var(--ok);
}
.cm__exp {
  color: var(--warn);
}
.cm__no {
  color: var(--text3);
}
.cm__comps {
  margin: 0;
  padding-left: 18px;
  color: var(--text2);
}
</style>
