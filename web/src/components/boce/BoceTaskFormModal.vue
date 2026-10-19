<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type BoceEstimate,
  type BoceScheduleSpec,
  type BoceTaskInput,
  type BoceTaskView,
  type BoceVantage,
  type NodeBoce,
} from '@/api/client'
import { LbIcon, LbInfoTip } from '@/components/lb'
import BoceVantagePicker from './BoceVantagePicker.vue'
import BoceScheduleEditor from './BoceScheduleEditor.vue'
import BoceEstimatePanel from './BoceEstimatePanel.vue'
import { emptyTaskInput, specOf } from './boceText'

/**
 * 新建 / 编辑 / 复制一个 Boce 拨测任务。
 *
 * 被测目标只能从这台节点的订阅入口里选(服务端再校验一次);IPv6 入口列出来但选不了,写明原因 ——
 * Boce 的 TCPing 文档没有说明支持 IPv6,面板不会悄悄改用 IPv4 去测。新建的任务默认不开自动检测:
 * 看过下面的费用估算再打开。保存任务本身不会创建任何付费检测。
 */
const props = defineProps<{
  open: boolean
  nodeId: number
  node: NodeBoce
  vantages: BoceVantage[]
  /** 编辑的任务;copy 为真时按它预填一份新任务。 */
  task?: BoceTaskView | null
  copy?: boolean
}>()
const emit = defineEmits<{ (e: 'close'): void; (e: 'saved'): void }>()

const editing = computed(() => !!props.task && !props.copy)
const form = reactive<BoceTaskInput>(emptyTaskInput(props.node.default_schedule))
const saving = ref(false)
const error = ref('')

const spec = computed<BoceScheduleSpec>({
  get: () => specOf(form),
  set: (v) => Object.assign(form, v),
})

watch(
  () => props.open,
  (open) => {
    if (!open) return
    error.value = ''
    const base = props.task
    if (base) {
      Object.assign(form, {
        ...specOf(base),
        name: props.copy ? `${base.name}(副本)` : base.name,
        ref: base.ref,
        vantage_ids: [...base.vantage_ids],
        month_points_limit: base.month_points_limit,
        enabled: props.copy ? false : base.enabled,
        note: base.note,
      })
      if (props.copy && !props.node.candidates.some((c) => c.ref === base.ref)) form.ref = undefined
    } else {
      Object.assign(form, emptyTaskInput(props.node.default_schedule))
      form.ref = props.node.candidates.find((c) => !c.note)?.ref
    }
  },
  { immediate: true },
)

const keepIDs = computed(() => (editing.value ? (props.task?.vantage_ids ?? []) : []))

// ---------- 估算(防抖) ----------

const estimate = ref<BoceEstimate | null>(null)
const estimating = ref(false)
const estimateError = ref('')
let timer: ReturnType<typeof setTimeout> | null = null
let seq = 0
watch(
  () => [props.open, JSON.stringify(spec.value), form.vantage_ids.length] as const,
  () => {
    if (!props.open) return
    if (timer) clearTimeout(timer)
    timer = setTimeout(async () => {
      const my = ++seq
      estimating.value = true
      try {
        const e = await api.boceEstimate({
          ...spec.value,
          vantages: form.vantage_ids.length,
          targets: 1,
          exclude_task_id: editing.value ? props.task?.id : undefined,
        })
        if (my === seq) {
          estimate.value = e
          estimateError.value = ''
        }
      } catch (err) {
        if (my === seq) estimateError.value = err instanceof ApiError ? err.message : '估算失败'
      } finally {
        if (my === seq) estimating.value = false
      }
    }, 350)
  },
  { immediate: true },
)

