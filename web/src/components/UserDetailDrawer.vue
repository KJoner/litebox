<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { message, Modal } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AccessTier,
  type AdjustAction,
  type AdjustmentRecord,
  type AuditLog,
  type Node,
  type ProxyUser,
  type UserTraffic,
} from '@/api/client'
import { formatBytes, formatTime } from '@/utils/format'
import { checkLoginUsername, checkPassword } from '@/utils/validate'
import {
  LbCopyField,
  LbEmptyState,
  LbNameConfirm,
  LbQuotaBar,
  LbSparkline,
  LbStatusTag,
  LbTimeText,
  lbDangerConfirm,
  type LbPoint,
} from '@/components/lb'
import {
  daysUntil,
  isExpiringSoon,
  isNearQuota,
  primaryUserAction,
  userActionLabel,
} from '@/components/lb/derive'
import UserAdjustModal from '@/components/user/UserAdjustModal.vue'
import { color } from '@/theme/tokens'

/**
 * 用户详情。Drawer 720 而不是独立路由页 ——
 * 详情大多是「看一眼就回列表」,整页跳转反而多一次返回。
 *
 * 三条贯穿全篇的规则:
 *   一、抽屉立即打开并显示骨架,不等数据回来再开。点了没反应比慢更难受;
 *       标题区先用列表行里已有的名字填上(preview)。
 *   二、四块内容各自降级。调整记录读不到不该让整个抽屉变成错误页 ——
 *       那会让人以为用户档案也出了问题。
 *   三、门户账号的措辞:login_enabled=false 一律叫「门户登录已关闭」,
 *       「已停用」只留给整个账号(status DISABLED)。一个词只指一件事。
 */
const props = defineProps<{
  userId: number | null
  nodes: Node[]
  tiers: AccessTier[]
  /** 列表行里已有的那一份,用来在数据回来之前把标题填上 */
  preview?: ProxyUser | null
}>()
const emit = defineEmits<{ close: []; changed: []; edit: [user: ProxyUser] }>()

const user = ref<ProxyUser | null>(null)
const loading = ref(false)
/** 用户本身读不到 —— 这才是整个抽屉的错误态。 */
const loadError = ref<{ message: string; status?: number; at: string } | null>(null)
const tab = ref('profile')

// 三块附属数据各自持有加载与失败状态,互不牵连。
const traffic = ref<UserTraffic | null>(null)
const trafficError = ref(false)
const adjustments = ref<AdjustmentRecord[]>([])
const adjustError = ref(false)
const logs = ref<AuditLog[]>([])
const logError = ref(false)

/** 标题区在数据回来之前用的占位。 */
const head = computed(() => user.value ?? props.preview ?? null)

// 门户登录地址就是面板首页。取当前页面的 origin 而不是订阅用的 base_url ——
// 管理员正是在这个地址上操作的,它一定对;而 base_url 是给代理客户端用的,
// 完全可能是另一个域名。
const portalLoginURL = window.location.origin

async function load(id: number) {
  loading.value = true
  loadError.value = null
  try {
    user.value = await api.user(id)
  } catch (err) {
    loadError.value = {
      message: err instanceof ApiError ? err.message : '加载用户详情失败',
      status: err instanceof ApiError ? err.status : undefined,
      at: new Date().toLocaleTimeString(),
    }
    user.value = null
    loading.value = false
    return
  }
  loading.value = false
  loadSections(user.value)
}

/** 附属数据。每块单独 catch —— 一块读不到不影响其余三块。 */
function loadSections(u: ProxyUser) {
  trafficError.value = false
  adjustError.value = false
  logError.value = false

  api
    .userTraffic(u.id, 30)
    .then((t) => (traffic.value = t))
    .catch(() => {
      traffic.value = null
      trafficError.value = true
    })
  api
    .userAdjustments(u.id, 50)
    .then((r) => (adjustments.value = r.items))
    .catch(() => {
      adjustments.value = []
      adjustError.value = true
    })
  api
    .auditLogs({ targetType: 'user', targetId: u.user_code, limit: 20 })
    .then((r) => (logs.value = r.items))
    .catch(() => {
      logs.value = []
      logError.value = true
    })
}

