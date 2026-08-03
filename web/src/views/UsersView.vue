<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { message, Modal } from 'ant-design-vue'
import { api, ApiError, type AccessTier, type Node, type ProxyUser } from '@/api/client'
import { daysUntil, formatBytes, formatQuota, formatTime } from '@/utils/format'
import StatusTag from '@/components/StatusTag.vue'
import UserDetailDrawer from '@/components/UserDetailDrawer.vue'

const users = ref<ProxyUser[]>([])
const nodes = ref<Node[]>([])
const loading = ref(false)
const detailId = ref<number | null>(null)

const tiers = ref<AccessTier[]>([])

// 额外授权的候选项里标出节点自己的等级:管理员挑节点时得知道
// 这个节点本来归谁用,否则会给普通用户重复授权他早就继承到的节点。
const nodeOptions = computed(() =>
  nodes.value.map((n) => ({
    label: `${n.name}（${n.access_tier_name}·${n.host}）`,
    value: n.id,
  })),
)

// 等级配色按"能用到的节点越多颜色越重"排,让越权配置在列表里一眼看得出来。
function tierColor(code: string): string {
  if (code === 'root') return 'red'
  if (code === 'vip') return 'gold'
  return 'default'
}

const columns = [
  { title: '用户', key: 'name', width: 200 },
  { title: '访问等级', key: 'tier', width: 110 },
  { title: '登录账号', key: 'login', width: 150 },
  { title: '状态', key: 'status', width: 110 },
  { title: '节点', key: 'nodes', width: 110 },
  { title: '已用流量', key: 'used', width: 180 },
  { title: '到期时间', key: 'expires', width: 160 },
  { title: '操作', key: 'actions', width: 200 },
]

async function load() {
  loading.value = true
  try {
    const [u, n, t] = await Promise.all([api.users(), api.nodes(), api.accessTiers()])
    users.value = u.items
    nodes.value = n.items
    tiers.value = t.items
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载用户列表失败')
  } finally {
    loading.value = false
  }
}

// ---------- 新增 / 编辑 ----------

const formOpen = ref(false)
const editingId = ref<number | null>(null)
const submitting = ref(false)
const form = reactive({
  display_name: '',
  remark: '',
  quota_gb: 0,
  expires_at: '',
  reset_cycle: 'NONE' as 'NONE' | 'MONTHLY',
  reset_day: 1,
  access_tier_id: 1,
  node_ids: [] as number[],
  // 门户登录账号,仅新增时使用。已有用户的登录账号在详情抽屉里管理 ——
  // 那里才有"重设密码会踢掉全部会话"这类需要当场说明的后果。
  login_username: '',
  login_password: '',
  must_change_password: true,
})

function openCreate() {
  editingId.value = null
  Object.assign(form, {
    display_name: '',
    remark: '',
    quota_gb: 0,
    expires_at: '',
    reset_cycle: 'NONE',
    reset_day: 1,
    access_tier_id: 1,
    node_ids: [],
    login_username: '',
    login_password: '',
    must_change_password: true,
  })
  formOpen.value = true
}

function openEdit(u: ProxyUser) {
  editingId.value = u.id
  Object.assign(form, {
    display_name: u.display_name,
    remark: u.remark,
    // 额度以 GB 输入,字节数对人不友好。
    quota_gb: u.quota_bytes > 0 ? Number((u.quota_bytes / 1024 ** 3).toFixed(2)) : 0,
    expires_at: u.expires_at ? u.expires_at.slice(0, 10) : '',
    reset_cycle: u.reset_cycle,
    reset_day: u.reset_day,
    access_tier_id: u.access_tier_id,
    // 只回填额外授权:等级继承来的节点不在这里改,
    // 把它们塞进多选框会让管理员一保存就把继承关系固化成手工授权。
    node_ids: [...u.node_ids],
  })
  formOpen.value = true
}

