<script setup lang="ts">
import { ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type DestCheckResult, type NodeInbound } from '@/api/client'

/**
 * 实测并写入一个入口的 REALITY 握手目标。
 *
 * 检测必须从**节点本机**发起(CDN 按地域下发不同证书链),而写入必须
 * 指名道姓地写到某一个入口上 —— 同机两个 REALITY 入站完全可以指向不同
 * 的目标,而 8192 字节记录上限是目标域名的属性、不是机器的属性。
 * 所以这个弹窗永远绑定在一个具体入口上,没有"给这台机器设一个"这种入口。
 */
const props = defineProps<{
  open: boolean
  inbound: NodeInbound | null
}>()

const emit = defineEmits<{
  'update:open': [boolean]
  /** 实测通过并写入了,调用方据此重新拉数据。 */
  applied: []
  busy: [label: string]
}>()

const running = ref('')
const input = ref('')
const result = ref<DestCheckResult | null>(null)
const error = ref('')

watch(
  () => props.open,
  (open) => {
    if (!open) return
    input.value = props.inbound?.reality_dest ?? ''
    result.value = null
    error.value = ''
  },
  { immediate: true },
)

async function run() {
  const target = props.inbound
  if (!target) return
  running.value = '正在实测握手目标'
  emit('busy', running.value)
  result.value = null
  error.value = ''
  try {
    const r = await api.applyInboundDest(target.id, input.value.trim())
    result.value = r.result
    if (r.error) {
      error.value = r.error
    } else {
      message.success('握手目标已实测通过并写入这个入口')
      emit('applied')
    }
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : '实测失败'
  } finally {
    running.value = ''
    emit('busy', '')
  }
}
</script>

<template>
  <a-modal
    :open="open"
    title="实测握手目标"
    :confirm-loading="!!running"
    ok-text="实测并写入"
    @update:open="(v: boolean) => emit('update:open', v)"
    @ok="run"
  >
    <p class="idm__hint">
      入口 <b>{{ inbound?.display_name ?? '' }}</b>({{ inbound?.node_display_name || inbound?.node_name }})。
      检测从<b>这台机器的出口</b>发起 —— CDN 按地域下发不同证书链,在别处测出来的结果
      对这台机器不成立。通过之后才写入这个入口;不通过时拒绝保存。
    </p>
    <a-form layout="vertical" size="small">
      <a-form-item label="握手目标域名">
        <a-input v-model:value="input" placeholder="例如:www.fastly.com" />
      </a-form-item>
    </a-form>
    <div v-if="error" class="idm__warn">{{ error }}</div>
    <div v-if="result" class="idm__result">
      <div>
        {{ result.server }}:{{ result.port }} · 最大 TLS 记录 {{ result.max_record_size }} 字节
      </div>
      <div v-for="(p, idx) in result.problems" :key="idx" class="idm__warn">{{ p }}</div>
    </div>
  </a-modal>
</template>

<style scoped>
/* 颜色只允许用 tokens.ts 里已有的值:text3 / danger。 */
.idm__hint {
  margin: 0 0 12px;
  font-size: 12px;
  line-height: 1.6;
  color: #6b7480;
}
.idm__warn {
  font-size: 12px;
  line-height: 1.6;
  color: #b4291d;
}
.idm__result {
  margin-top: 8px;
  font-size: 12px;
  line-height: 1.6;
  color: #6b7480;
}
</style>
