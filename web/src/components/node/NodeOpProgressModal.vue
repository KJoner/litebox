<script setup lang="ts">
import { computed } from 'vue'
import type { DeployResult } from '@/api/client'

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

/** 部署步骤里 SKIPPED 那一档要单独看得出来 —— 它既不是成功也不是失败。 */
function stepClass(status: string) {
  if (status === 'FAILED') return 'nop__step--fail'
  if (status === 'SKIPPED') return 'nop__step--skip'
  return ''
}
</script>

<template>
  <a-modal
    :open="open"
    :title="title"
    width="620px"
    :mask-closable="false"
    :keyboard="false"
    :closable="done"
    :footer="null"
    @update:open="(v: boolean) => done && emit('update:open', v)"
  >
    <div v-if="running" class="nop__running">
      <a-spin size="small" />
      <span>{{ running }}…</span>
    </div>
    <p v-if="running" class="nop__hint">
      正在这台机器上执行,请不要关闭 —— 一次已经开始的操作不会因为关掉窗口而停下。
    </p>

    <div v-if="error" class="nop__error">{{ error }}</div>

    <!-- 按服务的装卸:纯文本步骤 -->
    <ul v-if="steps?.length" class="nop__steps">
      <li v-for="(s, i) in steps" :key="i">{{ s }}</li>
    </ul>

    <!-- 下发:带状态的步骤表 -->
    <template v-if="deploy">
      <div class="nop__status">
        结果:<b>{{ deploy.status }}</b>
      </div>
      <div v-for="(s, i) in deploy.steps" :key="i" class="nop__step" :class="stepClass(s.status)">
        <span class="nop__step-name">{{ s.name }}</span>
        <span class="nop__step-status lb-mono">{{ s.status }}</span>
        <span class="nop__step-detail">{{ s.detail }}</span>
      </div>
      <!-- 回滚结果回答的是「节点现在还能不能用」,那与「这次下发失败了」
           是两个问题 —— 失败时管理员最先要知道的正是前者。 -->
      <div v-if="deploy.rollback_result" class="nop__rollback">
        回滚:{{ deploy.rollback_result }}
      </div>
    </template>

    <p v-if="note && !error" class="nop__note">{{ note }}</p>

    <div v-if="done && hasResult" class="nop__foot">
      <a-button size="small" type="primary" @click="emit('update:open', false)">关闭</a-button>
    </div>
  </a-modal>
</template>

<style scoped>
/* 颜色只用 tokens.ts 里已有的:text3 / danger / warning / border。 */
.nop__running {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
}
.nop__hint {
  margin: 8px 0 0;
  font-size: 12px;
  line-height: 1.7;
  color: #6b7480;
}
.nop__error {
  margin-bottom: 10px;
  padding: 8px 10px;
  border: 1px solid #f3cfc9;
  border-radius: 6px;
  background: #fdecea;
  font-size: 12px;
  line-height: 1.7;
  color: #8e2117;
  white-space: pre-wrap;
  word-break: break-word;
}
.nop__steps {
  margin: 0;
  padding-left: 18px;
  font-size: 12px;
  line-height: 1.9;
  color: #576070;
}
.nop__status {
  margin-bottom: 6px;
  font-size: 12px;
  color: #576070;
}
.nop__step {
  display: grid;
  grid-template-columns: 150px 84px 1fr;
  gap: 8px;
  padding: 4px 0;
  font-size: 12px;
  line-height: 1.6;
  border-top: 1px solid #edeff2;
  color: #576070;
}
.nop__step--fail {
  color: #8e2117;
}
.nop__step--skip {
  color: #5c4405;
}
.nop__step-detail {
  word-break: break-word;
  white-space: pre-wrap;
}
.nop__rollback {
  margin-top: 8px;
  font-size: 12px;
  line-height: 1.7;
  color: #5c4405;
}
.nop__note {
  margin: 10px 0 0;
  font-size: 12px;
  line-height: 1.7;
  color: #6b7480;
}
.nop__foot {
  margin-top: 14px;
  text-align: right;
}
</style>
