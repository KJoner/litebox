<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { message, Modal } from 'ant-design-vue'
import { api, ApiError, type AccessTier, type Node, type ProxyUser } from '@/api/client'
import { checkLoginUsername, checkPassword } from '@/utils/validate'
import { LbSensitiveField } from '@/components/lb'
import { fromBytes, toBytes, type LbQuotaUnit } from './quota'

/**
 * 新建 / 编辑用户。同一个表单两种 mode。
 *
 * 差异只有三处,其余字段共用:
 *   门户登录  新建时整块出现;编辑态隐藏 —— 已有账号在详情抽屉里管,
 *             那里才能同时说清「重设密码会踢掉全部会话」。
 *   标识字段  新建显示「自动分配」;编辑显示真值且只读。
 *   主按钮    「创建用户」/「保存」,后者无改动时禁用。
 */
const props = defineProps<{
  open: boolean
  /** null = 新建 */
  user: ProxyUser | null
  tiers: AccessTier[]
  nodes: Node[]
}>()

const emit = defineEmits<{
  (e: 'update:open', v: boolean): void
  (e: 'saved'): void
}>()

const isEdit = computed(() => props.user !== null)
const submitting = ref(false)
/** 只有服务端知道的错误(重名、外键)放表单顶部,不弹吐司。 */
const serverError = ref('')

const blank = {
  display_name: '',
  remark: '',
  quota_value: null as number | null,
  quota_unit: 'GB' as LbQuotaUnit,
  expires_at: '',
  reset_cycle: 'NONE' as 'NONE' | 'MONTHLY',
  reset_day: 1,
  access_tier_id: 1,
  node_ids: [] as number[],
  login_username: '',
  login_password: '',
  must_change_password: true,
}
const form = reactive({ ...blank })
/** 打开时的快照,用于脏检查 —— 不用「碰过就算脏」。 */
let snapshot = JSON.stringify(blank)

watch(
  () => props.open,
  (open) => {
    if (!open) return
    serverError.value = ''
    const u = props.user
    if (!u) {
      Object.assign(form, blank, { access_tier_id: props.tiers[0]?.id ?? 1 })
    } else {
      const q = fromBytes(u.quota_bytes)
      Object.assign(form, {
        ...blank,
        display_name: u.display_name,
        remark: u.remark,
        quota_value: q.value,
        quota_unit: q.unit,
        expires_at: u.expires_at ? u.expires_at.slice(0, 10) : '',
        reset_cycle: u.reset_cycle,
        reset_day: u.reset_day,
        access_tier_id: u.access_tier_id,
        // 只回填额外授权:等级继承来的节点不在这里改,
        // 塞进多选框会让管理员一保存就把继承关系固化成手工授权。
        node_ids: [...u.node_ids],
      })
    }
    snapshot = JSON.stringify(form)
  },
)

const dirtyFields = computed(() => {
  const before = JSON.parse(snapshot) as typeof form
  const labels: Record<string, string> = {
    display_name: '用户名称',
    remark: '备注',
    quota_value: '流量额度',
    quota_unit: '流量额度',
    expires_at: '到期时间',
    reset_cycle: '流量重置',
    reset_day: '重置日',
    access_tier_id: '访问等级',
    node_ids: '额外授权节点',
  }
  const out = new Set<string>()
  for (const k of Object.keys(labels)) {
    const a = JSON.stringify((before as never)[k])
    const b = JSON.stringify((form as never)[k])
    if (a !== b) out.add(labels[k])
  }
  return [...out]
})

const usernameError = computed(() =>
  form.login_username ? checkLoginUsername(form.login_username) : undefined,
)
const passwordError = computed(() =>
  form.login_password ? checkPassword(form.login_password) : undefined,
)

const nodeOptions = computed(() =>
  props.nodes.map((n) => ({
    // 标出这台机器上入口的等级:否则会给普通用户重复授权他早就继承到的机器。
    // 等级已经降到入口上,所以一台机器可能同时列出几档 —— 那正是要看到的。
    label: `${n.name}(${tiersOf(n)}·${n.host})`,
    value: n.id,
  })),
)

/** 这台机器上入口的等级,去重。没有入口时说清楚 —— 授权它没有任何效果。 */
function tiersOf(n: Node): string {
  if (n.role === 'RELAY') return '转发规则各自设定'
  const names = [...new Set((n.inbounds ?? []).map((i) => i.access_tier_name))]
  return names.length ? names.join('/') : '无入口'
}

function close() {
  emit('update:open', false)
}

