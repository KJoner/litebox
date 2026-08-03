<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { message, Modal } from 'ant-design-vue'
import { portalApi, ApiError, type PortalSession } from '@/api/client'
import { formatRelative, formatTime } from '@/utils/format'
import { usePortalStore } from '@/stores/portal'

const portal = usePortalStore()
const router = useRouter()

const sessions = ref<PortalSession[]>([])
const loadingSessions = ref(false)
const submitting = ref(false)
const form = reactive({ old_password: '', new_password: '', confirm: '' })

async function loadSessions() {
  // 强制改密期间这个接口是被挡住的(403),不必去试 ——
  // 弹一个"没有权限"只会让用户困惑,他要做的就是先改密码。
  if (portal.identity?.must_change_password) return
  loadingSessions.value = true
  try {
    sessions.value = (await portalApi.sessions()).items
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载登录设备失败')
  } finally {
    loadingSessions.value = false
  }
}

async function changePassword() {
  if (form.new_password !== form.confirm) {
    message.warning('两次输入的新密码不一致')
    return
  }
  submitting.value = true
  try {
    await portalApi.changePassword(form.old_password, form.new_password)
    portal.passwordChanged()
    form.old_password = ''
    form.new_password = ''
    form.confirm = ''
    message.success('密码已修改,其他设备需要重新登录')
    await loadSessions()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '修改密码失败')
  } finally {
    submitting.value = false
  }
}

async function revoke(session: PortalSession) {
  try {
    await portalApi.revokeSession(session.id)
    message.success('已下线该设备')
    await loadSessions()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '操作失败')
  }
}

function logoutAll() {
  Modal.confirm({
    title: '退出全部设备',
    content: '包括当前这台设备在内的所有登录都会失效,需要重新登录。',
    okText: '确认退出',
    okType: 'danger',
    cancelText: '取消',
    onOk: async () => {
      try {
        await portalApi.logoutAll()
      } finally {
        portal.clear()
        await router.replace({ name: 'portal-login' })
      }
    },
  })
}

const columns = [
  { title: '设备', key: 'agent' },
  { title: 'IP', dataIndex: 'client_ip', key: 'ip', width: 140 },
  { title: '最近活动', key: 'last_seen', width: 130 },
  { title: '过期时间', key: 'expires', width: 160 },
  { title: '操作', key: 'actions', width: 90 },
]

onMounted(loadSessions)
</script>

<template>
  <a-alert
    v-if="portal.identity?.must_change_password"
    type="warning"
    show-icon
    class="card"
    message="请先修改初始密码"
    description="在修改密码之前,其余页面暂不可用。"
  />

  <a-card title="修改密码" class="card">
    <a-form layout="vertical" style="max-width: 420px" @submit.prevent="changePassword">
      <a-form-item label="当前密码">
        <a-input-password v-model:value="form.old_password" autocomplete="current-password" />
      </a-form-item>
      <a-form-item label="新密码" extra="至少 8 位">
        <a-input-password v-model:value="form.new_password" autocomplete="new-password" />
      </a-form-item>
      <a-form-item label="确认新密码">
        <a-input-password v-model:value="form.confirm" autocomplete="new-password" />
      </a-form-item>
      <a-button
        type="primary"
        :loading="submitting"
        :disabled="!form.old_password || form.new_password.length < 8 || !form.confirm"
        @click="changePassword"
      >
        修改密码
      </a-button>
    </a-form>
  </a-card>

  <a-card v-if="!portal.identity?.must_change_password" title="登录设备">
    <template #extra>
      <a-space>
        <a-button size="small" :loading="loadingSessions" @click="loadSessions">刷新</a-button>
        <a-button size="small" danger @click="logoutAll">退出全部设备</a-button>
      </a-space>
    </template>

    <a-table
      :columns="columns"
      :data-source="sessions"
      :loading="loadingSessions"
      row-key="id"
      size="small"
      :pagination="false"
      :scroll="{ x: 700 }"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'agent'">
          <span class="agent">{{ record.user_agent || '未知设备' }}</span>
          <a-tag v-if="record.current" color="green" class="tag">当前设备</a-tag>
        </template>
        <template v-else-if="column.key === 'last_seen'">
          {{ formatRelative(record.last_seen_at) }}
        </template>
        <template v-else-if="column.key === 'expires'">
          {{ formatTime(record.expires_at) }}
        </template>
        <template v-else-if="column.key === 'actions'">
          <!-- 当前设备不给"下线"按钮:点了等于自己把自己踢出去,
               而用户想做这件事时该用「退出全部设备」。 -->
          <a v-if="!record.current" class="danger" @click="revoke(record)">下线</a>
          <span v-else class="muted">—</span>
        </template>
      </template>
    </a-table>
  </a-card>
</template>

<style scoped>
.card {
  margin-bottom: 16px;
}

.agent {
  display: inline-block;
  max-width: 320px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  vertical-align: middle;
}

.tag {
  margin-left: 8px;
}

.danger {
  color: #cf1322;
}

.muted {
  color: rgb(0 0 0 / 45%);
}
</style>