async function submit() {
  if (!form.display_name.trim()) {
    message.warning('请填写用户名称')
    return
  }
  submitting.value = true
  try {
    const quotaBytes = Math.round(form.quota_gb * 1024 ** 3)
    // 日期输入只有年月日,补成当天结束的 UTC 时刻。
    const expiresAt = form.expires_at ? `${form.expires_at}T23:59:59Z` : ''

    if (editingId.value === null) {
      const created = await api.createUser({
        display_name: form.display_name,
        remark: form.remark,
        quota_bytes: quotaBytes,
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
        // 用户已经建好了,只是登录账号没建成。这两件事必须分开说,
        // 否则管理员会以为整个操作失败而再建一个用户。
        Modal.warning({
          title: '用户已创建,但登录账号没有建成',
          content: `${created.portal_account_error}\n\n可以在用户详情里单独开通门户登录。`,
          okText: '知道了',
        })
      } else {
        message.success('用户已创建,受影响节点将在数秒内自动部署')
      }
    } else {
      await api.updateUser(editingId.value, {
        display_name: form.display_name,
        remark: form.remark,
        quota_bytes: quotaBytes,
        ...(expiresAt ? { expires_at: expiresAt } : { clear_expiry: true }),
        reset_cycle: form.reset_cycle,
        reset_day: form.reset_day,
        access_tier_id: form.access_tier_id,
        node_ids: form.node_ids,
      })
      message.success('已保存')
    }
    formOpen.value = false
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    submitting.value = false
  }
}

// ---------- 行内操作 ----------

async function toggleEnabled(u: ProxyUser) {
  const enable = u.status === 'DISABLED'
  try {
    await api.setUserEnabled(u.id, enable)
    message.success(enable ? '已启用' : '已停用,受影响节点将重新部署')
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '操作失败')
  }
}

function confirmDelete(u: ProxyUser) {
  Modal.confirm({
    title: `删除用户 ${u.display_name}?`,
    content: '删除后其 UUID 将在受影响节点重新部署后失效,历史流量记录会保留。',
    okText: '删除',
    okType: 'danger',
    cancelText: '取消',
    async onOk() {
      try {
        await api.deleteUser(u.id)
        message.success('已删除')
        await load()
      } catch (err) {
        message.error(err instanceof ApiError ? err.message : '删除失败')
      }
    },
  })
}

/** 已用流量占额度的比例,用于进度条。不限量时返回 null。 */
function usageRatio(u: ProxyUser): number | null {
  if (u.quota_bytes <= 0) return null
  return Math.min(100, Math.round((u.used_total / u.quota_bytes) * 100))
}

function expiryClass(u: ProxyUser): string {
  const days = daysUntil(u.expires_at)
  if (days === null) return ''
  if (days < 0) return 'expiry-past'
  if (days <= 7) return 'expiry-soon'
  return ''
}

onMounted(load)
</script>