/** 确实脏了才拦截,并且列出改了哪几项 —— 只说「有未保存的修改」还得回去找。 */
function tryClose() {
  if (dirtyFields.value.length === 0) return close()
  Modal.confirm({
    title: '放弃未保存的修改?',
    content: `已改动 ${dirtyFields.value.length} 项:${dirtyFields.value.join('、')}。`,
    okText: '放弃',
    okType: 'danger',
    cancelText: '继续编辑',
    autoFocusButton: 'cancel',
    onOk: close,
  })
}

async function submit() {
  if (!form.display_name.trim()) {
    serverError.value = '请填写用户名称'
    return
  }
  // 能在前端判断的(格式、长度、必填)提交前拦住,不让它变成服务端错误。
  if (!isEdit.value && form.login_username && (usernameError.value || passwordError.value)) {
    serverError.value = '登录账号或初始密码格式不正确'
    return
  }

  submitting.value = true
  serverError.value = ''
  const quota_bytes = toBytes(form.quota_value, form.quota_unit)
  // 日期输入只有年月日,补成当天结束的 UTC 时刻。
  const expiresAt = form.expires_at ? `${form.expires_at}T23:59:59Z` : ''

  try {
    if (!isEdit.value) {
      const created = await api.createUser({
        display_name: form.display_name,
        remark: form.remark,
        quota_bytes,
        expires_at: expiresAt || undefined,
        reset_cycle: form.reset_cycle,
        reset_day: form.reset_day,
        access_tier_id: form.access_tier_id,
        node_ids: form.node_ids,
        login_username: form.login_username,
        login_password: form.login_password,
        must_change_password: form.must_change_password,
      })
      // 口令只在这一次请求里用到,立刻从表单状态里抹掉。
      form.login_password = ''

      if (created.portal_account_error) {
        // 用户建好了、登录账号没建成。必须分开说,否则管理员会再建一个用户。
        Modal.warning({
          title: '用户已创建,但登录账号没有建成',
          width: 520,
          content: `${created.portal_account_error}\n\n该用户现在无法登录用户中心 —— 他拿账号去登录只会得到「账号或密码错误」。请在用户详情里单独开通门户登录。不要重新创建用户。`,
          okText: '知道了',
        })
      } else if (created.portal_account) {
        // 这两项管理员必须转发给用户,用一条 3 秒吐司交付等于让他回详情页翻。
        Modal.success({
          title: '用户已创建,门户登录已开通',
          width: 520,
          content: `请把下面两项发给用户:\n\n登录地址:${window.location.origin}\n登录账号:${created.portal_account.username}\n\n受影响节点将在数秒内自动部署。`,
          okText: '知道了',
        })
      } else {
        message.success('用户已创建,受影响节点将在数秒内自动部署')
      }
    } else {
      await api.updateUser(props.user!.id, {
        display_name: form.display_name,
        remark: form.remark,
        quota_bytes,
        ...(expiresAt ? { expires_at: expiresAt } : { clear_expiry: true }),
        reset_cycle: form.reset_cycle,
        reset_day: form.reset_day,
        access_tier_id: form.access_tier_id,
        node_ids: form.node_ids,
      })
      message.success('已保存')
    }
    close()
    emit('saved')
  } catch (err) {
    serverError.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <a-modal
    :open="props.open"
    :title="isEdit ? `编辑用户 · ${props.user?.display_name}` : '新增用户'"
    :width="560"
    :confirm-loading="submitting"
    :ok-text="isEdit ? '保存' : '创建用户'"
    :ok-button-props="{ disabled: isEdit && dirtyFields.length === 0 }"
    cancel-text="取消"
    :mask-closable="false"
    @cancel="tryClose"
    @ok="submit"
  >
    <a-alert
      v-if="serverError"
      type="error"
      show-icon
      class="uf__err"
      :message="serverError"
      description="表单内容已保留。"
    />

    <a-form layout="vertical">
      <a-form-item label="用户名称" required>
        <a-input v-model:value="form.display_name" placeholder="用于识别的显示名" />
        <div class="uf__help">只在管理后台出现,用户看不到。</div>
      </a-form-item>

      <a-form-item label="备注">
        <a-input v-model:value="form.remark" placeholder="例如:2026 年 8 月起" />
      </a-form-item>

      <a-form-item label="访问等级">
        <a-select v-model:value="form.access_tier_id">
          <a-select-option v-for="t in props.tiers" :key="t.id" :value="t.id">
            {{ t.name }} —— {{ t.description }}
          </a-select-option>
        </a-select>
        <div class="uf__help">等级不高于该等级的节点会自动可用,不必逐个勾选。</div>
      </a-form-item>

      <a-row :gutter="12">
        <a-col :span="14">
          <a-form-item label="流量额度">
            <a-input-number
              v-model:value="form.quota_value"
              :min="0"
              :precision="2"
              placeholder="不限量"
              style="width: 100%"
            />
          </a-form-item>
        </a-col>
        <a-col :span="10">
          <a-form-item label="单位">
            <a-select v-model:value="form.quota_unit">
              <a-select-option value="GB">GB</a-select-option>
              <a-select-option value="TB">TB</a-select-option>
            </a-select>
          </a-form-item>
        </a-col>
      </a-row>
      <div class="uf__help uf__help--row">
        留空或填 0 表示不限量。单位只影响输入,提交时换算成字节发给接口。
      </div>

      <a-form-item label="到期时间">
        <a-input v-model:value="form.expires_at" type="date" style="width: 100%" />
        <div class="uf__help">留空表示不过期。边界统一取 UTC。</div>
      </a-form-item>

      <a-form-item label="流量重置">
        <a-radio-group v-model:value="form.reset_cycle">
          <a-radio value="NONE">不重置</a-radio>
          <a-radio value="MONTHLY">每月</a-radio>
        </a-radio-group>
      </a-form-item>

      <a-form-item v-if="form.reset_cycle === 'MONTHLY'" label="重置日">
        <a-input-number v-model:value="form.reset_day" :min="1" :max="28" style="width: 160px" />
        <!-- 用户是 1~28,节点是 1~31。两处规则不同,各自写准确的帮助文字。 -->
        <div class="uf__help">1~28 日。不支持 29~31,避开短月份歧义。边界统一取 UTC 00:00。</div>
      </a-form-item>

      <template v-if="!isEdit">
        <a-divider orientation="left" plain>门户登录(可选)</a-divider>
        <a-form-item
          label="登录账号"
          :validate-status="usernameError ? 'error' : ''"
          :help="usernameError"
        >
          <a-input v-model:value="form.login_username" placeholder="字母、数字、下划线、连字符与点,3~32 位" autocomplete="off" />
          <div v-if="!usernameError" class="uf__help">留空表示该用户只用订阅,不登录用户中心。</div>
        </a-form-item>

        <template v-if="form.login_username">
          <div :class="passwordError ? 'uf__pwd uf__pwd--err' : 'uf__pwd'">
            <LbSensitiveField
              v-model:value="form.login_password"
              label="初始密码"
              mode="create"
              required
              help="至少 8 位。提交后不再回显,也不写进审计日志。请通过安全渠道发给用户。"
            />
            <div v-if="passwordError" class="uf__pwd-err">{{ passwordError }}</div>
          </div>
          <a-form-item>
            <a-checkbox v-model:checked="form.must_change_password">
              要求用户首次登录后修改密码
            </a-checkbox>
          </a-form-item>
        </template>
        <a-divider />
      </template>

      <a-form-item label="额外授权节点">
        <a-select
          v-model:value="form.node_ids"
          mode="multiple"
          :options="nodeOptions"
          placeholder="通常留空"
          style="width: 100%"
        />
        <div class="uf__help">
          在访问等级之外单独追加。等级已经覆盖的节点不必勾选 ——
          勾了会把继承关系固化成手工授权,之后调等级就不再自动生效。
        </div>
      </a-form-item>

      <a-form-item v-if="isEdit" label="用户编号 / UUID">
        <a-input :value="`${props.user?.user_code}`" disabled />
        <div class="uf__help">重新生成 UUID 不在这里 —— 它是危险操作,走详情页的确认弹窗。</div>
      </a-form-item>
    </a-form>

    <div v-if="!isEdit" class="uf__foot">保存后受影响节点将在数秒内自动部署。</div>
    <div v-else-if="dirtyFields.length" class="uf__foot">已改动 {{ dirtyFields.length }} 项</div>
  </a-modal>
</template>

<style scoped>
.uf__err {
  margin-bottom: 16px;
}

.uf__help {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.6;
  color: #6b7480;
}

.uf__help--row {
  margin: -12px 0 20px;
}

.uf__pwd--err :deep(.ant-input-affix-wrapper) {
  border-color: #b4291d;
}

.uf__pwd-err {
  margin: -18px 0 20px;
  font-size: 12px;
  color: #b4291d;
}

.uf__foot {
  padding-top: 4px;
  font-size: 11.5px;
  color: #6b7480;
}
</style>
