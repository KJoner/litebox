<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { message, Modal } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AccessTier,
  type AdjustAction,
  type AdjustPayload,
  type Node,
  type ProxyUser,
} from '@/api/client'
import { daysUntil, formatBytes, formatQuota, formatRelative, formatTime } from '@/utils/format'
import { checkLoginUsername, checkPassword } from '@/utils/validate'
import StatusTag from '@/components/StatusTag.vue'
import UserDetailDrawer from '@/components/UserDetailDrawer.vue'

const users = ref<ProxyUser[]>([])
const nodes = ref<Node[]>([])
const loading = ref(false)
const detailId = ref<number | null>(null)

const tiers = ref<AccessTier[]>([])

// 门户登录地址取当前页面的 origin:管理员正是在这个地址上操作的,它一定对。
// 订阅用的 base_url 是给代理客户端的,完全可能是另一个域名,不能拿来当登录地址。
const portalLoginURL = `${window.location.origin}/user/login`

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

// 筛选在前端做。用户规模是 10 人量级,把条件推到 SQL 只会多出一层
// 查询拼装代码,而它自己就是一处要维护的复杂度。
const filters = reactive({
  keyword: '',
  tierID: undefined as number | undefined,
  status: undefined as string | undefined,
  loginEnabled: undefined as 'yes' | 'no' | 'none' | undefined,
  expiringSoon: false,
  nearQuota: false,
})

const filteredUsers = computed(() =>
  users.value.filter((u) => {
    const kw = filters.keyword.trim().toLowerCase()
    if (kw) {
      const hay = [u.display_name, u.user_code, u.remark, u.portal_account?.username ?? '']
        .join(' ')
        .toLowerCase()
      if (!hay.includes(kw)) return false
    }
    if (filters.tierID !== undefined && u.access_tier_id !== filters.tierID) return false
    if (filters.status !== undefined && u.status !== filters.status) return false
    if (filters.loginEnabled === 'none' && u.portal_account) return false
    if (filters.loginEnabled === 'yes' && !u.portal_account?.login_enabled) return false
    if (filters.loginEnabled === 'no' && u.portal_account?.login_enabled !== false) return false
    if (filters.expiringSoon) {
      const days = daysUntil(u.expires_at)
      if (days === null || days > 7) return false
    }
    if (filters.nearQuota) {
      // 不限量的用户永远不算"接近上限":没有上限可接近。
      if (u.quota_bytes <= 0) return false
      if (u.used_total / u.quota_bytes < 0.8) return false
    }
    return true
  }),
)

function resetFilters() {
  filters.keyword = ''
  filters.tierID = undefined
  filters.status = undefined
  filters.loginEnabled = undefined
  filters.expiringSoon = false
  filters.nearQuota = false
}

const selectedIDs = ref<number[]>([])
const rowSelection = computed(() => ({
  selectedRowKeys: selectedIDs.value,
  onChange: (keys: (string | number)[]) => {
    selectedIDs.value = keys.map(Number)
  },
}))

