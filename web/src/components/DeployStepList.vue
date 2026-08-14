<script setup lang="ts">
import type { DeploymentRecord } from '@/api/client'
import { formatDuration, shortHash } from '@/utils/format'
import { LbCopyField, LbShapeIcon, LbTimeText } from '@/components/lb'
import { color } from '@/theme/tokens'

/**
 * 部署步骤时间线。结构不变,只换视觉。
 *
 * 失败原因与回滚结果置顶 —— 这一点原实现就做对了,保留。
 * 失败步骤的输出用等宽 + 保留换行:那是拨测的原始输出,
 * 折行重排之后就不是它了。
 */
defineProps<{ record: DeploymentRecord }>()

const stepMeta: Record<string, { shape: 'check' | 'cross' | 'minus'; fg: string; text: string }> = {
  SUCCESS: { shape: 'check', fg: color.success, text: '成功' },
  FAILED: { shape: 'cross', fg: color.danger, text: '失败' },
  // 已跳过不标绿 —— 那会让人以为这一步做过了。
  SKIPPED: { shape: 'minus', fg: color.neutral, text: '跳过' },
}
</script>

<template>
  <div class="ds">
    <div class="ds__meta">
      <span>
        开始 <LbTimeText :value="record.started_at" mode="both" />
      </span>
      <!-- ?. 兜底:一步都没跑就失败的部署在旧版本里会把 steps 发成 null,
           而这里一旦抛错,整个部署结果弹窗就是空的。 -->
      <span>{{ record.steps?.length ?? 0 }} 步</span>
      <span class="lb-mono" :title="record.config_sha256">
        配置哈希 {{ shortHash(record.config_sha256) }}
      </span>
    </div>

    <div v-if="record.error_message" class="ds__banner ds__banner--error">
      <div class="ds__banner-title">失败原因</div>
      <div class="ds__banner-body">{{ record.error_message }}</div>
    </div>
    <div
      v-if="record.rollback_result"
      class="ds__banner"
      :class="record.rollback_result.includes('成功') ? 'ds__banner--warn' : 'ds__banner--error'"
    >
      <div class="ds__banner-title">回滚结果</div>
      <div class="ds__banner-body">{{ record.rollback_result }}</div>
    </div>

    <ol class="ds__steps">
      <li v-for="(s, i) in record.steps" :key="i" class="ds__step">
        <span class="ds__step-icon">
          <LbShapeIcon
            :shape="stepMeta[s.status]?.shape ?? 'ring'"
            :color="stepMeta[s.status]?.fg ?? color.neutral"
            :size="9"
          />
        </span>
        <div class="ds__step-body">
          <div class="ds__step-head">
            <span class="ds__step-no lb-mono">{{ i + 1 }}</span>
            <span class="ds__step-name">{{ s.name }}</span>
            <span
              class="ds__step-status"
              :style="{ color: stepMeta[s.status]?.fg ?? color.neutral }"
            >
              {{ stepMeta[s.status]?.text ?? s.status }}
            </span>
            <span v-if="s.duration_ms > 0" class="ds__step-time lb-mono">
              {{ formatDuration(s.duration_ms) }}
            </span>
          </div>
          <!-- 失败步骤的原始输出。等宽 + 保留换行,不 break-all 铺开。 -->
          <pre v-if="s.detail" class="ds__step-detail lb-mono">{{ s.detail }}</pre>
          <LbCopyField
            v-if="s.detail && s.status === 'FAILED'"
            :value="s.detail"
            button-text="复制全文"
          />
        </div>
      </li>
    </ol>
  </div>
</template>

<style scoped>
.ds {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.ds__meta {
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
  font-size: 11.5px;
  color: #6b7480;
}

.ds__banner {
  padding: 10px 12px;
  border-radius: 6px;
  border: 1px solid;
}

.ds__banner--error {
  background: #fdecea;
  border-color: #f3cfc9;
  color: #8e2117;
}

.ds__banner--warn {
  background: #fcf3e3;
  border-color: #efdcb4;
  color: #5c4405;
}

.ds__banner-title {
  font-size: 11.5px;
  font-weight: 600;
  margin-bottom: 4px;
}

.ds__banner-body {
  font-size: 12px;
  line-height: 1.7;
}

.ds__steps {
  margin: 0;
  padding: 0;
  list-style: none;
  border: 1px solid #edeff2;
  border-radius: 6px;
  overflow: hidden;
}

.ds__step {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr);
  gap: 10px;
  padding: 9px 12px;
}

.ds__step + .ds__step {
  border-top: 1px solid #edeff2;
}

.ds__step-icon {
  padding-top: 4px;
}

.ds__step-body {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.ds__step-head {
  display: flex;
  align-items: baseline;
  gap: 8px;
  flex-wrap: wrap;
}

.ds__step-no {
  font-size: 11px;
  color: #6b7480;
}

.ds__step-name {
  font-size: 12.5px;
}

.ds__step-status,
.ds__step-time {
  font-size: 11px;
}

.ds__step-time {
  color: #6b7480;
}

.ds__step-detail {
  margin: 0;
  padding: 8px 10px;
  background: #f6f7f9;
  border: 1px solid #edeff2;
  border-radius: 4px;
  font-size: 11px;
  line-height: 1.7;
  color: #576070;
  white-space: pre-wrap;
  overflow-x: auto;
}
</style>