async function save() {
  error.value = ''
  if (!editing.value && !form.ref) {
    error.value = '选一个被测入口'
    return
  }
  if (!form.vantage_ids.length) {
    error.value = '至少选一个 Boce 拨测点'
    return
  }
  saving.value = true
  try {
    const body: BoceTaskInput = { ...form, times: [...form.times] }
    if (editing.value && props.task) {
      delete body.enabled
      delete body.ref
      await api.updateBoceTask(props.task.id, body)
      message.success('已保存(修改任务不会删除已有的检测历史)')
    } else {
      await api.createBoceTask(props.nodeId, body)
      message.success(form.enabled ? '已创建并开启自动检测' : '已创建:自动检测还没开,可以先手动检测一次')
    }
    emit('saved')
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <a-modal
    :open="open"
    :title="editing ? '编辑 Boce 拨测任务' : copy ? '复制 Boce 拨测任务' : '新建 Boce 拨测任务'"
    :width="760"
    :mask-closable="false"
    :confirm-loading="saving"
    ok-text="保存"
    cancel-text="取消"
    @cancel="emit('close')"
    @ok="save"
  >
    <div class="btf" data-testid="boce-task-form">
      <div v-if="node.blocked" class="lb-notice btf__notice">
        <span class="lb-notice__icon lb-notice__icon--warn"><LbIcon name="circle-alert" :size="18" /></span>
        <div class="lb-notice__body">
          <div class="lb-notice__text">{{ node.blocked }}。任务可以先建好,满足条件之后才会执行。</div>
        </div>
      </div>
      <div v-if="error" class="lb-error-strip">{{ error }}</div>
      <div class="lb-group">
        <div class="lb-group__row btf__row">
          <span class="lb-group__label">任务名称</span>
          <a-input v-model:value="form.name" :maxlength="64" placeholder="留空 = 用入口名" aria-label="任务名称" />
        </div>
        <div class="lb-group__row btf__row">
          <span class="lb-group__label">
            被测入口
            <LbInfoTip :width="320" text="只能从这台节点的订阅入口里选,地址与端口由服务端按入口配置解析(NAT 映射的公网端口照订阅的规则走);入口地址或端口变了,测量跟着走,历史按目标版本分开统计。创建后不能更换,要测别的入口请复制任务。" />
          </span>
          <a-select
            v-model:value="form.ref"
            :disabled="editing"
            placeholder="选择订阅入口"
            aria-label="被测入口"
            class="btf__target"
          >
            <a-select-option v-for="c in node.candidates" :key="c.ref" :value="c.ref" :disabled="!!c.note" :title="c.note">
              {{ c.label }} · {{ c.host }}:{{ c.port }}<template v-if="c.note"> —— {{ c.note }}</template>
            </a-select-option>
          </a-select>
        </div>
        <div class="lb-group__row btf__row">
          <span class="lb-group__label">检测类型</span>
          <span class="btf__muted">TCPing(首期只支持这一种;Boce 拨测点对入口的端口做 TCP 连接测速)</span>
        </div>
      </div>

      <div class="btf__sec">
        <div class="btf__sec-title">拨测点</div>
        <BoceVantagePicker v-model="form.vantage_ids" :vantages="vantages" :keep="keepIDs" :points-per-check="node.points_per_check" />
      </div>

      <div class="btf__sec">
        <div class="btf__sec-title">时间表</div>
        <BoceScheduleEditor v-model="spec" />
      </div>

      <div class="lb-group">
        <div class="lb-group__row btf__row">
          <span class="lb-group__label">
            单任务每月上限
            <LbInfoTip :width="300" text="可选。0 = 只受全局的日 / 月 / 单次上限约束;设了之后这个任务本月估算消耗到了就不再自动执行(记「未执行」)。" />
          </span>
          <a-input-number v-model:value="form.month_points_limit" :min="0" :max="100000000" :precision="0" addon-after="波点 / 月" />
        </div>
        <div v-if="!editing" class="lb-group__row btf__row btf__row--switch">
          <span class="lb-group__label">保存后开启自动检测</span>
          <a-switch v-model:checked="form.enabled" aria-label="保存后开启自动检测" />
        </div>
        <div class="lb-group__row btf__row">
          <span class="lb-group__label">备注</span>
          <a-input v-model:value="form.note" :maxlength="200" aria-label="备注" />
        </div>
      </div>

      <BoceEstimatePanel :estimate="estimate" :loading="estimating" :error="estimateError" />
    </div>
  </a-modal>
</template>

<style scoped>
.btf {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.btf__row {
  grid-template-columns: 130px minmax(0, 1fr);
}
.btf__row--switch {
  grid-template-columns: minmax(0, 1fr) auto;
}
.btf__target {
  width: 100%;
}
.btf__sec {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.btf__sec-title {
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--text2);
}
.btf__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.btf__notice {
  margin: 0;
}
</style>
