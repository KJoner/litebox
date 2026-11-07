<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type GrantEntryKind, type ProxyUser } from '@/api/client'
import { lbDangerConfirm } from '@/components/lb'

/**
 * 入口这一侧的授权(VNext AUTH03):开不开「仅指定用户」、直接授权了谁。
 * 与用户详情里的「入口授权」是同一张表的两个视角 —— 在这里改,那边立刻看得到。
 *
 * 保存要重新下发这台机器的 sing-box(Mieru 入口逐入口下发,不自动做),
 * 所以走 lbDangerConfirm 并写出后果。
 */
const props = defineProps<{
  open: boolean
  target: { kind: GrantEntryKind; id: number; name: string; nodeName: string } | null
}>()
const emit = defineEmits<{ 'update:open': [boolean]; saved: [] }>()

const loading = ref(false)
const loadError = ref('')
const restricted = ref(false)
const userIds = ref<number[]>([])
const users = ref<ProxyUser[]>([])
const saving = ref(false)
const original = ref<{ restricted: boolean; ids: number[] }>({ restricted: false, ids: [] })
/** 共享 psk 的 Snell(R04):改授权只改订阅可见性,收不回已经拿到 psk 的人。 */
const shared = ref(false)

const options = computed(() =>
  users.value.map((u) => ({ value: u.id, label: `${u.display_name}(${u.user_code})` })),
)

watch(
  () => [props.open, props.target] as const,
  async ([open, t]) => {
    if (!open || !t) return
    loading.value = true
    loadError.value = ''
    try {
      const [ea, us] = await Promise.all([api.entryAccess(t.kind, t.id), api.users()])
      users.value = us.items
      restricted.value = ea.restricted
      shared.value = ea.shared_credential
      userIds.value = ea.users.map((u) => u.id)
      original.value = { restricted: ea.restricted, ids: [...userIds.value] }
    } catch (e) {
      loadError.value = e instanceof ApiError ? e.message : '读取失败'
    } finally {
      loading.value = false
    }
  },
  { immediate: true },
)

function close() {
  emit('update:open', false)
}

function save() {
  const t = props.target
  if (!t) return
  const impacts = [
    restricted.value
      ? `**这个入口只对下面 ${userIds.value.length} 个用户开放**,等级继承与整机授权都不再算`
      : `这个入口仍按访问等级开放,另外直接授权 ${userIds.value.length} 个用户(越过等级门槛)`,
    t.kind === 'SINGBOX'
      ? `会自动重新下发「${t.nodeName}」的 sing-box —— 重启时这台机器上全部入口的在线连接会断一下`
      : '这是 Mieru 入口:只动了用户的话会自动 reload(一条连接都不断,已建立的会话可能继续存活);' +
        '它还有端口 / 传输层 / 出口改动没下发时不会自动做,要在列表上手动下发',
    '直接授权越过的只是等级门槛:账号停用、过期、超额照样生效',
  ]
  if (shared.value) {
    impacts.unshift(
      '**这个入口用共享 psk:这里的改动只决定订阅里给谁这个入口,收不回已经拿到 psk 的人**。' +
        '要真正收回,保存之后在入口上「轮换 psk」(所有人一起换)、改成逐用户凭据,或停用入口',
    )
    if (restricted.value && !userIds.value.length)
      impacts.unshift('**一个用户都没选:订阅里对所有人不再出现,但持有 psk 的人仍然连得上**')
  } else if (restricted.value && !userIds.value.length) {
    impacts.unshift('**一个用户都没选:这个入口将对所有人不可用**')
  }
  lbDangerConfirm({
    title: `保存「${t.name}」的入口授权?`,
    okText: '保存',
    okType: restricted.value && !original.value.restricted ? 'danger' : 'primary',
    impacts,
    onOk: () => {
      void doSave(t)
    },
  })
}

async function doSave(t: NonNullable<typeof props.target>) {
  saving.value = true
  try {
    const res = await api.setEntryAccess(t.kind, t.id, { restricted: restricted.value, user_ids: userIds.value })
    message.success(res.auto_deploy ? '已保存,这台机器会自动重新下发' : '已保存,这个 Mieru 入口会自动 reload')
    emit('saved')
    close()
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '保存失败')
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <a-modal
    :open="open"
    :title="target ? `入口授权 · ${target.nodeName}/${target.name}` : '入口授权'"
    :confirm-loading="saving"
    ok-text="保存"
    cancel-text="取消"
    :ok-button-props="{ disabled: loading || !!loadError }"
    width="520px"
    @ok="save"
    @cancel="close"
  >
    <a-skeleton v-if="loading" active :paragraph="{ rows: 3 }" />
    <div v-else-if="loadError" class="ea__bad">{{ loadError }}</div>
    <div v-else class="ea">
      <label class="ea__row">
        <a-switch v-model:checked="restricted" size="small" />
        <span>仅指定用户</span>
      </label>
      <div class="ea__hint">
        开着时这个入口只经直接授权出现,等级继承与整机授权都不算。关着时按访问等级开放,
        下面的用户是在等级之外额外授权的。
      </div>
      <div v-if="shared" class="ea__warn">
        共享 psk 入口:节点上没有逐用户身份,这里只管订阅里给谁。拿到过 psk 的人(包括被移出的)都还连得上 ——
        收回要在入口上「轮换 psk」,所有仍有权限的人都要重新拉一次订阅。
      </div>
      <a-select
        v-model:value="userIds"
        mode="multiple"
        :options="options"
        placeholder="选择直接授权的用户"
        show-search
        option-filter-prop="label"
        style="width: 100%"
      />
    </div>
  </a-modal>
</template>

<style scoped>
.ea {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.ea__row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  font-weight: 600;
  cursor: pointer;
}
.ea__hint {
  font-size: var(--fs-xs);
  color: var(--text2);
}
.ea__bad {
  color: var(--bad);
}
.ea__warn {
  font-size: var(--fs-xs);
  color: var(--warn);
}
</style>