<template>
  <a-card>
    <template #title>用户管理</template>
    <template #extra>
      <a-space>
        <a-button :loading="loading" @click="load">刷新</a-button>
        <a-button type="primary" @click="openCreate">新增用户</a-button>
      </a-space>
    </template>

    <a-alert
      v-if="nodes.length === 0"
      type="info"
      show-icon
      class="hint"
      message="还没有节点"
      description="用户需要分配到节点才能使用。请先在节点管理中添加并部署节点。"
    />

    <a-table
      :columns="columns"
      :data-source="users"
      :loading="loading"
      row-key="id"
      size="middle"
      :pagination="false"
      :scroll="{ x: 940 }"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'name'">
          <a @click="detailId = record.id">{{ record.display_name }}</a>
          <div class="user-code">{{ record.user_code }}</div>
        </template>

        <template v-else-if="column.key === 'tier'">
          <a-tag :color="tierColor(record.access_tier_code)">{{ record.access_tier_name }}</a-tag>
        </template>

        <template v-else-if="column.key === 'login'">
          <span v-if="!record.portal_account" class="muted">未开通</span>
          <template v-else>
            <div class="login-name">{{ record.portal_account.username }}</div>
            <div v-if="!record.portal_account.login_enabled" class="login-off">已停用</div>
            <div v-else-if="record.portal_account.must_change_password" class="login-warn">
              待改初始密码
            </div>
          </template>
        </template>

        <template v-else-if="column.key === 'status'">
          <StatusTag :status="record.status" kind="user" />
        </template>

        <template v-else-if="column.key === 'nodes'">
          <!-- 显示实际可用节点数。只显示额外授权数的话,纯靠等级继承的用户
               永远是 0,看起来像"没分配节点"。 -->
          <span class="tabular">{{ record.effective_node_ids.length }}</span>
          <span v-if="record.node_ids.length" class="extra-grant">
            (含追加 {{ record.node_ids.length }})
          </span>
        </template>

        <template v-else-if="column.key === 'used'">
          <div class="usage-value">
            {{ formatBytes(record.used_total) }} / {{ formatQuota(record.quota_bytes) }}
          </div>
          <a-progress
            v-if="usageRatio(record) !== null"
            :percent="usageRatio(record) ?? 0"
            :show-info="false"
            size="small"
            :status="(usageRatio(record) ?? 0) >= 100 ? 'exception' : 'normal'"
          />
        </template>

        <template v-else-if="column.key === 'expires'">
          <span v-if="!record.expires_at">不过期</span>
          <span v-else :class="expiryClass(record)">
            {{ formatTime(record.expires_at) }}
          </span>
        </template>

        <template v-else-if="column.key === 'actions'">
          <a-space size="small">
            <a @click="detailId = record.id">详情</a>
            <a @click="openEdit(record)">编辑</a>
            <a @click="toggleEnabled(record)">
              {{ record.status === 'DISABLED' ? '启用' : '停用' }}
            </a>
            <a class="danger" @click="confirmDelete(record)">删除</a>
          </a-space>
        </template>
      </template>
    </a-table>
  </a-card>

  <a-modal
    v-model:open="formOpen"
    :title="editingId === null ? '新增用户' : '编辑用户'"
    :confirm-loading="submitting"
    ok-text="保存"
    cancel-text="取消"
    @ok="submit"
  >
    <a-form layout="vertical">
      <a-form-item label="用户名称" required>
        <a-input v-model:value="form.display_name" placeholder="用于识别的显示名" />
      </a-form-item>
      <a-form-item label="备注">
        <a-input v-model:value="form.remark" />
      </a-form-item>
      <a-form-item label="访问等级" extra="等级不高于该等级的节点会自动可用,不必逐个勾选">
        <a-select v-model:value="form.access_tier_id">
          <a-select-option v-for="t in tiers" :key="t.id" :value="t.id">
            {{ t.name }} —— {{ t.description }}
          </a-select-option>
        </a-select>
      </a-form-item>
      <a-form-item label="流量额度(GB)" extra="填 0 表示不限量">
        <a-input-number v-model:value="form.quota_gb" :min="0" :step="1" style="width: 100%" />
      </a-form-item>
      <a-form-item label="到期时间" extra="留空表示不过期">
        <a-input v-model:value="form.expires_at" type="date" style="width: 100%" />
      </a-form-item>
      <a-form-item label="流量重置">
        <a-radio-group v-model:value="form.reset_cycle">
          <a-radio value="NONE">不重置</a-radio>
          <a-radio value="MONTHLY">每月</a-radio>
        </a-radio-group>
      </a-form-item>
      <a-form-item v-if="form.reset_cycle === 'MONTHLY'" label="重置日" extra="1~28 日">
        <a-input-number v-model:value="form.reset_day" :min="1" :max="28" style="width: 100%" />
      </a-form-item>
      <template v-if="editingId === null">
        <a-divider orientation="left" plain>门户登录（可选）</a-divider>
        <a-form-item label="登录账号" extra="留空表示该用户只用订阅,不登录用户中心">
          <a-input
            v-model:value="form.login_username"
            placeholder="字母、数字、下划线、连字符与点,3~32 位"
            autocomplete="off"
          />
        </a-form-item>
        <a-form-item
          v-if="form.login_username"
          label="初始密码"
          extra="至少 8 位,请通过安全渠道发给用户"
        >
          <a-input-password v-model:value="form.login_password" autocomplete="new-password" />
        </a-form-item>
        <a-form-item v-if="form.login_username">
          <a-checkbox v-model:checked="form.must_change_password">
            要求用户首次登录后修改密码
          </a-checkbox>
        </a-form-item>
        <a-divider />
      </template>

      <a-form-item
        label="额外授权节点"
        extra="在访问等级之外单独追加的节点。等级已经覆盖的节点不必在这里勾选"
      >
        <a-select
          v-model:value="form.node_ids"
          mode="multiple"
          :options="nodeOptions"
          placeholder="通常留空"
          style="width: 100%"
        />
      </a-form-item>
    </a-form>
  </a-modal>

  <UserDetailDrawer
    :user-id="detailId"
    :nodes="nodes"
    @close="detailId = null"
    @changed="load"
  />
</template>

<style scoped>
.login-name {
  font-size: 13px;
}

.login-off {
  color: #cf1322;
  font-size: 12px;
}

.login-warn {
  color: #d46b08;
  font-size: 12px;
}

.muted {
  color: rgb(0 0 0 / 45%);
}

.extra-grant {
  margin-left: 4px;
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
}

.tabular {
  font-variant-numeric: tabular-nums;
}

.hint {
  margin-bottom: 16px;
}

.user-code {
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}

.usage-value {
  font-variant-numeric: tabular-nums;
  font-size: 13px;
}

.expiry-soon {
  color: #d46b08;
}

.expiry-past {
  color: #cf1322;
}

.danger {
  color: #cf1322;
}
</style>
