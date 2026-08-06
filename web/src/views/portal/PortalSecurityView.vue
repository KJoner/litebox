<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import { portalApi, ApiError, type PortalSession } from '@/api/client'
import { usePortalStore } from '@/stores/portal'
import { LbEmptyState, LbTimeText, lbDangerConfirm } from '@/components/lb'

/**
 * 安全设置。强制改密时这是整个门户唯一可达的页面。
 *
 * 两处与原实现不同:
 *   确认框就地校验 —— 原来是提交时 message.warning('两次输入的新密码不一致'),
 *                     吐司飘在角上、输入框没有任何标记。
 *   「登录设备」整块不渲染 —— 强制改密期间那个接口返回 403,
 *                     去请求它只会换来一句「没有权限」。
 */
const portal = usePortalStore()
const router = useRouter()

const mustChange = computed(() => portal.identity?.must_change_password === true)

const sessions = ref<PortalSession[]>([])
const loadingSessions = ref(false)
const sessionError = ref('')

const form = reactive({ old_password: '', new_password: '', confirm: '' })
const submitting = ref(false)

/** 就地校验,不用等提交。 */
const newPwdHint = computed(() => {
  if (!form.new_password) return ''
  return form.new_password.length >= 8 ? `${form.new_password.length} 位 · 可以` : '至少 8 位'
})
const newPwdOK = computed(() => form.new_password.length >= 8)
const confirmError = computed(() =>
  form.confirm && form.confirm !== form.new_password ? '两次输入的新密码不一致' : '',
)
const canSubmit = computed(
  () => !!form.old_password && newPwdOK.value && !!form.confirm && !confirmError.value,
)

async function loadSessions() {
  if (mustChange.value) return
  loadingSessions.value = true
  sessionError.value = ''
  try {
    sessions.value = (await portalApi.sessions()).items
  } catch (err) {
    sessionError.value = err instanceof ApiError ? err.message : '暂时读不到登录设备'
    sessions.value = []
  } finally {
    loadingSessions.value = false
  }
}

onMounted(loadSessions)

async function changePassword() {
  if (!canSubmit.value) return
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

async function revoke(s: PortalSession) {
  try {
    await portalApi.revokeSession(s.id)
    message.success('已下线该设备')
    await loadSessions()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '操作失败')
  }
}

