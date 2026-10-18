<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type PublicProbeNode, type PublicProbeScope, type PublicProbeTarget } from '@/api/client'

/**
 * 把一个或一批公共目标应用到节点:全部节点 / 选中的节点 / 都不启用。
 * 这是「换成这个集合」:节点详情里单独设过的启用 / 停用会一并清掉。
 * 第三方来源没确认授权时服务端会拒绝;单台节点超过第三方目标上限时返回 409,原因写在弹窗里。
 */
const props = defineProps<{ open: boolean; targets: PublicProbeTarget[] }>()
const emit = defineEmits<{ (e: 'close'): void; (e: 'saved'): void }>()

const scope = ref<PublicProbeScope>('SELECTED')
const nodes = ref<PublicProbeNode[]>([])
const picked = ref<number[]>([])
const loading = ref(false)
const saving = ref(false)
const error = ref('')

const single = computed(() => (props.targets.length === 1 ? props.targets[0] : null))
const thirdParty = computed(() => props.targets.filter((t) => t.third_party))
const unauthorized = computed(() => props.targets.filter((t) => t.pause_reason.includes('授权')))

watch(
  () => props.open,
  async (open) => {
    if (!open || !props.targets.length) return
    error.value = ''
    loading.value = true
    try {
      const r = await api.publicProbeTargetNodes(props.targets[0].id)
      nodes.value = r.nodes
      if (single.value) {
        const p = single.value
        picked.value = r.nodes.filter((n) => n.effective || n.mode === 'ON').map((n) => n.node_id)
        // 一台都没选时默认「都不启用」:刚登记完直接点保存,不会有任何机器开始测。
        scope.value = p.apply_all && !p.off_count ? 'ALL' : picked.value.length ? 'SELECTED' : 'NONE'
      } else {
        picked.value = []
        scope.value = 'SELECTED'
      }
    } catch (err) {
      error.value = err instanceof ApiError ? err.message : '读取节点失败'
    } finally {
      loading.value = false
    }
  },
)

async function save() {
  if (scope.value === 'SELECTED' && !picked.value.length) {
    error.value = '至少选一台节点,或者改选「都不启用」'
    return
  }
  saving.value = true
  error.value = ''
  try {
    await api.publicProbeBatch({
      ids: props.targets.map((t) => t.id),
      action: 'scope',
      scope: scope.value,
      node_ids: scope.value === 'SELECTED' ? picked.value : [],
    })
    const what = single.value ? `「${single.value.name}」` : `${props.targets.length} 个目标`
    message.success(
      scope.value === 'ALL'
        ? `${what}已对全部自建节点启用`
        : scope.value === 'SELECTED'
          ? `${what}已对 ${picked.value.length} 台节点启用`
          : `${what}已对所有节点停用(历史保留)`,
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
  <a-modal
    :open="open"
    :title="single ? `启用节点 · ${single.name}` : `应用到节点 · ${targets.length} 个目标`"
    :width="540"
    :confirm-loading="saving"
    ok-text="保存"
    cancel-text="取消"
    :mask-closable="false"
    @cancel="emit('close')"
    @ok="save"
  >
    <div class="psm" data-testid="probe-scope-modal">
      <div v-if="error" class="lb-error-strip">{{ error }}</div>
      <div v-if="unauthorized.length" class="lb-error-strip">
        其中 {{ unauthorized.length }} 个来自还没有确认授权的第三方来源:先在来源卡片里阅读使用说明并确认,才能对节点启用。
      </div>
      <a-skeleton v-if="loading" active :paragraph="{ rows: 3 }" />
      <template v-else>
        <a-radio-group v-model:value="scope" class="psm__scope">
          <a-radio value="ALL">全部自建节点(含以后新建的)</a-radio>
          <a-radio value="SELECTED">只对选中的节点</a-radio>
          <a-radio value="NONE">都不启用</a-radio>
        </a-radio-group>
        <div v-if="scope === 'SELECTED'" class="psm__nodes">
          <div class="psm__head">
            <span class="psm__muted">已选 {{ picked.length }} / {{ nodes.length }}</span>
            <button type="button" class="lb-linkbtn" @click="picked = nodes.map((n) => n.node_id)">全选</button>
            <button type="button" class="lb-linkbtn" @click="picked = []">全不选</button>
          </div>
          <a-checkbox-group v-model:value="picked" class="psm__list">
            <a-checkbox v-for="n in nodes" :key="n.node_id" :value="n.node_id">
              {{ n.display_name || n.name }} <span class="psm__muted">{{ n.name }}</span>
              <span v-if="n.paused" class="lb-chip psm__chip">本机已暂停</span>
            </a-checkbox>
          </a-checkbox-group>
        </div>
        <div class="psm__muted">
          这是「换成这个集合」:节点详情里单独设过的启用 / 停用会一并清掉。
          <template v-if="thirdParty.length">
            其中 {{ thirdParty.length }} 个是第三方目标:单台节点有第三方目标数上限,各机器的首次探测会错峰,全站另有每分钟预算与单目标限流,
            超出的延后执行、不记失败。
          </template>
          启用之后,面板经已有的 SSH 连接在这些机器上临时启动一个约 3 MB 的辅助程序(跑完即退出,不常驻、不监听端口)。
        </div>
      </template>
    </div>
  </a-modal>
</template>

<style scoped>
.psm {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.psm__scope {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.psm__nodes {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-3);
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
  max-height: 280px;
  overflow: auto;
}
.psm__head {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
}
.psm__list {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.psm__chip {
  margin-left: 4px;
}
.psm__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
  line-height: 1.6;
}
</style>