const columns = [
  { title: '用户', key: 'name', width: 200 },
  { title: '访问等级', key: 'tier', width: 110 },
  { title: '登录账号', key: 'login', width: 150 },
  { title: '状态', key: 'status', width: 110 },
  { title: '节点', key: 'nodes', width: 110 },
  { title: '已用流量', key: 'used', width: 180 },
  { title: '到期时间', key: 'expires', width: 160 },
  { title: '最近续期', key: 'renewal', width: 120 },
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
  // 登录账号的格式在这里就拦住。放过去的话用户会被创建、登录账号不会,
  // 而用户拿账号去登录得到的是「账号或密码错误」—— 看起来完全就是密码打错了。
  if (editingId.value === null && form.login_username) {
    const bad = checkLoginUsername(form.login_username) ?? checkPassword(form.login_password)
    if (bad) {
      message.warning(bad)
      return
    }
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
          content:
            `${created.portal_account_error}\n\n` +
            '该用户现在还无法登录用户中心 —— 他拿账号去登录只会得到' +
            '「账号或密码错误」。请在用户详情里单独开通门户登录。',
          width: 520,
          okText: '知道了',
        })
      } else if (created.portal_account) {
        Modal.success({
          title: '用户已创建,门户登录已开通',
          content:
            `请把下面两项发给用户:\n\n登录地址:${portalLoginURL}\n` +
            `登录账号:${created.portal_account.username}\n\n` +
            '注意这不是面板首页 —— 首页是管理员登录页,用户在那里输账号会失败。\n' +
            '受影响节点将在数秒内自动部署。',
          width: 520,
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

// ---------- 续期与额度调整 ----------

const adjustOpen = ref(false)
const adjustSubmitting = ref(false)
// 非 null 表示调整单个用户;null 表示对当前勾选的用户批量调整。
const adjustTarget = ref<ProxyUser | null>(null)

const adjustForm = reactive({
  action: 'ADD_QUOTA' as AdjustAction,
  quota_gb: 10,
  expiry_days: 30,
  expires_at: '',
  access_tier_id: 1,
  remark: '',
})

const adjustActions: { value: AdjustAction; label: string }[] = [
  { value: 'ADD_QUOTA', label: '增加流量' },
  { value: 'SET_QUOTA', label: '设置流量额度' },
  { value: 'EXTEND_EXPIRY', label: '延长有效期' },
  { value: 'SET_EXPIRY', label: '设置到期时间' },
  { value: 'RESET_TRAFFIC', label: '重置已用流量' },
  { value: 'CHANGE_TIER', label: '调整访问等级' },
  { value: 'ENABLE_USER', label: '启用账号' },
  { value: 'DISABLE_USER', label: '停用账号' },
]

function openAdjust(user: ProxyUser | null) {
  adjustTarget.value = user
  adjustForm.action = 'ADD_QUOTA'
  adjustForm.quota_gb = 10
  adjustForm.expiry_days = 30
  adjustForm.expires_at = ''
  adjustForm.access_tier_id = user?.access_tier_id ?? 1
  adjustForm.remark = ''
  adjustOpen.value = true
}

function adjustPayload(): AdjustPayload {
  const body: AdjustPayload = { action: adjustForm.action, remark: adjustForm.remark }
  switch (adjustForm.action) {
    case 'ADD_QUOTA':
      body.quota_delta_bytes = Math.round(adjustForm.quota_gb * 1024 ** 3)
      break
    case 'SET_QUOTA':
      body.quota_bytes = Math.round(adjustForm.quota_gb * 1024 ** 3)
      break
    case 'EXTEND_EXPIRY':
      body.expiry_delta_days = adjustForm.expiry_days
      break
    case 'SET_EXPIRY':
      // 日期输入只有年月日,补成当天结束的 UTC 时刻;留空表示改为不过期。
      body.expires_at = adjustForm.expires_at ? `${adjustForm.expires_at}T23:59:59Z` : ''
      break
    case 'CHANGE_TIER':
      body.access_tier_id = adjustForm.access_tier_id
      break
  }
  return body
}

async function submitAdjust() {
  adjustSubmitting.value = true
  try {
    if (adjustTarget.value) {
      await api.adjustUser(adjustTarget.value.id, adjustPayload())
      message.success('已调整')
    } else {
      const result = await api.batchAdjust(selectedIDs.value, adjustPayload())
      if (result.succeeded === result.total) {
        message.success(`已处理 ${result.succeeded} 个用户`)
      } else {
        // 部分失败必须逐条说清楚是谁、为什么 —— 只报一个数字的话,
        // 管理员既不知道漏了谁,也不知道要不要重来一遍。
        const failed = result.items.filter((i) => !i.ok)
        Modal.warning({
          title: `${result.total} 个用户中 ${result.succeeded} 个成功`,
          content: failed
            .map((i) => `${userName(i.user_id)}:${i.error ?? '未知原因'}`)
            .join('\n'),
          width: 560,
          okText: '知道了',
        })
      }
      selectedIDs.value = []
    }
    adjustOpen.value = false
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '调整失败')
  } finally {
    adjustSubmitting.value = false
  }
}

function userName(id: number): string {
  return users.value.find((u) => u.id === id)?.display_name ?? `用户 ${id}`
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

    <div class="filters">
      <a-input-search
        v-model:value="filters.keyword"
        placeholder="名称 / 编号 / 备注 / 登录账号"
        allow-clear
        style="width: 240px"
      />
      <a-select
        v-model:value="filters.tierID"
        placeholder="访问等级"
        allow-clear
        style="width: 130px"
      >
        <a-select-option v-for="t in tiers" :key="t.id" :value="t.id">{{ t.name }}</a-select-option>
      </a-select>
      <a-select v-model:value="filters.status" placeholder="状态" allow-clear style="width: 140px">
        <a-select-option value="ACTIVE">正常</a-select-option>
        <a-select-option value="DISABLED">已停用</a-select-option>
        <a-select-option value="EXPIRED">已到期</a-select-option>
        <a-select-option value="QUOTA_EXCEEDED">流量用完</a-select-option>
      </a-select>
      <a-select
        v-model:value="filters.loginEnabled"
        placeholder="门户登录"
        allow-clear
        style="width: 130px"
      >
        <a-select-option value="yes">已启用</a-select-option>
        <a-select-option value="no">已停用</a-select-option>
        <a-select-option value="none">未开通</a-select-option>
      </a-select>
      <a-checkbox v-model:checked="filters.expiringSoon">7 天内到期</a-checkbox>
      <a-checkbox v-model:checked="filters.nearQuota">流量超 80%</a-checkbox>
      <a @click="resetFilters">重置</a>
      <span class="filter-count">{{ filteredUsers.length }} / {{ users.length }}</span>
    </div>

    <div v-if="selectedIDs.length > 0" class="batch-bar">
      <span>已选 {{ selectedIDs.length }} 个用户</span>
      <a-button size="small" type="primary" @click="openAdjust(null)">批量调整</a-button>
      <a @click="selectedIDs = []">取消选择</a>
    </div>

    <a-table
      :columns="columns"
      :data-source="filteredUsers"
      :loading="loading"
      :row-selection="rowSelection"
      row-key="id"
      size="middle"
      :pagination="false"
      :scroll="{ x: 1120 }"
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

        <template v-else-if="column.key === 'renewal'">
          <span class="muted">{{ record.last_renewal_at ? formatRelative(record.last_renewal_at) : '从未' }}</span>
        </template>

        <template v-else-if="column.key === 'actions'">
          <a-space size="small">
            <a @click="detailId = record.id">详情</a>
            <a @click="openEdit(record)">编辑</a>
            <a @click="openAdjust(record)">续期</a>
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
        <a-form-item
          label="登录账号"
          extra="留空表示该用户只用订阅,不登录用户中心"
          :validate-status="form.login_username && checkLoginUsername(form.login_username) ? 'error' : ''"
          :help="form.login_username ? checkLoginUsername(form.login_username) : undefined"
        >
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
          :validate-status="form.login_password && checkPassword(form.login_password) ? 'error' : ''"
          :help="form.login_password ? checkPassword(form.login_password) : undefined"
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

  <a-modal
    v-model:open="adjustOpen"
    :title="adjustTarget ? `续期 / 调整 · ${adjustTarget.display_name}` : `批量调整 ${selectedIDs.length} 个用户`"
    :confirm-loading="adjustSubmitting"
    ok-text="执行"
    cancel-text="取消"
    @ok="submitAdjust"
  >
    <a-form layout="vertical">
      <a-form-item label="操作">
        <a-select v-model:value="adjustForm.action" :options="adjustActions" />
      </a-form-item>

      <a-form-item
        v-if="adjustForm.action === 'ADD_QUOTA'"
        label="增加流量(GB)"
        extra="填负数表示扣减。不限量的用户请改用「设置流量额度」"
      >
        <a-input-number v-model:value="adjustForm.quota_gb" :step="1" style="width: 100%" />
      </a-form-item>
      <a-form-item
        v-else-if="adjustForm.action === 'SET_QUOTA'"
        label="流量额度(GB)"
        extra="填 0 表示不限量"
      >
        <a-input-number v-model:value="adjustForm.quota_gb" :min="0" :step="1" style="width: 100%" />
      </a-form-item>
      <a-form-item
        v-else-if="adjustForm.action === 'EXTEND_EXPIRY'"
        label="延长天数"
        extra="已过期的用户从今天起算,未到期的从原到期日起算"
      >
        <a-input-number v-model:value="adjustForm.expiry_days" :step="30" style="width: 100%" />
      </a-form-item>
      <a-form-item
        v-else-if="adjustForm.action === 'SET_EXPIRY'"
        label="到期时间"
        extra="留空表示改为不过期"
      >
        <a-input v-model:value="adjustForm.expires_at" type="date" style="width: 100%" />
      </a-form-item>
      <a-form-item v-else-if="adjustForm.action === 'CHANGE_TIER'" label="访问等级">
        <a-select v-model:value="adjustForm.access_tier_id">
          <a-select-option v-for="t in tiers" :key="t.id" :value="t.id">{{ t.name }}</a-select-option>
        </a-select>
      </a-form-item>
      <a-alert
        v-else-if="adjustForm.action === 'RESET_TRAFFIC'"
        type="info"
        message="已用流量清零,历史流水与节点计数器基线保持不变。此前因超额被停的用户会自动恢复。"
        class="adjust-hint"
      />

      <a-form-item label="备注" extra="这句话会展示给用户,不要写内部说明">
        <a-input v-model:value="adjustForm.remark" :maxlength="128" placeholder="例如:2026 年 8 月续费" />
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
.filters {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 12px;
  margin-bottom: 12px;
}

.filter-count {
  margin-left: auto;
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}

.batch-bar {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 12px;
  padding: 8px 12px;
  background: #e6f4ff;
  border-radius: 6px;
}

.adjust-hint {
  margin-bottom: 16px;
}

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