function logoutAll() {
  lbDangerConfirm({
    title: '退出全部设备?',
    okText: '退出全部',
    impacts: [
      '包括你现在这台设备,会立即回到登录页',
      '订阅地址不变,代理客户端不受影响',
      // 用户容易把「退出全部设备」误当成「重置密码」,看到自己被踢出去会以为账号出了问题。
      '密码不变,用原密码重新登录即可',
    ],
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

function deviceName(s: PortalSession): string {
  // 「未知设备」是没有上报 User-Agent 的客户端,不代表异常。
  return s.user_agent || '未知设备'
}
</script>

<template>
  <div class="pv">
    <div class="pv__head">
      <h2 class="pv__title">安全设置</h2>
      <div class="pv__sub">密码与登录设备</div>
    </div>

    <div v-if="mustChange" class="pv__force">
      <div class="pv__force-title">请先修改初始密码</div>
      <div class="pv__force-body">
        在改密完成之前,其余页面暂不可用 —— 初始口令还没换掉之前,不让它换到订阅地址。
      </div>
    </div>

    <section class="pv__card">
      <div class="pv__card-head"><span>修改密码</span></div>
      <div class="pv__card-body">
        <a-form layout="vertical" @submit.prevent="changePassword">
          <a-form-item :label="mustChange ? '当前密码(管理员给你的那个)' : '当前密码'" required>
            <a-input-password v-model:value="form.old_password" autocomplete="current-password" />
          </a-form-item>
          <a-form-item label="新密码" required>
            <a-input-password v-model:value="form.new_password" autocomplete="new-password" />
            <div class="pv__help" :class="{ 'pv__help--ok': newPwdOK }">
              {{ newPwdHint || '至少 8 位。' }}
            </div>
          </a-form-item>
          <!-- 就地校验:吐司飘在角上、输入框没有任何标记是最难注意到的那种错。 -->
          <a-form-item
            label="确认新密码"
            required
            :validate-status="confirmError ? 'error' : ''"
            :help="confirmError || undefined"
          >
            <a-input-password v-model:value="form.confirm" autocomplete="new-password" />
          </a-form-item>
        </a-form>

        <div class="pv__note">
          修改成功后,除当前设备外的全部登录都会失效,其他设备需要重新登录。
          订阅地址不受影响,客户端不用改。
        </div>

        <div>
          <a-button type="primary" :loading="submitting" :disabled="!canSubmit" @click="changePassword">
            {{ mustChange ? '修改并继续' : '修改密码' }}
          </a-button>
        </div>
      </div>
    </section>

    <!-- 强制改密期间整块不渲染:那个接口返回 403,请求它只会换来「没有权限」。 -->
    <section v-if="!mustChange" class="pv__card">
      <div class="pv__card-head">
        <span>登录设备 {{ sessions.length }} 台</span>
        <a-space size="small">
          <a-button size="small" :loading="loadingSessions" @click="loadSessions">刷新</a-button>
          <a-button size="small" danger @click="logoutAll">退出全部设备</a-button>
        </a-space>
      </div>
      <div class="pv__card-body">
        <LbEmptyState v-if="sessionError" variant="error" :title="sessionError" @retry="loadSessions" />
        <LbEmptyState
          v-else-if="!loadingSessions && sessions.length === 0"
          variant="empty"
          title="没有其他登录记录"
        />
        <!-- 三列表在窄屏下换成列表块,与「按节点」同一条规则。 -->
        <div v-else class="pv__sessions">
          <div v-for="s in sessions" :key="s.id" class="pv__session">
            <div class="pv__session-head">
              <span class="lb-ellipsis" :title="deviceName(s)">{{ deviceName(s) }}</span>
              <span v-if="s.current" class="pv__session-cur">当前设备</span>
            </div>
            <div class="pv__session-meta lb-mono">
              {{ s.client_ip || '—' }} · 最近活动 <LbTimeText :value="s.last_seen_at" /> · 过期
              {{ s.expires_at.slice(0, 10) }}
            </div>
            <div class="pv__session-act">
              <!-- 当前设备不给「下线」:点它等于自己把自己踢出去,
                   真想这么做时该用的是「退出全部设备」。 -->
              <span v-if="s.current" class="pv__muted">—</span>
              <a v-else @click="revoke(s)">下线</a>
            </div>
          </div>
        </div>

        <div class="pv__note">
          「未知设备」是没有上报客户端标识的连接,不代表异常。
          如果这里出现你不认识的 IP,改密码即可 —— 改密会让其余全部会话立即失效。
        </div>
      </div>
    </section>
  </div>
</template>

<style scoped>
.pv {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.pv__head {
  display: flex;
  flex-direction: column;
  gap: 3px;
}

.pv__title {
  margin: 0;
  font-size: 19px;
  font-weight: 600;
}

.pv__sub {
  font-size: 12.5px;
  color: #6b7480;
}

.pv__force {
  padding: 12px 14px;
  background: #fcf3e3;
  border: 1px solid #efdcb4;
  border-radius: 8px;
}

.pv__force-title {
  font-size: 13px;
  font-weight: 600;
  color: #5c4405;
}

.pv__force-body {
  margin-top: 5px;
  font-size: 12.5px;
  line-height: 1.8;
  color: #5c4405;
}

.pv__card {
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

.pv__card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  padding: 12px 16px;
  border-bottom: 1px solid #edeff2;
  font-size: 13px;
  font-weight: 600;
}

.pv__card-body {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 16px;
}

.pv__card-body :deep(.ant-form-item) {
  margin-bottom: 14px;
}

.pv__help {
  margin-top: 4px;
  font-size: 12px;
  color: #6b7480;
}

.pv__help--ok {
  color: #1b7a4b;
}

.pv__note {
  padding: 11px 13px;
  background: #f6f7f9;
  border: 1px solid #edeff2;
  border-radius: 6px;
  font-size: 12px;
  line-height: 1.8;
  color: #576070;
}

.pv__sessions {
  border: 1px solid #edeff2;
  border-radius: 6px;
  overflow: hidden;
}

.pv__session {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: 4px 12px;
  padding: 11px 13px;
}

.pv__session + .pv__session {
  border-top: 1px solid #edeff2;
}

.pv__session-head {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
  font-size: 12.5px;
}

.pv__session-cur {
  flex: none;
  padding: 1px 6px;
  background: #eef4fc;
  border: 1px solid #c9dcf3;
  border-radius: 3px;
  font-size: 10.5px;
  color: #1d4f96;
}

.pv__session-meta {
  grid-column: 1;
  font-size: 11px;
  color: #6b7480;
}

.pv__session-act {
  grid-row: 1 / 3;
  grid-column: 2;
  align-self: center;
  font-size: 12px;
}

.pv__muted {
  color: #6b7480;
}
</style>