watch(
  () => props.userId,
  (id) => {
    if (id !== null) {
      tab.value = 'profile'
      user.value = null
      traffic.value = null
      adjustments.value = []
      logs.value = []
      load(id)
    }
  },
  { immediate: true },
)

function reload() {
  if (props.userId !== null) load(props.userId)
}

// ---------- 派生展示 ----------

const nodeLabel = (id: number) => props.nodes.find((n) => n.id === id)?.display_name
  ?? props.nodes.find((n) => n.id === id)?.name
  ?? `节点 ${id}`

const warningLevel = computed(() => {
  const u = user.value
  if (!u) return undefined
  if (u.status === 'QUOTA_EXCEEDED') return 'EXCEEDED' as const
  if (u.quota_bytes <= 0) return 'UNLIMITED' as const
  return isNearQuota(u) ? ('WARNING' as const) : ('NORMAL' as const)
})

/**
 * 顶部横幅。只出一条,按严重程度取第一个命中的 ——
 * 三条警告叠在一起等于一条都没有。
 */
const banner = computed(() => {
  const u = user.value
  if (!u) return null
  const reset = u.next_reset_at
    ? `额度将在 ${formatUTC(u.next_reset_at)} 重置。`
    : '该用户的流量不自动重置。'
  if (u.status === 'QUOTA_EXCEEDED') {
    return {
      type: 'error' as const,
      text: `流量已用满,账号已自动停用并触发受影响节点重新部署,用户现在连不上。${reset}`,
    }
  }
  if (u.status === 'EXPIRED') {
    return { type: 'error' as const, text: '已过期,凭据已从各节点移除。续期后自动恢复,订阅地址不变。' }
  }
  if (u.status === 'DISABLED') {
    return { type: 'warning' as const, text: '账号已停用。门户仍可登录,但看不到订阅地址;历史流量与调整记录保留。' }
  }
  if (isNearQuota(u)) {
    const pct = Math.round((u.used_total / u.quota_bytes) * 100)
    return {
      type: 'warning' as const,
      text: `已用 ${pct}% 额度。达到 100% 时账号会被自动停用并触发受影响节点重新部署,用户届时会连不上。${reset}`,
    }
  }
  if (isExpiringSoon(u)) {
    return { type: 'warning' as const, text: `${daysUntil(u.expires_at)} 天后到期。到期后凭据会从各节点移除。` }
  }
  return null
})

