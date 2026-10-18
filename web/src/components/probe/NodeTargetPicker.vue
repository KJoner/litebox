<script setup lang="ts">
import { ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type NodePublicProbe, type ProbeBindingMode, type ProbeTrialResult } from '@/api/client'
import { LbInfoTip } from '@/components/lb'
import { rttText } from '@/components/asset/probeView'
import ProbeTargetSelectTable from './ProbeTargetSelectTable.vue'

/**
 * 节点详情里的「选择公共目标」:从目标库里勾选,在这台机器上启用 / 停用 / 改回跟随全局;
 * 也可以对勾选的目标(最多 8 个)做一次试测 —— 试测与周期探测走同一个辅助程序、同一个非阻塞取锁,
 * 第三方目标照样占预算,结果不进历史,只写回目标库的验证状态。
 */
const props = defineProps<{ open: boolean; nodeId: number; publics: NodePublicProbe[] }>()
const emit = defineEmits<{ (e: 'close'): void; (e: 'saved'): void }>()

const picked = ref<number[]>([])
const mode = ref<ProbeBindingMode>('ON')
const saving = ref(false)
const trialing = ref(false)
const trial = ref<ProbeTrialResult[] | null>(null)
const error = ref('')

watch(
  () => props.open,
  (open) => {
    if (!open) return
    picked.value = []
    trial.value = null
    error.value = ''
  },
)

async function apply() {
  if (!picked.value.length) {
    error.value = '至少勾选一个目标'
    return
  }
  saving.value = true
  error.value = ''
  try {
    await api.setProbeBindings({ node_ids: [props.nodeId], public_ids: picked.value, mode: mode.value })
    message.success(
      `${picked.value.length} 个目标在这台机器上改为${{ ON: '启用', OFF: '停用', INHERIT: '跟随全局' }[mode.value]}`,
    )
    emit('saved')
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    saving.value = false
  }
}

async function runTrial() {
  if (!picked.value.length || picked.value.length > 8) {
    error.value = '试测一次勾选 1~8 个目标'
    return
  }
  trialing.value = true
  error.value = ''
  trial.value = null
  try {
    trial.value = (await api.probeTrial(props.nodeId, picked.value)).results
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '试测失败'
  } finally {
    trialing.value = false
  }
}

function trialTone(r: ProbeTrialResult): string {
  if (r.status === 'OK') return 'ntp__ok'
  if (r.status === 'SSH_ERROR' || r.status === 'TOOL_ERROR') return 'ntp__muted'
  if (r.status === 'LOSS') return 'ntp__warn'
  return 'ntp__bad'
}
</script>

<template>
  <a-modal :open="open" title="选择公共目标" :width="900" :mask-closable="false" @cancel="emit('close')">
    <div class="ntp" data-testid="node-target-picker">
      <div class="ntp__muted">
        这里只改这一台机器的开关(覆盖目标库里的「全部节点」设置)。第三方来源要先在目标库里确认授权;
        单台节点有第三方目标数上限,超出时会直接拒绝并说明原因。
      </div>
      <ProbeTargetSelectTable v-model="picked" :items="publics" node-state />
      <div v-if="error" class="lb-error-strip">{{ error }}</div>
      <div v-if="trial" class="ntp__trial" data-testid="node-trial-results">
        <div class="ntp__trial-title">
          试测结果(不进历史)
          <LbInfoTip :width="300" text="采集失败(面板连不上这台机器)与工具不可用说明不了目标怎样,不写回验证状态;其余结果写回目标库的「验证」一列。" />
        </div>
        <div v-for="r in trial" :key="r.public_id" class="ntp__trial-row lb-tabular">
          <span class="ntp__name lb-ellipsis">{{ r.name }}</span>
          <span :class="trialTone(r)">{{ r.label }}</span>
          <span>{{ r.rtt_ms != null ? rttText(r.rtt_ms) : '' }}</span>
          <span class="ntp__muted">
            <template v-if="r.sent">{{ r.received }}/{{ r.sent }} {{ r.metric === 'LOSS' ? '收到' : r.metric === 'SYN' ? '有应答' : '' }}</template>
            <template v-if="r.peer"> · {{ r.peer }}</template>
            <template v-if="r.error"> · {{ r.error }}</template>
          </span>
        </div>
      </div>
    </div>
    <template #footer>
      <div class="ntp__foot">
        <a-button :loading="trialing" :disabled="!picked.length || picked.length > 8" @click="runTrial">
          试测勾选的({{ picked.length }}/8)
        </a-button>
        <span class="ntp__spacer" />
        <a-select v-model:value="mode" class="ntp__mode" aria-label="在这台机器上">
          <a-select-option value="ON">在这台机器上启用</a-select-option>
          <a-select-option value="OFF">在这台机器上停用</a-select-option>
          <a-select-option value="INHERIT">跟随全局设置</a-select-option>
        </a-select>
        <a-button @click="emit('close')">关闭</a-button>
        <a-button type="primary" :loading="saving" :disabled="!picked.length" @click="apply">应用到本机</a-button>
      </div>
    </template>
  </a-modal>
</template>

<style scoped>
.ntp {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.ntp__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
  line-height: 1.6;
}
.ntp__trial {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: var(--sp-3);
  border-radius: var(--r-group);
  background: var(--surface2);
  font-size: var(--fs-xs);
}
.ntp__trial-title {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-weight: 600;
  color: var(--text2);
}
.ntp__trial-row {
  display: grid;
  grid-template-columns: minmax(0, 200px) 120px 70px minmax(0, 1fr);
  gap: var(--sp-2);
}
.ntp__name {
  font-weight: 600;
}
.ntp__ok {
  color: var(--ok);
}
.ntp__warn {
  color: var(--warn);
}
.ntp__bad {
  color: var(--bad);
}
.ntp__foot {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
}
.ntp__spacer {
  flex: 1;
}
.ntp__mode {
  width: 170px;
}
</style>
