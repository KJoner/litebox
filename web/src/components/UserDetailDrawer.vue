<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { message, Modal } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AdjustmentRecord,
  type AuditLog,
  type Node,
  type ProxyUser,
  type UserTraffic,
} from '@/api/client'
import { formatBytes, formatQuota, formatRelative, formatTime } from '@/utils/format'
import { checkLoginUsername, checkPassword } from '@/utils/validate'
import StatusTag from '@/components/StatusTag.vue'
import TrafficChart from '@/components/TrafficChart.vue'

const props = defineProps<{ userId: number | null; nodes: Node[] }>()
const emit = defineEmits<{ close: []; changed: [] }>()

const user = ref<ProxyUser | null>(null)
const traffic = ref<UserTraffic | null>(null)
const logs = ref<AuditLog[]>([])
const adjustments = ref<AdjustmentRecord[]>([])
const loading = ref(false)
const revealUUID = ref(false)

// 门户登录账号。密码只在这里输入一次,提交后立刻清空 ——
// 不回显、不保留在组件状态里。
const accountOpen = ref(false)
const accountSubmitting = ref(false)

// 门户登录地址。取当前页面的 origin 而不是订阅用的 base_url ——
// 管理员正是在这个地址上操作的,它一定对;而 base_url 是给代理客户端用的,
// 完全可能是另一个域名。
//
// 之所以要把完整地址摆出来:管理员开通账号后要把地址发给用户,
// 手边没有就会顺手发面板首页,而那是管理员登录页 —— 用户在那儿输账号
// 只会得到「用户名或密码错误」,看起来完全就是密码发错了。
const portalLoginURL = `${window.location.origin}/user/login`
const accountForm = reactive({ username: '', password: '', must_change_password: true })

async function load(id: number) {
  loading.value = true
  revealUUID.value = false
  try {
    const u = await api.user(id)
    user.value = u
    const [t, l, adj] = await Promise.all([
      api.userTraffic(id, 30),
      api.auditLogs({ targetType: 'user', targetId: u.user_code, limit: 20 }),
      api.userAdjustments(id, 20),
    ])
    traffic.value = t
    logs.value = l.items
    adjustments.value = adj.items
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载用户详情失败')
  } finally {
    loading.value = false
  }
}

watch(
  () => props.userId,
  (id) => {
    if (id !== null) load(id)
    else {
      user.value = null
      traffic.value = null
      logs.value = []
      adjustments.value = []
    }
  },
  { immediate: true },
)

function nodeName(id: number): string {
  return props.nodes.find((n) => n.id === id)?.name ?? `节点 ${id}`
}

function openAccountForm() {
  accountForm.username = user.value?.portal_account?.username ?? ''
  accountForm.password = ''
  // 已有账号时默认不强制改密:管理员多半只是改个账号名。
  accountForm.must_change_password = !user.value?.portal_account
  accountOpen.value = true
}

async function submitAccount() {
  if (props.userId === null) return
  const badName = checkLoginUsername(accountForm.username)
  if (badName) {
    message.warning(badName)
    return
  }
  // 新建账号必须给密码;已有账号留空表示不改密码。
  const isNew = !user.value?.portal_account
  if (isNew || accountForm.password) {
    const badPassword = checkPassword(accountForm.password)
    if (badPassword) {
      message.warning(isNew ? `初始密码${badPassword}` : badPassword)
      return
    }
  }
  accountSubmitting.value = true
  const changedPassword = accountForm.password !== ''
  try {
    await api.setPortalAccount(props.userId, {
      username: accountForm.username,
      // 留空表示不改密码。这一点必须显式表达,不能让后端把空串当成新密码。
      password: accountForm.password || undefined,
      must_change_password: accountForm.must_change_password,
    })
    accountOpen.value = false
    const username = accountForm.username.trim().toLowerCase()
    accountForm.password = ''
    if (isNew) {
      // 新开通时用 Modal 而不是一闪而过的 toast:这一刻正是管理员要把
      // 地址与账号发给用户的时候,手边没有就会顺手发面板首页,
      // 而那是管理员登录页。
      Modal.success({
        title: '已开通用户中心登录',
        content: `请把下面两项发给用户:\n\n登录地址:${portalLoginURL}\n登录账号:${username}\n\n注意这不是面板首页 —— 首页是管理员登录页,用户在那里输账号会失败。`,
        width: 520,
        okText: '知道了',
      })
    } else {
      message.success(changedPassword ? '已保存,旧会话已全部失效' : '已保存')
    }
    emit('changed')
    await load(props.userId)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存登录账号失败')
  } finally {
    accountSubmitting.value = false
  }
}

