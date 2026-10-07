<script setup lang="ts">
import { computed } from 'vue'
import type { DeployResult } from '@/api/client'
import { LbShapeIcon, LbStatusTag } from '@/components/lb'
import { color } from '@/theme/tokens'

/**
 * 一次「要去节点上做事」的操作的进度与结果。
 *
 * 装、卸、下发、重启都要连 SSH,少则两三秒、多则二十几秒。原来这些操作
 * 只有一个按钮 loading 转圈,结果落在一条三秒吐司里 —— 而这类操作的结果
 * 恰恰是最需要读的:失败时要看卡在哪一步,成功时要看它到底做了什么
 * (顺带打开了 sshd 的转发?回滚了?拨测跳过了?)。
 *
 * 三条规矩:
 *
 *   1. **跑的时候关不掉。** 遮罩点击与 ESC 一律失效 —— 这些操作要十几秒,
 *      期间随手一点就关掉了,而结果几秒后才回来、已经没有地方呈现。
 *      表现为"点了下发,等了一会儿,窗口自己没了",像是操作把页面搞崩了。
 *      右上角的 × 也不给:那会让人以为关掉窗口就能取消,而操作照跑不误。
 *   2. **跑完之后不自动关。** 自动关等于把结果藏起来。
 *   3. **失败时也要显示已经做完的步骤。** "停了服务但没删定义"与
 *      "什么都没做"要人做的事完全不同,只给一句错误的话管理员分不出。
 *
 * V18:运行中 34px 圆环 + 标题 + 说明;完成后结果条 + --surface2 步骤列表
 * (22px 圆形状态图标、名称 / 结果 / 耗时、失败详情 <pre>),每行依次入场。
 */
const props = defineProps<{
  open: boolean
  /** 弹窗标题,例如「安装 Mieru」。 */
  title: string
  /** 非空表示还在跑,内容是正在做什么。 */
  running: string
  /** 纯文本步骤(按服务的装卸用这一种)。 */
  steps?: string[]
  /** 部署结果(下发用这一种,它带每一步的状态与详情)。 */
  deploy?: DeployResult | null
  /** 失败原因。与 steps/deploy 同时出现是正常的 —— 做到一半失败。 */
  error?: string
  /** 成功后的一句补充说明,例如「接下来执行下发」。 */
  note?: string
}>()

const emit = defineEmits<{ 'update:open': [boolean] }>()

const done = computed(() => !props.running)
const hasResult = computed(
  () => !!props.error || !!props.note || !!props.deploy || (props.steps?.length ?? 0) > 0,
)

const stepMeta: Record<string, { shape: 'check' | 'cross' | 'minus'; fg: string; bg: string; text: string }> = {
  SUCCESS: { shape: 'check', fg: color.success, bg: color.successBg, text: '成功' },
  FAILED: { shape: 'cross', fg: color.danger, bg: color.dangerBg, text: '失败' },
  // SKIPPED 那一档要单独看得出来 —— 它既不是成功也不是失败。
  SKIPPED: { shape: 'minus', fg: color.neutral, bg: color.neutralBg, text: '跳过' },
}

/** 结果条:失败 / 回滚一律红底,其余绿底。 */
const verdictOK = computed(() => !props.error && props.deploy?.status !== 'FAILED' && props.deploy?.status !== 'ROLLED_BACK')
</script>