function formatUTC(iso: string): string {
  const d = new Date(iso)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())} ${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())} UTC`
}

/** 缺失的日子传 null,不补 0 —— 补 0 会把「那天没同步」画成「那天没人用」。 */
const dailyPoints = computed<LbPoint[]>(() => {
  const byDay = new Map(traffic.value?.daily.map((d) => [d.day, d.total]) ?? [])
  const out: LbPoint[] = []
  for (let i = 29; i >= 0; i--) {
    const d = new Date(Date.now() - i * 86400000)
    const key = d.toISOString().slice(0, 10)
    out.push({ at: key, value: byDay.has(key) ? (byDay.get(key) as number) : null })
  }
  return out
})

// ---------- 门户账号 ----------

const accountOpen = ref(false)
const accountSubmitting = ref(false)
const accountForm = reactive({ username: '', password: '', must_change_password: true })

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
      // 留空表示不改密码。必须显式表达,不能让后端把空串当成新密码。
      password: accountForm.password || undefined,
      must_change_password: accountForm.must_change_password,
    })
    accountOpen.value = false
    const username = accountForm.username.trim().toLowerCase()
    // 口令只在这一次请求里用到,立刻从组件状态里抹掉。
    accountForm.password = ''
    if (isNew) {
      // 这一刻正是管理员要把地址与账号发给用户的时候,手边没有就会顺手发
      // 面板首页 —— 用一条三秒吐司交付等于让他回来再翻一次。
      Modal.success({
        title: '已开通用户中心登录',
        width: 520,
        content: `请把下面两项发给用户:\n\n登录地址:${portalLoginURL}\n登录账号:${username}`,
        okText: '知道了',
      })
    } else {
      message.success(changedPassword ? '已保存,该账号的全部会话已失效' : '已保存')
    }
    emit('changed')
    reload()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存登录账号失败')
  } finally {
    accountSubmitting.value = false
  }
}

async function toggleLogin() {
  const acct = user.value?.portal_account
  if (props.userId === null || !acct) return
  const enable = !acct.login_enabled
  try {
    await api.setPortalLoginEnabled(props.userId, enable)
    message.success(enable ? '已开启门户登录' : '已关闭门户登录,在线会话已全部踢出')
    emit('changed')
    reload()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '操作失败')
  }
}

function confirmRevokeSessions() {
  lbDangerConfirm({
    title: '撤销该用户的全部登录会话?',
    okText: '撤销',
    impacts: [
      '所有已登录的设备都会被踢出,需要重新登录',
      '密码不变,用原密码即可重新登录',
      '代理连接与订阅地址不受影响',
    ],
    onOk: async () => {
      try {
        await api.revokePortalSessions(props.userId!)
        message.success('已撤销全部会话')
        reload()
      } catch (err) {
        message.error(err instanceof ApiError ? err.message : '操作失败')
      }
    },
  })
}

function confirmDeleteAccount() {
  lbDangerConfirm({
    title: '删除门户登录账号?',
    okText: '删除',
    impacts: [
      '该用户将无法再登录用户中心',
      '代理服务与订阅地址不受影响,客户端照常可用',
      '之后可以重新开通,但要重设一次账号与初始密码',
    ],
    onOk: async () => {
      try {
        await api.deletePortalAccount(props.userId!)
        message.success('已删除登录账号')
        emit('changed')
        reload()
      } catch (err) {
        message.error(err instanceof ApiError ? err.message : '操作失败')
      }
    },
  })
}

// ---------- 危险动作 ----------

async function act(fn: () => Promise<ProxyUser>, successText: string) {
  try {
    await fn()
    message.success(successText)
    emit('changed')
    reload()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '操作失败')
  }
}

function confirmRegenerateUUID() {
  lbDangerConfirm({
    title: `重新生成 ${user.value?.display_name} 的 UUID?`,
    okText: '重新生成',
    impacts: [
      '该用户当前的客户端在节点重新部署后立即失效',
      '需要重新导入订阅才能恢复',
      '订阅地址本身不变',
    ],
    onOk: () => act(() => api.regenerateUserUUID(props.userId!), 'UUID 已重新生成'),
  })
}

function confirmRegenerateToken() {
  lbDangerConfirm({
    title: `重新生成 ${user.value?.display_name} 的订阅地址?`,
    okText: '重新生成',
    impacts: ['旧地址立即失效', '该用户全部客户端需重新导入', '节点侧凭据不变,无需部署'],
    onOk: () => act(() => api.regenerateSubToken(props.userId!), '订阅地址已重新生成,请通知用户重新导入'),
  })
}

const deleteOpen = ref(false)
const deleteLoading = ref(false)

async function doDelete() {
  if (props.userId === null) return
  deleteLoading.value = true
  try {
    await api.deleteUser(props.userId)
    message.success('已删除')
    deleteOpen.value = false
    emit('changed')
    emit('close')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '删除失败')
  } finally {
    deleteLoading.value = false
  }
}

// ---------- 调整弹窗 ----------

const adjustOpen = ref(false)
const adjustAction = ref<AdjustAction>('EXTEND_EXPIRY')

function openAdjust(action: AdjustAction) {
  adjustAction.value = action
  adjustOpen.value = true
}

/** 头部主操作与列表行的主操作同一套判定,免得两处给出不同的建议。 */
function runPrimary() {
  const u = user.value
  if (!u) return
  switch (primaryUserAction(u)) {
    case 'renew':
      return openAdjust('EXTEND_EXPIRY')
    case 'addQuota':
      return openAdjust('ADD_QUOTA')
    case 'enable':
      return openAdjust('ENABLE_USER')
    case 'assignNode':
      return emit('edit', u)
    default:
      return openAdjust('EXTEND_EXPIRY')
  }
}

const primaryLabel = computed(() =>
  user.value ? userActionLabel[primaryUserAction(user.value)] : '续期',
)

const adjustColumns = [
  { title: '时间', key: 'time', width: 140 },
  { title: '操作', key: 'action', width: 110 },
  { title: '变化', key: 'delta', width: 110 },
  { title: '备注(用户可见)', key: 'remark' },
]
</script>

<template>
  <a-drawer
    :open="userId !== null"
    :width="720"
    :body-style="{ padding: '0 20px 20px' }"
    @close="emit('close')"
  >
    <template #title>
      <div class="ud__head">
        <div class="ud__title">
          <span class="ud__name">{{ head?.display_name ?? '用户详情' }}</span>
          <LbStatusTag v-if="head" kind="user" :status="head.status" />
          <a-tag v-if="head">{{ head.access_tier_name }}</a-tag>
        </div>
        <div v-if="head" class="ud__sub lb-mono">
          {{ head.user_code }} · 创建于 {{ formatTime(head.created_at) }} ·
          <template v-if="user?.last_renewal_at">
            最近续期 <LbTimeText :value="user.last_renewal_at" />
          </template>
          <template v-else>从未续期</template>
        </div>
      </div>
    </template>

    <template #extra>
      <a-space v-if="user">
        <a-button type="primary" size="small" @click="runPrimary">{{ primaryLabel }}</a-button>
        <a-button size="small" @click="emit('edit', user)">编辑</a-button>
        <a-dropdown placement="bottomRight">
          <a-button size="small" :aria-label="`${user.display_name} 的更多操作`" title="更多操作">⋯</a-button>
          <template #overlay>
            <a-menu>
              <a-menu-item @click="openAdjust('RESET_TRAFFIC')">重置已用流量</a-menu-item>
              <a-menu-item @click="openAdjust('CHANGE_TIER')">调整访问等级</a-menu-item>
              <a-menu-item @click="confirmRegenerateToken">重新生成订阅地址</a-menu-item>
              <a-menu-item @click="confirmRegenerateUUID">重新生成 UUID</a-menu-item>
              <a-menu-divider />
              <a-menu-item danger @click="deleteOpen = true">删除用户</a-menu-item>
            </a-menu>
          </template>
        </a-dropdown>
      </a-space>
    </template>

    <!-- 用户本身读不到 —— 整个抽屉进错误态。附属数据的失败不走这里。 -->
    <LbEmptyState
      v-if="loadError"
      variant="error"
      :title="loadError.status === 404 ? '用户不存在或已被删除' : loadError.message"
      description="列表可能已经过期。关闭抽屉会自动刷新一次列表。"
      :http-status="loadError.status"
      :occurred-at="loadError.at"
      @retry="reload"
    >
      <template #action>
        <a-button
          size="small"
          type="primary"
          @click="
            () => {
              emit('changed')
              emit('close')
            }
          "
        >
          关闭并刷新
        </a-button>
      </template>
    </LbEmptyState>

    <!-- 骨架保留版面,不整页转圈 —— 数据到位时不发生跳动。 -->
    <div v-else-if="loading || !user" class="ud__skel">
      <a-skeleton active :paragraph="{ rows: 3 }" />
      <a-skeleton active :paragraph="{ rows: 4 }" />
    </div>

    <template v-else>
      <a-alert
        v-if="banner"
        class="ud__banner"
        :type="banner.type"
        show-icon
        :message="banner.text"
      />

      <a-tabs v-model:activeKey="tab" size="small">
        <a-tab-pane key="profile" tab="档案">
          <div class="ud__grid">
            <section class="ud__card">
              <div class="ud__card-head">流量与有效期</div>
              <div class="ud__card-body">
                <LbQuotaBar
                  :used-bytes="user.used_total"
                  :quota-bytes="user.quota_bytes"
                  :warning-level="warningLevel"
                  size="md"
                />
                <div class="ud__facts">
                  <div><span>上行</span><b class="lb-mono">{{ formatBytes(user.used_uplink) }}</b></div>
                  <div><span>下行</span><b class="lb-mono">{{ formatBytes(user.used_downlink) }}</b></div>
                  <div>
                    <span>到期时间</span>
                    <b class="lb-mono" :style="{ color: user.status === 'EXPIRED' ? color.danger : undefined }">
                      {{ user.expires_at ? user.expires_at.slice(0, 10) : '不过期' }}
                    </b>
                  </div>
                  <div>
                    <span>下次重置</span>
                    <!-- 后端算好的时刻。前端不自己推 —— 门户上给用户看的是同一份。 -->
                    <b class="lb-mono">
                      <LbTimeText v-if="user.next_reset_at" :value="user.next_reset_at" mode="cycle" />
                      <template v-else>不重置</template>
                    </b>
                  </div>
                </div>
                <div class="ud__spark">
                  <LbSparkline :points="dailyPoints" type="bar" :height="72" />
                  <div class="ud__spark-cap">近 30 天 · 按 UTC 日 · 空心柱表示当天没有记录,不是 0</div>
                </div>
              </div>
            </section>

            <section class="ud__card">
              <div class="ud__card-head">
                门户登录账号
                <a-space size="small">
                  <a v-if="user.portal_account" @click="openAccountForm">重设密码</a>
                  <a v-if="user.portal_account" @click="toggleLogin">
                    {{ user.portal_account.login_enabled ? '关闭门户登录' : '开启门户登录' }}
                  </a>
                  <a v-else @click="openAccountForm">开通</a>
                </a-space>
              </div>
              <div v-if="user.portal_account" class="ud__card-body">
                <div class="ud__facts">
                  <div><span>登录账号</span><b class="lb-mono">{{ user.portal_account.username }}</b></div>
                  <div>
                    <span>登录状态</span>
                    <b>
                      <LbStatusTag
                        v-if="user.portal_account.must_change_password"
                        :meta="{ text: '待改初始密码', shape: 'triangle', fg: '#92610A', bg: '#FCF3E3', bd: '#EFDCB4' }"
                      />
                      <!-- login_enabled=false 全站统称「门户登录已关闭」,不叫「已停用」 -->
                      <LbStatusTag
                        v-else-if="!user.portal_account.login_enabled"
                        :meta="{ text: '门户登录已关闭', shape: 'pause', fg: '#5F52A0', bg: '#F0EEF9', bd: '#D6D0EE' }"
                      />
                      <LbStatusTag v-else kind="user" status="ACTIVE" />
                    </b>
                  </div>
                  <div>
                    <span>最后登录</span>
                    <b><LbTimeText :value="user.portal_account.last_login_at" empty="从未登录" /></b>
                  </div>
                  <div><span>在线会话</span><b class="lb-mono">{{ user.portal_account.session_count }}</b></div>
                </div>

                <LbCopyField :value="portalLoginURL" label="登录地址" button-text="复制" />

                <div v-if="user.portal_account.must_change_password" class="ud__note ud__note--info">
                  该用户尚未改过初始密码。在他改密之前,订阅地址与节点信息在门户上都不会显示 ——
                  初始口令还没换掉之前,不让它换到任何有价值的东西。
                </div>

                <div class="ud__acct-ops">
                  <a @click="confirmRevokeSessions">踢出全部会话</a>
                  <a class="ud__danger" @click="confirmDeleteAccount">删除登录账号</a>
                </div>
              </div>
              <div v-else class="ud__card-body">
                <div class="ud__note">
                  未开通门户登录。该用户只能用订阅地址,看不到流量、节点与到期时间。
                </div>
              </div>
            </section>

            <section class="ud__card">
              <div class="ud__card-head">凭据</div>
              <div class="ud__card-body">
                <LbCopyField
                  :value="user.subscription_url ?? ''"
                  label="订阅地址"
                  caution="等同于密码,勿转发"
                  middle-ellipsis
                />
                <!-- 技术串中段省略:7f3a…c91d 还能人工比对,7f3a2b1c… 不能。 -->
                <LbCopyField :value="user.uuid ?? ''" label="UUID" middle-ellipsis />
                <div class="ud__facts">
                  <div>
                    <span>订阅拉取</span>
                    <b class="lb-mono">
                      <template v-if="user.sub_access_count === 0">从未拉取</template>
                      <template v-else>
                        {{ user.sub_access_count }} 次 · <LbTimeText :value="user.sub_last_access_at" />
                      </template>
                    </b>
                  </div>
                  <div class="ud__facts-wide">
                    <span>最近客户端</span>
                    <b class="lb-ellipsis" :title="user.sub_last_user_agent">
                      {{ user.sub_last_user_agent || '—' }}
                      <template v-if="user.sub_last_access_ip"> · {{ user.sub_last_access_ip }}</template>
                    </b>
                  </div>
                </div>
              </div>
            </section>

            <section class="ud__card">
              <div class="ud__card-head">可用节点 {{ user.effective_node_ids.length }}</div>
              <div class="ud__card-body">
                <div v-if="user.effective_node_ids.length" class="ud__nodes">
                  <span v-for="id in user.effective_node_ids" :key="id" class="ud__node">
                    {{ nodeLabel(id) }}
                    <em>{{ user.node_ids.includes(id) ? '额外授权' : '等级继承' }}</em>
                  </span>
                </div>
                <div v-else class="ud__note ud__note--danger">
                  一个可用节点都没有。等级「{{ user.access_tier_name }}」下没有节点,
                  额外授权也是空的 —— 该用户拿到订阅也连不上任何东西。
                </div>
              </div>
            </section>
          </div>
        </a-tab-pane>

        <a-tab-pane key="traffic" tab="流量">
          <LbEmptyState
            v-if="trafficError"
            variant="error"
            title="流量数据暂时读不到"
            description="档案与调整记录正常,此处不代表「没有流量」。"
            @retry="user && loadSections(user)"
          />
          <template v-else>
            <LbSparkline :points="dailyPoints" type="bar" :height="140" />
            <div class="ud__spark-cap">
              近 30 天 · 按 UTC 日聚合 · 空心柱表示当天没有记录(不补 0、不插值)
            </div>

            <div class="ud__card-head ud__card-head--plain">按节点</div>
            <div v-if="traffic && traffic.by_node.length" class="ud__bynode">
              <div v-for="n in traffic.by_node" :key="n.node_id" class="ud__bynode-row">
                <span class="lb-ellipsis">{{ n.node_name }}</span>
                <span class="lb-mono">{{ formatBytes(n.total) }}</span>
                <span class="lb-mono ud__bynode-dir">
                  ↑ {{ formatBytes(n.uplink) }} · ↓ {{ formatBytes(n.downlink) }}
                </span>
              </div>
            </div>
            <div v-else class="ud__note">该用户在任何节点上都还没有产生流量。</div>
          </template>
        </a-tab-pane>

        <a-tab-pane key="adjust" tab="调整记录">
          <LbEmptyState
            v-if="adjustError"
            variant="error"
            title="调整记录暂时读不到"
            description="档案与流量数据正常,此处不代表「没有调整过」。"
            @retry="user && loadSections(user)"
          />
          <LbEmptyState
            v-else-if="adjustments.length === 0"
            variant="empty"
            title="还没有调整记录"
            description="续期、加流量、改等级都会记在这里,备注会显示给用户。"
          />
          <a-table
            v-else
            :columns="adjustColumns"
            :data-source="adjustments"
            row-key="id"
            size="small"
            :pagination="{ pageSize: 10, size: 'small', hideOnSinglePage: true, showSizeChanger: false }"
          >
            <template #bodyCell="{ column, record }">
              <template v-if="column.key === 'time'">
                <LbTimeText :value="record.created_at" mode="both" />
              </template>
              <template v-else-if="column.key === 'action'">{{ record.action_text }}</template>
              <template v-else-if="column.key === 'delta'">
                <span v-if="record.quota_delta_bytes" class="lb-mono">
                  {{ record.quota_delta_bytes > 0 ? '+' : '−'
                  }}{{ formatBytes(Math.abs(record.quota_delta_bytes)) }}
                </span>
                <span v-else-if="record.expiry_delta_days" class="lb-mono">
                  {{ record.expiry_delta_days > 0 ? '+' : '' }}{{ record.expiry_delta_days }} 天
                </span>
                <span v-else class="ud__muted">—</span>
              </template>
              <template v-else-if="column.key === 'remark'">
                <span v-if="record.remark" class="lb-clamp-2">{{ record.remark }}</span>
                <span v-else class="ud__muted">—</span>
              </template>
            </template>
          </a-table>
        </a-tab-pane>

        <a-tab-pane key="audit" tab="审计">
          <LbEmptyState
            v-if="logError"
            variant="error"
            title="审计记录暂时读不到"
            @retry="user && loadSections(user)"
          />
          <LbEmptyState
            v-else-if="logs.length === 0"
            variant="empty"
            title="还没有针对该用户的操作记录"
            description="日志从面板首次启动时开始记录。"
          />
          <div v-else class="ud__logs">
            <div v-for="l in logs" :key="l.id" class="ud__log">
              <LbStatusTag
                :meta="
                  l.succeeded
                    ? { text: '成功', shape: 'check', fg: '#1B7A4B', bg: '#E9F5EE', bd: '#C3E3D0' }
                    : { text: '失败', shape: 'cross', fg: '#B4291D', bg: '#FDECEA', bd: '#F3CFC9' }
                "
              />
              <div class="ud__log-body">
                <!-- 英文常量收进 title:管理员不看代码,每行却要多占 15px。 -->
                <div class="ud__log-action" :title="l.action">{{ l.action }}</div>
                <div class="ud__log-detail lb-clamp-2">{{ l.detail || '—' }}</div>
              </div>
              <LbTimeText :value="l.created_at" mode="both" />
            </div>
          </div>
        </a-tab-pane>
      </a-tabs>
    </template>
  </a-drawer>

  <!-- 确认框盖在抽屉之上且抽屉不关。不允许套娃:确认框里不再开第二个确认框。 -->
  <a-modal
    v-model:open="accountOpen"
    :title="user?.portal_account ? '重设登录密码' : '开通门户登录'"
    :width="460"
    :confirm-loading="accountSubmitting"
    ok-text="保存"
    cancel-text="取消"
    :mask-closable="false"
    @ok="submitAccount"
  >
    <a-form layout="vertical">
      <a-form-item
        label="登录账号"
        required
        :validate-status="accountForm.username && checkLoginUsername(accountForm.username) ? 'error' : ''"
        :help="
          accountForm.username
            ? checkLoginUsername(accountForm.username) ?? undefined
            : '字母、数字、下划线、连字符与点,3~32 位'
        "
      >
        <a-input v-model:value="accountForm.username" autocomplete="off" />
      </a-form-item>
      <a-form-item
        :label="user?.portal_account ? '新密码' : '初始密码'"
        :required="!user?.portal_account"
        :validate-status="accountForm.password && checkPassword(accountForm.password) ? 'error' : ''"
        :help="
          accountForm.password
            ? checkPassword(accountForm.password) ?? undefined
            : user?.portal_account
              ? '留空表示不修改密码。填写后该账号的全部会话立即失效'
              : '至少 8 位。提交后不再回显,也不写进审计日志'
        "
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

  <UserAdjustModal
    v-model:open="adjustOpen"
    :user="user"
    :targets="[]"
    :tiers="props.tiers"
    :initial-action="adjustAction"
    @done="
      () => {
        emit('changed')
        reload()
      }
    "
  />

  <LbNameConfirm
    v-model:open="deleteOpen"
    :title="`删除用户 ${user?.display_name ?? ''}`"
    :name="user?.display_name ?? ''"
    :loading="deleteLoading"
    prompt="输入用户名称以确认"
    :impacts="[
      `UUID 在 ${user?.effective_node_ids.length ?? 0} 个节点重新部署后失效`,
      '门户登录账号一并删除',
      '历史流量记录保留,用户本身无法恢复',
    ]"
    @confirm="doDelete"
  />
</template>

<style scoped>
.ud__head {
  display: flex;
  flex-direction: column;
  gap: 3px;
}

.ud__title {
  display: flex;
  align-items: center;
  gap: 8px;
}

.ud__name {
  font-size: 16px;
  font-weight: 600;
}

.ud__sub {
  font-size: 11.5px;
  font-weight: 400;
  color: #6b7480;
}

.ud__skel {
  display: flex;
  flex-direction: column;
  gap: 20px;
  padding-top: 16px;
}

.ud__banner {
  margin-top: 12px;
}

.ud__grid {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.ud__card {
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

.ud__card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 11px 16px;
  border-bottom: 1px solid #edeff2;
  font-size: 13px;
  font-weight: 600;
}

.ud__card-head--plain {
  margin-top: 20px;
  padding: 0 0 8px;
  border-bottom: none;
}

.ud__card-body {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 16px;
}

.ud__facts {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 10px 20px;
}

.ud__facts > div {
  display: flex;
  flex-direction: column;
  gap: 3px;
  min-width: 0;
}

.ud__facts-wide {
  grid-column: 1 / -1;
}

.ud__facts span {
  font-size: 11.5px;
  color: #6b7480;
}

.ud__facts b {
  font-size: 12.5px;
  font-weight: 500;
}

.ud__spark {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.ud__spark-cap {
  font-size: 11px;
  color: #6b7480;
}

.ud__note {
  padding: 10px 12px;
  background: #f6f7f9;
  border: 1px solid #edeff2;
  border-radius: 6px;
  font-size: 12px;
  line-height: 1.7;
  color: #576070;
}

.ud__note--info {
  background: #eef4fc;
  border-color: #c9dcf3;
  color: #1d4f96;
}

.ud__note--danger {
  background: #fdecea;
  border-color: #f3cfc9;
  color: #8e2117;
}

.ud__acct-ops {
  display: flex;
  gap: 16px;
  padding-top: 2px;
  font-size: 12.5px;
}

.ud__danger {
  color: #b4291d;
}

.ud__nodes {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

.ud__node {
  display: inline-flex;
  align-items: baseline;
  gap: 6px;
  padding: 3px 9px;
  background: #f6f7f9;
  border: 1px solid #e3e6ea;
  border-radius: 4px;
  font-size: 12.5px;
}

.ud__node em {
  font-style: normal;
  font-size: 10.5px;
  color: #6b7480;
}

.ud__bynode {
  border: 1px solid #edeff2;
  border-radius: 6px;
  overflow: hidden;
}

.ud__bynode-row {
  display: grid;
  grid-template-columns: 1.4fr 0.8fr 1.4fr;
  align-items: center;
  gap: 10px;
  padding: 9px 12px;
  font-size: 12.5px;
}

.ud__bynode-row + .ud__bynode-row {
  border-top: 1px solid #edeff2;
}

.ud__bynode-dir {
  font-size: 11px;
  color: #6b7480;
  text-align: right;
}

.ud__logs {
  display: flex;
  flex-direction: column;
  border: 1px solid #edeff2;
  border-radius: 6px;
  overflow: hidden;
}

.ud__log {
  display: grid;
  grid-template-columns: auto 1fr auto;
  align-items: start;
  gap: 12px;
  padding: 10px 12px;
}

.ud__log + .ud__log {
  border-top: 1px solid #edeff2;
}

.ud__log-body {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.ud__log-action {
  font-size: 12.5px;
  font-weight: 500;
}

.ud__log-detail {
  font-size: 11.5px;
  line-height: 1.6;
  color: #6b7480;
}

.ud__muted {
  color: #6b7480;
}

/* 窄屏:两列事实压成一列,抽屉本身由 AntD 撑满宽度。 */
@media (max-width: 767px) {
  .ud__facts {
    grid-template-columns: 1fr;
  }

  .ud__bynode-row {
    grid-template-columns: 1fr auto;
  }

  .ud__bynode-dir {
    grid-column: 1 / -1;
    text-align: left;
  }
}
</style>