async function toggleLogin() {
  if (props.userId === null || !user.value?.portal_account) return
  const enable = !user.value.portal_account.login_enabled
  try {
    await api.setPortalLoginEnabled(props.userId, enable)
    message.success(enable ? '已允许登录' : '已停用登录,在线会话已全部踢出')
    await load(props.userId)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '操作失败')
  }
}

function confirmRevokeSessions() {
  Modal.confirm({
    title: '撤销该用户的全部登录会话?',
    content: '所有已登录的设备都会被踢出,需要重新登录。代理连接不受影响。',
    okText: '撤销',
    okType: 'danger',
    cancelText: '取消',
    onOk: async () => {
      try {
        await api.revokePortalSessions(props.userId!)
        message.success('已撤销全部会话')
        await load(props.userId!)
      } catch (err) {
        message.error(err instanceof ApiError ? err.message : '操作失败')
      }
    },
  })
}

function confirmDeleteAccount() {
  Modal.confirm({
    title: '删除门户登录账号?',
    content: '该用户将无法再登录用户中心,但代理服务与订阅不受影响。',
    okText: '删除',
    okType: 'danger',
    cancelText: '取消',
    onOk: async () => {
      try {
        await api.deletePortalAccount(props.userId!)
        message.success('已删除登录账号')
        emit('changed')
        await load(props.userId!)
      } catch (err) {
        message.error(err instanceof ApiError ? err.message : '操作失败')
      }
    },
  })
}

async function copy(text: string, label: string) {
  try {
    await navigator.clipboard.writeText(text)
    message.success(`${label}已复制`)
  } catch {
    message.warning('浏览器拒绝了剪贴板访问,请手动复制')
  }
}

async function act(fn: () => Promise<ProxyUser>, successText: string) {
  try {
    user.value = await fn()
    message.success(successText)
    emit('changed')
    if (props.userId !== null) await load(props.userId)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '操作失败')
  }
}

function confirmResetTraffic() {
  Modal.confirm({
    title: '重置已用流量?',
    content: '用户累计流量将清零,历史流水与节点计数器基线保持不变。若此前因超额被停用,将自动恢复。',
    okText: '重置',
    cancelText: '取消',
    onOk: () => act(() => api.resetUserTraffic(props.userId!), '流量已重置'),
  })
}

function confirmRegenerateUUID() {
  Modal.confirm({
    title: '重新生成 UUID?',
    content: '用户当前的客户端将在节点重新部署后立即失效,需要重新导入订阅。',
    okText: '重新生成',
    okType: 'danger',
    cancelText: '取消',
    onOk: () => act(() => api.regenerateUserUUID(props.userId!), 'UUID 已重新生成'),
  })
}

function confirmRegenerateToken() {
  Modal.confirm({
    title: '重新生成订阅地址?',
    content: '旧订阅地址立即失效,需要把新地址重新发给用户。',
    okText: '重新生成',
    okType: 'danger',
    cancelText: '取消',
    onOk: () => act(() => api.regenerateSubToken(props.userId!), '订阅地址已重新生成'),
  })
}
</script>

