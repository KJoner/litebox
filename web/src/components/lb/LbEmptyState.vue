<script setup lang="ts">
/**
 * 四态分开。AntD Empty 的默认插画不区分「还没有数据」「筛选后为空」
 * 「接口 500」,前两种要引导,第三种要重试。
 *
 * 尤其 error:表格保持空白,不显示「暂无数据」—— 那会被读成「一条都没有」。
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
    <div v-if="variant === 'empty'" class="lb-empty__glyph" aria-hidden="true" />
    <svg v-else-if="variant === 'error'" width="16" height="16" viewBox="0 0 9 9" aria-hidden="true">
      <path d="M1.2 1.2 7.8 7.8M7.8 1.2 1.2 7.8" stroke="#B4291D" stroke-width="1.6" stroke-linecap="round" />
    </svg>

    <div class="lb-empty__title">{{ title }}</div>
    <div v-if="description" class="lb-empty__desc">{{ description }}</div>

    <div class="lb-empty__actions">
      <!-- 首次为空引导创建;筛选为空给清除;错误给重试。三者互不混用。 -->
      <slot v-if="variant === 'empty'" name="action" />
      <a-button v-else-if="variant === 'filtered'" size="small" @click="$emit('clear')">
        清除全部筛选
      </a-button>
      <template v-else>
        <a-button size="small" @click="$emit('retry')">重试</a-button>
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
  gap: 9px;
  padding: 30px 20px;
  text-align: center;
}

.lb-empty__glyph {
  width: 32px;
  height: 32px;
  border: 1px dashed #c6ccd3;
  border-radius: 6px;
}

.lb-empty__title {
  font-size: 13px;
  font-weight: 600;
}

.lb-empty__desc {
  max-width: 300px;
  font-size: 12.5px;
  line-height: 1.65;
  color: #6b7480;
  text-wrap: pretty;
}

.lb-empty__actions {
  display: flex;
  gap: 8px;
  margin-top: 2px;
}

.lb-empty__meta {
  font-size: 11px;
  color: #6b7480;
}
</style>
