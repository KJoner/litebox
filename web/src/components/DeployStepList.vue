<script setup lang="ts">
import type { DeploymentRecord } from '@/api/client'
import { formatDuration, formatTime, shortHash } from '@/utils/format'

defineProps<{ record: DeploymentRecord }>()

// 状态既有颜色也有文字,颜色不单独承载含义。
const stepMeta: Record<string, { color: string; text: string }> = {
  SUCCESS: { color: 'green', text: '成功' },
  FAILED: { color: 'red', text: '失败' },
  SKIPPED: { color: 'gray', text: '跳过' },
}
</script>

<template>
  <a-descriptions :column="2" size="small" class="meta">
    <a-descriptions-item label="配置哈希">
      <span class="mono" :title="record.config_sha256">{{ shortHash(record.config_sha256) }}</span>
    </a-descriptions-item>
    <a-descriptions-item label="开始时间">{{ formatTime(record.started_at) }}</a-descriptions-item>
    <a-descriptions-item label="完成时间">{{ formatTime(record.finished_at) }}</a-descriptions-item>
    <a-descriptions-item label="步骤数">{{ record.steps.length }}</a-descriptions-item>
  </a-descriptions>

  <a-alert
    v-if="record.error_message"
    type="error"
    show-icon
    class="msg"
    message="失败原因"
    :description="record.error_message"
  />
  <a-alert
    v-if="record.rollback_result"
    :type="record.rollback_result.includes('成功') ? 'warning' : 'error'"
    show-icon
    class="msg"
    message="回滚结果"
    :description="record.rollback_result"
  />

  <a-timeline class="steps">
    <a-timeline-item
      v-for="(s, i) in record.steps"
      :key="i"
      :color="stepMeta[s.status]?.color ?? 'blue'"
    >
      <div class="step-head">
        <span class="step-name">{{ s.name }}</span>
        <a-tag :color="stepMeta[s.status]?.color">{{ stepMeta[s.status]?.text ?? s.status }}</a-tag>
        <span v-if="s.duration_ms > 0" class="step-time">{{ formatDuration(s.duration_ms) }}</span>
      </div>
      <div v-if="s.detail" class="step-detail">{{ s.detail }}</div>
    </a-timeline-item>
  </a-timeline>
</template>

<style scoped>
.meta {
  margin-bottom: 8px;
}

.msg {
  margin-bottom: 12px;
}

.steps {
  margin-top: 12px;
}

.step-head {
  display: flex;
  align-items: center;
  gap: 8px;
}

.step-name {
  font-size: 13px;
}

.step-time {
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}

.step-detail {
  color: rgb(0 0 0 / 65%);
  font-size: 12px;
  margin-top: 2px;
  word-break: break-all;
  white-space: pre-wrap;
}

.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px;
}
</style>