<template>
  <a-drawer
    :open="userId !== null"
    :title="user ? `${user.display_name} · ${user.user_code}` : '用户详情'"
    width="640"
    @close="emit('close')"
  >
    <a-spin :spinning="loading">
      <template v-if="user">
        <a-descriptions :column="2" size="small" bordered>
          <a-descriptions-item label="状态">
            <StatusTag :status="user.status" kind="user" />
          </a-descriptions-item>
          <a-descriptions-item label="备注">{{ user.remark || '—' }}</a-descriptions-item>
          <a-descriptions-item label="已用流量">
            {{ formatBytes(user.used_total) }} / {{ formatQuota(user.quota_bytes) }}
          </a-descriptions-item>
          <a-descriptions-item label="到期时间">
            {{ user.expires_at ? formatTime(user.expires_at) : '不过期' }}
          </a-descriptions-item>
          <a-descriptions-item label="上行 / 下行">
            {{ formatBytes(user.used_uplink) }} / {{ formatBytes(user.used_downlink) }}
          </a-descriptions-item>
          <a-descriptions-item label="流量重置">
            {{ user.reset_cycle === 'MONTHLY' ? `每月 ${user.reset_day} 日` : '不重置' }}
          </a-descriptions-item>
          <a-descriptions-item label="创建时间">{{ formatTime(user.created_at) }}</a-descriptions-item>
          <a-descriptions-item label="订阅拉取">
            <span v-if="user.sub_access_count === 0">从未拉取</span>
            <span v-else>
              {{ user.sub_access_count }} 次 · {{ formatRelative(user.sub_last_access_at) }}
            </span>
          </a-descriptions-item>
        </a-descriptions>

        <div class="section-title">可用节点</div>
        <a-space v-if="user.effective_node_ids.length > 0" wrap>
          <a-tag
            v-for="id in user.effective_node_ids"
            :key="id"
            :color="user.node_ids.includes(id) ? 'blue' : undefined"
          >
            {{ nodeName(id) }}
          </a-tag>
        </a-space>
        <a-empty v-else description="没有可用节点" :image="undefined" />
        <div class="hint-line">
          蓝色为单独追加的授权,其余由访问等级「{{ user.access_tier_name }}」继承。
        </div>

        <div class="section-title">
          门户登录
          <a class="reveal" @click="openAccountForm">
            {{ user.portal_account ? '修改' : '开通' }}
          </a>
        </div>
        <a-descriptions v-if="user.portal_account" :column="2" size="small" bordered>
          <a-descriptions-item label="登录账号">
            {{ user.portal_account.username }}
          </a-descriptions-item>
          <a-descriptions-item label="登录状态">
            <a-tag :color="user.portal_account.login_enabled ? 'green' : 'red'">
              {{ user.portal_account.login_enabled ? '已启用' : '已停用' }}
            </a-tag>
            <a-tag v-if="user.portal_account.must_change_password" color="orange">
              待改初始密码
            </a-tag>
          </a-descriptions-item>
          <a-descriptions-item label="最后登录">
            {{ formatRelative(user.portal_account.last_login_at) }}
          </a-descriptions-item>
          <a-descriptions-item label="最后登录 IP">
            {{ user.portal_account.last_login_ip || '—' }}
          </a-descriptions-item>
          <a-descriptions-item label="在线会话">
            {{ user.portal_account.session_count }} 个
          </a-descriptions-item>
          <a-descriptions-item label="登录地址" :span="2">
            <a-input-group compact class="copy-row">
              <a-input :value="portalLoginURL" readonly style="width: calc(100% - 80px)" />
              <a-button @click="copy(portalLoginURL, '登录地址')">复制</a-button>
            </a-input-group>
            <div class="hint-line">
              这是用户中心的地址,不是面板首页 —— 首页是管理员登录页,
              用户在那里输账号只会得到「用户名或密码错误」。
            </div>
          </a-descriptions-item>
          <a-descriptions-item label="操作">
            <a-space size="small">
              <a @click="toggleLogin">
                {{ user.portal_account.login_enabled ? '停用登录' : '允许登录' }}
              </a>
              <a @click="confirmRevokeSessions">踢出全部</a>
              <a class="danger" @click="confirmDeleteAccount">删除账号</a>
            </a-space>
          </a-descriptions-item>
        </a-descriptions>
        <a-empty v-else description="未开通门户登录" :image="undefined" />

        <div class="section-title">订阅地址</div>
        <a-input-group compact class="copy-row">
          <a-input :value="user.subscription_url" readonly style="width: calc(100% - 80px)" />
          <a-button @click="copy(user.subscription_url ?? '', '订阅地址')">复制</a-button>
        </a-input-group>
        <div class="sub-formats">
          <a :href="`${user.subscription_url}?format=uri`" target="_blank">查看 VLESS 链接</a>
          <a-divider type="vertical" />
          <a :href="`${user.subscription_url}?format=sing-box`" target="_blank">sing-box 配置</a>
        </div>

        <div class="section-title">
          UUID
          <a class="reveal" @click="revealUUID = !revealUUID">
            {{ revealUUID ? '隐藏' : '显示' }}
          </a>
        </div>
        <a-input-group compact class="copy-row">
          <a-input
            :value="revealUUID ? user.uuid : '••••••••-••••-••••-••••-••••••••••••'"
            readonly
            style="width: calc(100% - 80px)"
          />
          <a-button @click="copy(user.uuid ?? '', 'UUID')">复制</a-button>
        </a-input-group>

        <div class="section-title">最近 30 天流量</div>
        <TrafficChart :data="traffic?.daily ?? []" />

        <div v-if="traffic && traffic.by_node.length > 0" class="by-node">
          <div v-for="n in traffic.by_node" :key="n.node_id" class="by-node-row">
            <span class="by-node-name">{{ n.node_name }}</span>
            <span class="by-node-value">{{ formatBytes(n.total) }}</span>
          </div>
        </div>

        <div class="section-title">续期与调整记录</div>
        <a-empty v-if="adjustments.length === 0" description="暂无调整记录" :image="undefined" />
        <a-table
          v-else
          :columns="[
            { title: '时间', key: 'time', width: 130 },
            { title: '操作', key: 'action', width: 110 },
            { title: '变化', key: 'delta', width: 110 },
            { title: '备注', key: 'remark' },
          ]"
          :data-source="adjustments"
          row-key="id"
          size="small"
          :pagination="false"
        >
          <template #bodyCell="{ column, record }">
            <template v-if="column.key === 'time'">
              <span class="log-time">{{ formatTime(record.created_at) }}</span>
            </template>
            <template v-else-if="column.key === 'action'">{{ record.action_text }}</template>
            <template v-else-if="column.key === 'delta'">
              <span v-if="record.quota_delta_bytes" class="tabular">
                {{ record.quota_delta_bytes > 0 ? '+' : '' }}{{ formatBytes(Math.abs(record.quota_delta_bytes)) }}
              </span>
              <span v-else-if="record.expiry_delta_days" class="tabular">
                {{ record.expiry_delta_days > 0 ? '+' : '' }}{{ record.expiry_delta_days }} 天
              </span>
              <span v-else class="muted">—</span>
            </template>
            <template v-else-if="column.key === 'remark'">
              {{ record.remark || '—' }}
            </template>
          </template>
        </a-table>

        <div class="section-title">最近操作记录</div>
        <a-empty v-if="logs.length === 0" description="暂无记录" :image="undefined" />
        <a-timeline v-else class="log-timeline">
          <a-timeline-item
            v-for="l in logs"
            :key="l.id"
            :color="l.succeeded ? 'green' : 'red'"
          >
            <div class="log-action">{{ l.action }}</div>
            <div class="log-detail">{{ l.detail || '—' }}</div>
            <div class="log-time">{{ formatTime(l.created_at) }}</div>
          </a-timeline-item>
        </a-timeline>
      </template>
    </a-spin>

    <template #footer>
      <a-space>
        <a-button @click="confirmResetTraffic">重置流量</a-button>
        <a-button @click="confirmRegenerateUUID">重新生成 UUID</a-button>
        <a-button @click="confirmRegenerateToken">重新生成订阅地址</a-button>
      </a-space>
    </template>
  </a-drawer>

  <a-modal
    v-model:open="accountOpen"
    :title="user?.portal_account ? '修改登录账号' : '开通门户登录'"
    :confirm-loading="accountSubmitting"
    ok-text="保存"
    cancel-text="取消"
    @ok="submitAccount"
  >
    <a-form layout="vertical">
      <a-form-item
        label="登录账号"
        required
        extra="字母、数字、下划线、连字符与点,3~32 位"
        :validate-status="accountForm.username && checkLoginUsername(accountForm.username) ? 'error' : ''"
        :help="accountForm.username ? checkLoginUsername(accountForm.username) : undefined"
      >
        <a-input v-model:value="accountForm.username" autocomplete="off" />
      </a-form-item>
      <a-form-item
        :label="user?.portal_account ? '新密码' : '初始密码'"
        :extra="
          user?.portal_account
            ? '留空表示不修改密码。填写后该用户的全部登录会话会立即失效'
            : '至少 8 位,请通过安全渠道发给用户'
        "
        :validate-status="accountForm.password && checkPassword(accountForm.password) ? 'error' : ''"
        :help="accountForm.password ? checkPassword(accountForm.password) : undefined"
      >
        <a-input-password v-model:value="accountForm.password" autocomplete="new-password" />
      </a-form-item>
      <a-form-item>
        <a-checkbox v-model:checked="accountForm.must_change_password">
          要求用户首次登录后修改密码
        </a-checkbox>
      </a-form-item>
    </a-form>
  </a-modal>
</template>

<style scoped>
.section-title {
  margin: 20px 0 8px;
  font-weight: 500;
  display: flex;
  align-items: center;
  gap: 8px;
}

.reveal {
  font-weight: 400;
  font-size: 12px;
}

.copy-row {
  display: flex;
}

.sub-formats {
  margin-top: 6px;
  font-size: 12px;
}

.tabular {
  font-variant-numeric: tabular-nums;
}

.muted {
  color: rgb(0 0 0 / 45%);
}

.hint-line {
  margin-top: 6px;
  font-size: 12px;
  color: rgb(0 0 0 / 45%);
}

.danger {
  color: #cf1322;
}

.by-node {
  margin-top: 12px;
}

.by-node-row {
  display: flex;
  justify-content: space-between;
  padding: 4px 0;
  border-bottom: 1px solid #f0f0f0;
  font-size: 13px;
}

.by-node-value {
  font-variant-numeric: tabular-nums;
}

.log-timeline {
  margin-top: 8px;
}

.log-action {
  font-size: 13px;
}

.log-detail {
  color: rgb(0 0 0 / 65%);
  font-size: 12px;
  word-break: break-all;
}

.log-time {
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
}
</style>
