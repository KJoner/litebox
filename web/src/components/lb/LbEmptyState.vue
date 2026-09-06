<script setup lang="ts">
/**
 * 四态分开。AntD Empty 的默认插画不区分「还没有数据」「筛选后为空」
 * 「接口 500」,前两种要引导,第三种要重试。
 *
 * 尤其 error:表格保持空白,不显示「暂无数据」—— 那会被读成「一条都没有」。
 * V18:卡片内 48px 内距居中,操作按钮用幽灵胶囊,错误态图标底块 --bad-bg。
 */
withDefaults(
  defineProps<{
    variant: 'empty' | 'filtered' | 'error'
    title: string
    description?: string
    /**
     * 错误态的补充事实。只放确实能帮到管理员的东西:
     * HTTP 状态码(能取到时)与发生时间。
     *
     * 没有请求 ID —— 后端没有请求追踪中间件,ApiError 上只有 status 与 message。
     * 编一个 ID 出来、或者说「凭此 ID 可查审计日志」都是假承诺。
     */
    httpStatus?: number
    occurredAt?: string
  }>(),
  {},
)

defineEmits<{ (e: 'retry'): void; (e: 'clear'): void }>()
</script>

<template>
  <div class="lb-empty">
    <div v-if="variant === 'empty'" class="lb-empty__tile lb-empty__tile--muted" aria-hidden="true">
      <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">
        <path d="M21 8V6a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v2" /><rect x="3" y="8" width="18" height="12" rx="2" /><path d="M9 14h6" />
      </svg>
    </div>
    <div v-else-if="variant === 'error'" class="lb-empty__tile lb-empty__tile--bad" aria-hidden="true">
      <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round">
        <path d="M18 6 6 18" /><path d="m6 6 12 12" />
      </svg>
    </div>
    <div v-else class="lb-empty__tile lb-empty__tile--muted" aria-hidden="true">
      <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">
        <circle cx="11" cy="11" r="8" /><path d="m21 21-4.3-4.3" />
      </svg>
    </div>

    <div class="lb-empty__title">{{ title }}</div>
    <div v-if="description" class="lb-empty__desc">{{ description }}</div>

    <div class="lb-empty__actions">
      <!-- 首次为空引导创建;筛选为空给清除;错误给重试。三者互不混用。 -->
      <slot v-if="variant === 'empty'" name="action" />
      <a-button v-else-if="variant === 'filtered'" size="small" class="lb-btn-ghost" @click="$emit('clear')">
        清除全部筛选
      </a-button>
      <template v-else>
        <a-button size="small" class="lb-btn-ghost" @click="$emit('retry')">重试</a-button>
        <slot name="action" />
      </template>
    </div>

    <div v-if="httpStatus || occurredAt" class="lb-empty__meta lb-mono">
      <span v-if="httpStatus">HTTP {{ httpStatus }}</span>
      <span v-if="httpStatus && occurredAt"> · </span>
      <span v-if="occurredAt">{{ occurredAt }}</span>
    </div>
  </div>
</template>

<style scoped>
.lb-empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  padding: 48px 20px;
  text-align: center;
  color: var(--text3);
}

.lb-empty__tile {
  width: 38px;
  height: 38px;
  border-radius: var(--r-tile);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  margin-bottom: 2px;
}
.lb-empty__tile--muted {
  background: var(--fill);
  color: var(--text3);
}
.lb-empty__tile--bad {
  background: var(--bad-bg);
  color: var(--bad);
}

.lb-empty__title {
  font-size: 13.5px;
  font-weight: 600;
  color: var(--text);
}

.lb-empty__desc {
  max-width: 320px;
  font-size: 13px;
  line-height: 1.65;
  color: var(--text3);
  text-wrap: pretty;
}

.lb-empty__actions {
  display: flex;
  gap: 8px;
  margin-top: 4px;
}

.lb-empty__meta {
  font-size: 11px;
  color: var(--text3);
}
</style>
