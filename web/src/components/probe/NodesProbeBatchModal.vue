<script setup lang="ts">
import { ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type ProbeBindingMode, type PublicProbeTarget } from '@/api/client'
import ProbeTargetSelectTable from './ProbeTargetSelectTable.vue'

/**
 * 节点列表的批量操作「应用探测目标」:对勾选的几台机器,把勾选的公共目标一起改成 启用 / 停用 / 跟随全局。
 * 与目标库里的「应用到节点」不同,这里只动这几台的覆盖,不改目标的「全部节点」设置。
 */
const props = defineProps<{ open: boolean; nodeIds: number[] }>()
const emit = defineEmits<{ (e: 'close'): void; (e: 'saved'): void }>()

const targets = ref<PublicProbeTarget[]>([])
const picked = ref<number[]>([])
const mode = ref<ProbeBindingMode>('ON')
const loading = ref(false)
const saving = ref(false)
const error = ref('')

watch(
  () => props.open,
  async (open) => {
    if (!open) return
    picked.value = []
    error.value = ''
    loading.value = true
    try {
      targets.value = (await api.publicProbeTargets()).targets
    } catch (err) {
      error.value = err instanceof ApiError ? err.message : '读取目标库失败'
    } finally {
      loading.value = false
    }
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
    await api.setProbeBindings({ node_ids: props.nodeIds, public_ids: picked.value, mode: mode.value })
    message.success(
      `已在 ${props.nodeIds.length} 台节点上把 ${picked.value.length} 个目标改为${{ ON: '启用', OFF: '停用', INHERIT: '跟随全局' }[mode.value]}`,
    )
    emit('saved')
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <a-modal :open="open" :title="`应用探测目标 · ${nodeIds.length} 台节点`" :width="880" :mask-closable="false" @cancel="emit('close')">
    <div class="npb" data-testid="nodes-probe-batch">
      <div class="npb__muted">
        只改这几台机器上的开关,不改目标库里的「全部节点」设置。第三方来源要先在「探测目标库」里确认授权;
        单台节点有第三方目标数上限,超出时整批拒绝并列出是哪几台。
      </div>
      <a-skeleton v-if="loading" active :paragraph="{ rows: 4 }" />
      <ProbeTargetSelectTable v-else v-model="picked" :items="targets" />
      <div v-if="error" class="lb-error-strip">{{ error }}</div>
    </div>
    <template #footer>
      <div class="npb__foot">
        <a-select v-model:value="mode" class="npb__mode" aria-label="在这些机器上">
          <a-select-option value="ON">在这些机器上启用</a-select-option>
          <a-select-option value="OFF">在这些机器上停用</a-select-option>
          <a-select-option value="INHERIT">改回跟随全局</a-select-option>
        </a-select>
        <a-button @click="emit('close')">取消</a-button>
        <a-button type="primary" :loading="saving" :disabled="!picked.length" @click="apply">应用</a-button>
      </div>
    </template>
  </a-modal>
</template>

<style scoped>
.npb {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.npb__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
  line-height: 1.6;
}
.npb__foot {
  display: flex;
  justify-content: flex-end;
  align-items: center;
  gap: var(--sp-2);
}
.npb__mode {
  width: 170px;
}
</style>