<template>
  <a-modal
    :open="open"
    :title="title"
    width="600px"
    :mask-closable="false"
    :keyboard="false"
    :closable="done"
    :footer="null"
    :z-index="1100"
    @update:open="(v: boolean) => done && emit('update:open', v)"
  >
    <div class="nop">
      <div v-if="running" class="nop__running">
        <span class="nop__spinner" aria-hidden="true" />
        <div>
          <div class="nop__running-title">{{ running }}…</div>
          <div class="nop__hint">
            正在这台机器上执行,请不要关闭 —— 一次已经开始的操作不会因为关掉窗口而停下。
          </div>
        </div>
      </div>

      <!-- 完成后的结果条:一眼看出成败,细节在下面。 -->
      <div v-if="done && hasResult" class="nop__verdict" :class="verdictOK ? 'nop__verdict--ok' : 'nop__verdict--bad'">
        <LbStatusTag
          v-if="deploy"
          kind="deploy"
          :status="deploy.status"
        />
        <LbStatusTag
          v-else
          :meta="verdictOK
            ? { text: '完成', shape: 'check', fg: 'var(--ok)', bg: 'var(--ok-bg)' }
            : { text: '失败', shape: 'cross', fg: 'var(--bad)', bg: 'var(--bad-bg)' }"
        />
        <span class="nop__verdict-text">
          <template v-if="error">{{ error }}</template>
          <template v-else-if="deploy?.unchanged">配置已一致,这次没有重启服务</template>
          <template v-else-if="deploy?.rollback_result">回滚:{{ deploy.rollback_result }}</template>
          <template v-else-if="note">{{ note }}</template>
          <template v-else>已完成</template>
        </span>
      </div>

      <!-- 按服务的装卸:纯文本步骤 -->
      <ul v-if="steps?.length" class="nop__list">
        <li
          v-for="(s, i) in steps"
          :key="i"
          class="nop__row"
          :style="{ animationDelay: `${i * 60}ms` }"
        >
          <span class="nop__dot" :class="{ 'nop__dot--pulse': running }" />
          <span class="nop__row-name">{{ s }}</span>
        </li>
      </ul>

      <!-- 下发:带状态的步骤表 -->
      <ul v-if="deploy" class="nop__list">
        <li
          v-for="(s, i) in deploy.steps"
          :key="i"
          class="nop__row nop__row--deploy"
          :style="{ animationDelay: `${i * 60}ms` }"
        >
          <span class="nop__icon" :style="{ background: stepMeta[s.status]?.bg ?? color.neutralBg }">
            <LbShapeIcon
              :shape="stepMeta[s.status]?.shape ?? 'ring'"
              :color="stepMeta[s.status]?.fg ?? color.neutral"
              :size="9"
            />
          </span>
          <div class="nop__row-body">
            <div class="nop__row-head">
              <span class="nop__row-name">{{ s.name }}</span>
              <span class="nop__row-status" :style="{ color: stepMeta[s.status]?.fg ?? color.neutral }">
                {{ stepMeta[s.status]?.text ?? s.status }}
              </span>
              <span v-if="s.duration_ms > 0" class="nop__row-time lb-tabular">
                {{ (s.duration_ms / 1000).toFixed(1) }}s
              </span>
            </div>
            <pre v-if="s.detail" class="nop__detail lb-mono">{{ s.detail }}</pre>
          </div>
        </li>
      </ul>

      <!-- 回滚结果回答的是「节点现在还能不能用」,那与「这次下发失败了」
           是两个问题 —— 失败时管理员最先要知道的正是前者。结果条里已经说了,
           这里只在还有备注时再补一句。 -->
      <p v-if="note && !error && deploy" class="nop__note">{{ note }}</p>
      <div v-if="error && deploy" class="nop__error">{{ error }}</div>

      <div v-if="done && hasResult" class="nop__foot">
        <a-button type="primary" @click="emit('update:open', false)">完成</a-button>
      </div>
    </div>
  </a-modal>
</template>

<style scoped>
.nop {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.nop__running {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 8px 0 4px;
}
.nop__spinner {
  flex: none;
  width: 34px;
  height: 34px;
  border-radius: 50%;
  border: 3px solid var(--fill2);
  border-top-color: var(--brand);
  animation: lb-spin 0.9s linear infinite;
}
.nop__running-title {
  font-size: 15px;
  font-weight: 600;
}
.nop__hint {
  margin-top: 3px;
  font-size: 13px;
  line-height: 1.55;
  color: var(--text2);
}

.nop__verdict {
  display: flex;
  align-items: center;
  gap: 14px;
  padding: 14px 16px;
  border-radius: var(--r-group);
  font-size: 13.5px;
}
.nop__verdict--ok {
  background: var(--ok-bg);
}
.nop__verdict--bad {
  background: var(--bad-bg);
}
.nop__verdict-text {
  min-width: 0;
  white-space: pre-wrap;
  word-break: break-word;
}

.nop__list {
  margin: 0;
  padding: 0;
  list-style: none;
  background: var(--surface2);
  border-radius: var(--r-group);
  overflow: hidden;
}
.nop__row {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 14px;
  font-size: 13.5px;
  color: var(--text2);
  animation: lb-step 0.35s var(--ease) both;
}
.nop__row + .nop__row {
  border-top: 1px solid var(--sep);
}
.nop__row--deploy {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr);
  align-items: start;
  padding: 12px 16px;
}
.nop__dot {
  flex: none;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--fill2);
}
.nop__dot--pulse {
  animation: lb-pulse 1.4s ease-in-out infinite;
}
.nop__icon {
  width: 22px;
  height: 22px;
  border-radius: 50%;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  margin-top: 1px;
}
.nop__row-body {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.nop__row-head {
  display: flex;
  align-items: baseline;
  gap: 10px;
}
.nop__row-name {
  font-size: 13.5px;
  font-weight: 500;
  color: var(--text);
}
.nop__row-status {
  font-size: 12px;
  font-weight: 500;
}
.nop__row-time {
  margin-left: auto;
  font-size: 12px;
  color: var(--text3);
}
.nop__detail {
  margin: 0;
  padding: 10px 12px;
  background: var(--surface);
  border-radius: var(--r-input);
  font-size: 12px;
  line-height: 1.65;
  color: var(--text2);
  white-space: pre-wrap;
  word-break: break-word;
}

.nop__error {
  padding: 12px 14px;
  border-radius: var(--r-group);
  background: var(--bad-bg);
  font-size: 13px;
  line-height: 1.7;
  white-space: pre-wrap;
  word-break: break-word;
}
.nop__note {
  margin: 0;
  font-size: 12.5px;
  line-height: 1.7;
  color: var(--text3);
}
.nop__foot {
  display: flex;
  justify-content: flex-end;
}
</style>
