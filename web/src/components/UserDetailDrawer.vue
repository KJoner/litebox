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
  type UserNodeRange,
  type UserTraffic,
} from '@/api/client'
import { formatBytes, formatTime, formatUTCDay } from '@/utils/format'
import { checkLoginUsername, checkPassword } from '@/utils/validate'
import {
  LbCopyField,
  LbEmptyState,
  LbIcon,
  LbInfoTip,
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

  void loadTraffic(u.id)
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

// ---------- 流量:时间区间、排序与筛选(V20) ----------

/**
 * 一个区间同时作用于区间总量、上传 / 下载、趋势图与按节点明细 —— 它们来自后端
 * 同一次查询。快捷范围按 UTC 日切(账本的日桶就是 UTC 日),自定义起止按本机时间填、
 * 转成 UTC 发给后端。粒度由后端按区间长度定(≤ 3 天按小时,更长且对齐按日),
 * 前端只按它返回的 granularity 铺时间轴,缺桶传 null,不补 0、不插值。
 */
type RangePreset = 'today' | 'yesterday' | '7d' | '30d' | 'this_month' | 'last_month' | 'custom'
const presetOptions: { value: RangePreset; label: string }[] = [
  { value: 'today', label: '今天' },
  { value: 'yesterday', label: '昨天' },
  { value: '7d', label: '近 7 天' },
  { value: '30d', label: '近 30 天' },
  { value: 'this_month', label: '本月' },
  { value: 'last_month', label: '上月' },
  { value: 'custom', label: '自定义' },
]
const rangePreset = ref<RangePreset>('30d')
const customFrom = ref('')
const customTo = ref('')
const trafficLoading = ref(false)
const rangeError = ref('')

function presetRange(p: RangePreset): { from: string; to: string } | null {
  const now = new Date()
  const dayStart = Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate())
  const day = 86400000
  const iso = (ms: number) => new Date(ms).toISOString().replace(/\.\d{3}Z$/, 'Z')
  switch (p) {
    case 'today':
      return { from: iso(dayStart), to: iso(dayStart + day) }
    case 'yesterday':
      return { from: iso(dayStart - day), to: iso(dayStart) }
    case '7d':
      return { from: iso(dayStart - 6 * day), to: iso(dayStart + day) }
    case '30d':
      return { from: iso(dayStart - 29 * day), to: iso(dayStart + day) }
    case 'this_month':
      return {
        from: iso(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1)),
        to: iso(dayStart + day),
      }
    case 'last_month':
      return {
        from: iso(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() - 1, 1)),
        to: iso(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1)),
      }
    default: {
      if (!customFrom.value || !customTo.value) return null
      const f = new Date(customFrom.value)
      const t = new Date(customTo.value)
      if (Number.isNaN(f.getTime()) || Number.isNaN(t.getTime())) return null
      return { from: iso(f.getTime()), to: iso(t.getTime()) }
    }
  }
}

/** 快速切换范围时旧请求不得覆盖新结果:只认最后一次发出的那一个。 */
let trafficSeq = 0

async function loadTraffic(id = props.userId) {
  if (id === null) return
  const r = presetRange(rangePreset.value)
  if (!r) {
    rangeError.value = '自定义范围要同时填起止时间'
    return
  }
  if (r.to <= r.from) {
    rangeError.value = '结束时间必须晚于开始时间'
    return
  }
  rangeError.value = ''
  const seq = ++trafficSeq
  trafficLoading.value = true
  trafficError.value = false
  try {
    const t = await api.userTraffic(id, { from: r.from, to: r.to })
    if (seq !== trafficSeq) return
    traffic.value = t
  } catch (err) {
    if (seq !== trafficSeq) return
    traffic.value = null
    trafficError.value = true
    if (err instanceof ApiError && err.status === 400) rangeError.value = err.message
  } finally {
    if (seq === trafficSeq) trafficLoading.value = false
  }
}

watch(rangePreset, (p) => {
  if (p !== 'custom') void loadTraffic()
})

const rangeCaption = computed(() => {
  const r = traffic.value?.range
  if (!r) return ''
  const unit = r.granularity === 'hour' ? '按小时' : '按 UTC 日'
  const src = r.source === 'daily' ? '日汇总表' : '流水账本'
  const upd = r.updated_at ? `,数据更新于 ${formatTime(r.updated_at)}` : ',还没有入账记录'
  return `${formatUTC(r.from)} ~ ${formatUTC(r.to)} · ${unit}(来自${src})${upd}`
})

/** 按后端给的粒度铺时间轴:缺桶传 null —— 补 0 会把「那段没同步」画成「那段没人用」。 */
const seriesPoints = computed<LbPoint[]>(() => {
  const t = traffic.value
  if (!t?.range) return []
  const from = new Date(t.range.from).getTime()
  const to = new Date(t.range.to).getTime()
  if (Number.isNaN(from) || Number.isNaN(to) || to <= from) return []
  const step = t.range.granularity === 'hour' ? 3600000 : 86400000
  const byAt = new Map(t.series.map((p) => [new Date(p.at).getTime(), p.total]))
  const out: LbPoint[] = []
  // 起点对齐到桶边界(小时 / UTC 日),最多铺 1000 个桶。
  let cursor = Math.floor(from / step) * step
  let guard = 0
  while (cursor < to && guard++ < 1000) {
    out.push({ at: new Date(cursor).toISOString(), value: byAt.has(cursor) ? (byAt.get(cursor) as number) : null })
    cursor += step
  }
  return out
})

function bucketLabel(at: string): string {
  const r = traffic.value?.range
  if (r?.granularity === 'hour') {
    const d = new Date(at)
    const pad = (n: number) => String(n).padStart(2, '0')
    return `${d.getUTCMonth() + 1}/${d.getUTCDate()} ${pad(d.getUTCHours())}:00 UTC`
  }
  return formatUTCDay(at)
}

type NodeSortKey = 'total' | 'uplink' | 'downlink' | 'name' | 'sort_order'
const nodeSortKey = ref<NodeSortKey>('total')
const nodeSortDesc = ref(true)
const nodeFilter = ref('')
const nodeSortOptions: { value: NodeSortKey; label: string }[] = [
  { value: 'total', label: '总流量' },
  { value: 'uplink', label: '上传' },
  { value: 'downlink', label: '下载' },
  { value: 'name', label: '内部名称' },
  { value: 'sort_order', label: '排序号' },
]

/** 默认总流量降序;同值按稳定的次级键(名称、节点 ID),不随刷新漂移。 */
const sortedByNode = computed<UserNodeRange[]>(() => {
  const list = [...(traffic.value?.by_node ?? [])]
  const kw = nodeFilter.value.trim().toLowerCase()
  const filtered = kw ? list.filter((n) => n.node_name.toLowerCase().includes(kw)) : list
  const dir = nodeSortDesc.value ? -1 : 1
  const key = nodeSortKey.value
  return filtered.sort((a, b) => {
    let c = 0
    if (key === 'name') c = a.node_name.localeCompare(b.node_name, 'zh-Hans-CN')
    else if (key === 'sort_order') c = a.node_sort_order - b.node_sort_order
    else c = a[key] - b[key]
    if (c !== 0) return c * dir
    return a.node_name.localeCompare(b.node_name, 'zh-Hans-CN') || a.node_id - b.node_id
  })
})

function sharePercent(n: UserNodeRange): string {
  const total = traffic.value?.total?.total ?? 0
  if (total <= 0) return '—'
  return `${((n.total / total) * 100).toFixed(1)}%`
}

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

/**
 * 两种协议各有一份凭据,重置一份不动另一份。
 *
 * 影响面也不同:重置 UUID 只让跑 VLESS 的节点重新部署,重置这一把
 * 只让跑 Shadowsocks 的节点重新部署 —— 后端按节点协议筛过,
 * 不相干的机器不会被白白重启一次。
 */
function confirmRegenerateSSPassword() {
  lbDangerConfirm({
    title: `重新生成 ${user.value?.display_name} 的 Shadowsocks 密钥?`,
    okText: '重新生成',
    impacts: [
      '该用户在所有 Shadowsocks 节点上的凭据在重新部署后立即失效',
      '需要重新导入订阅才能恢复',
      'VLESS 节点不受影响,订阅地址本身也不变',
    ],
    onOk: () =>
      act(() => api.regenerateUserSSPassword(props.userId!), 'Shadowsocks 密钥已重新生成'),
  })
}

/** 第三把,与上面两把对称。 */
function confirmRegenerateSnellKey() {
  lbDangerConfirm({
    title: `重新生成 ${user.value?.display_name} 的 Snell 凭据?`,
    okText: '重新生成',
    impacts: [
      '该用户在所有 Snell 入口上的凭据在重新部署后立即失效',
      '需要重新导入订阅才能恢复',
      'VLESS 与 Shadowsocks 入口不受影响,订阅地址本身也不变',
    ],
    onOk: () => act(() => api.regenerateUserSnellKey(props.userId!), 'Snell 凭据已重新生成'),
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
    class="ud"
    @close="emit('close')"
  >
    <template #title>
      <div class="ud__head">
        <div class="ud__title">
          <span class="ud__name">{{ head?.display_name ?? '用户详情' }}</span>
          <LbStatusTag v-if="head" kind="user" :status="head.status" />
          <span v-if="head" class="lb-chip">{{ head.access_tier_name }}</span>
        </div>
        <div v-if="head" class="ud__sub lb-tabular">
          {{ head.user_code }} · 创建于 {{ formatTime(head.created_at) }} ·
          <template v-if="user?.last_renewal_at">
            最近续期 <LbTimeText :value="user.last_renewal_at" />
          </template>
          <template v-else>从未续期</template>
        </div>
      </div>
    </template>

    <template #extra>
      <div v-if="user" class="ud__extra">
        <a-button type="primary" size="small" @click="runPrimary">{{ primaryLabel }}</a-button>
        <a-button size="small" class="lb-btn-ghost lb-btn-ghost--text" @click="emit('edit', user)">编辑</a-button>
        <a-dropdown placement="bottomRight">
          <a-button
            size="small"
            class="lb-btn-circle lb-btn-ghost lb-btn-ghost--text ud__more"
            :aria-label="`${user.display_name} 的更多操作`"
            title="更多操作"
          >
            <LbIcon name="more" :size="16" />
          </a-button>
          <template #overlay>
            <a-menu>
              <a-menu-item @click="openAdjust('RESET_TRAFFIC')">重置已用流量</a-menu-item>
              <a-menu-item @click="openAdjust('CHANGE_TIER')">调整访问等级</a-menu-item>
              <a-menu-item @click="confirmRegenerateToken">重新生成订阅地址</a-menu-item>
              <a-menu-item @click="confirmRegenerateUUID">重新生成 UUID(VLESS)</a-menu-item>
              <a-menu-item @click="confirmRegenerateSSPassword">
                重新生成密钥(Shadowsocks)
              </a-menu-item>
              <a-menu-item @click="confirmRegenerateSnellKey">
                重新生成凭据(Snell)
              </a-menu-item>
              <a-menu-divider />
              <a-menu-item danger @click="deleteOpen = true">删除用户</a-menu-item>
            </a-menu>
          </template>
        </a-dropdown>
      </div>
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
      <div v-if="banner" class="lb-notice ud__banner">
        <span class="lb-notice__icon" :class="banner.type === 'error' ? 'lb-notice__icon--bad' : 'lb-notice__icon--warn'">
          <LbIcon name="alert-triangle" :size="18" />
        </span>
        <div class="lb-notice__body ud__banner-text">{{ banner.text }}</div>
      </div>

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
                  <LbSparkline :points="seriesPoints" type="bar" :height="72" :label-format="bucketLabel" />
                  <div class="ud__spark-cap">
                    {{ presetOptions.find((o) => o.value === rangePreset)?.label ?? '区间' }} ·
                    {{ traffic?.range?.granularity === 'hour' ? '按小时' : '按 UTC 日' }}(与「流量」Tab 同一区间)
                    <LbInfoTip text="空心柱表示那一段没有记录,不是 0 —— 不补 0、不插值。" :width="260" />
                  </div>
                </div>
              </div>
            </section>

            <section class="ud__card">
              <div class="ud__card-head">
                门户登录账号
                <span class="ud__card-links">
                  <a v-if="user.portal_account" @click="openAccountForm">重设密码</a>
                  <a v-if="user.portal_account" @click="toggleLogin">
                    {{ user.portal_account.login_enabled ? '关闭门户登录' : '开启门户登录' }}
                  </a>
                  <a v-else @click="openAccountForm">开通</a>
                </span>
              </div>
              <div v-if="user.portal_account" class="ud__card-body">
                <div class="ud__facts">
                  <div><span>登录账号</span><b class="lb-mono">{{ user.portal_account.username }}</b></div>
                  <div>
                    <span>登录状态</span>
                    <b>
                      <LbStatusTag
                        v-if="user.portal_account.must_change_password"
                        :meta="{ text: '待改初始密码', shape: 'triangle', fg: 'var(--warn)', bg: 'var(--warn-bg)' }"
                      />
                      <!-- login_enabled=false 全站统称「门户登录已关闭」,不叫「已停用」 -->
                      <LbStatusTag
                        v-else-if="!user.portal_account.login_enabled"
                        :meta="{ text: '门户登录已关闭', shape: 'pause', fg: 'var(--purple)', bg: 'var(--purple-bg)' }"
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
              <div class="ud__card-head">可用节点 <span class="ud__count">{{ user.effective_node_ids.length }}</span></div>
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
            <div class="ud__range">
              <a-segmented v-model:value="rangePreset" :options="presetOptions" size="small" />
              <div v-if="rangePreset === 'custom'" class="ud__range-custom">
                <a-input v-model:value="customFrom" type="datetime-local" size="small" />
                <span class="ud__muted">~</span>
                <a-input v-model:value="customTo" type="datetime-local" size="small" />
                <a-button size="small" type="primary" :loading="trafficLoading" @click="loadTraffic()">查询</a-button>
              </div>
            </div>
            <div v-if="rangeError" class="ud__note ud__note--danger">{{ rangeError }}</div>

            <section class="ud__card">
              <div class="ud__card-head">
                区间流量
                <span class="ud__spark-cap">
                  <span v-if="trafficLoading">正在查询…</span>
                  <span v-else>{{ rangeCaption }}</span>
                  <LbInfoTip
                    :width="300"
                    text="这是所选区间内的用户流量(与额度同一口径:链路凭据与不计流量的入口都不算),与下面的「当前额度用量」是两回事 —— 查历史区间不改变额度、重置时间与累计账本。空心柱表示那一段没有记录,不是 0;不补 0、不插值。"
                  />
                </span>
              </div>
              <div class="ud__card-body">
                <div class="ud__range-stats">
                  <div class="ud__range-stat">
                    <div class="ud__range-stat-label">区间总流量</div>
                    <div class="ud__range-stat-value lb-tabular">{{ traffic?.total ? formatBytes(traffic.total.total) : '—' }}</div>
                  </div>
                  <div class="ud__range-stat">
                    <div class="ud__range-stat-label">上传</div>
                    <div class="ud__range-stat-value lb-tabular">{{ traffic?.total ? formatBytes(traffic.total.uplink) : '—' }}</div>
                  </div>
                  <div class="ud__range-stat">
                    <div class="ud__range-stat-label">下载</div>
                    <div class="ud__range-stat-value lb-tabular">{{ traffic?.total ? formatBytes(traffic.total.downlink) : '—' }}</div>
                  </div>
                  <div class="ud__range-stat">
                    <div class="ud__range-stat-label">当前额度用量</div>
                    <div class="ud__range-stat-value lb-tabular">
                      {{ traffic ? formatBytes(traffic.used_total) : '—' }}
                      <span class="ud__muted">/ {{ traffic ? (traffic.quota_bytes > 0 ? formatBytes(traffic.quota_bytes) : '不限') : '—' }}</span>
                    </div>
                  </div>
                </div>
                <LbSparkline :points="seriesPoints" type="bar" :height="130" :label-format="bucketLabel" />
              </div>
            </section>

            <div class="ud__card-head ud__card-head--plain ud__bynode-head">
              <span>按节点</span>
              <span class="ud__bynode-tools">
                <a-input v-model:value="nodeFilter" size="small" placeholder="筛选节点" allow-clear style="width: 130px" />
                <a-select v-model:value="nodeSortKey" size="small" style="width: 110px">
                  <a-select-option v-for="o in nodeSortOptions" :key="o.value" :value="o.value">{{ o.label }}</a-select-option>
                </a-select>
                <a-button size="small" class="lb-btn-ghost" @click="nodeSortDesc = !nodeSortDesc">
                  {{ nodeSortDesc ? '降序' : '升序' }}
                </a-button>
              </span>
            </div>
            <div v-if="sortedByNode.length" class="ud__bynode">
              <div v-for="n in sortedByNode" :key="n.node_id" class="ud__bynode-row">
                <span class="lb-ellipsis">
                  {{ n.node_name }}
                  <span v-if="n.deleted" class="lb-chip">已删除</span>
                  <span class="ud__muted">#{{ n.node_sort_order }}</span>
                </span>
                <span class="lb-mono">{{ formatBytes(n.total) }} <span class="ud__muted">{{ sharePercent(n) }}</span></span>
                <span class="lb-mono ud__bynode-dir">
                  ↑ {{ formatBytes(n.uplink) }} · ↓ {{ formatBytes(n.downlink) }}
                </span>
              </div>
            </div>
            <div v-else-if="traffic && nodeFilter" class="ud__note">没有名称匹配「{{ nodeFilter }}」的节点。</div>
            <div v-else-if="traffic" class="ud__note">这段时间里该用户在任何节点上都没有记录。</div>
            <div v-if="traffic?.no_data?.length" class="ud__nodata">
              <div v-for="n in traffic.no_data" :key="n.node_id" class="ud__nodata-row">
                <span class="lb-ellipsis">{{ n.node_name }}</span>
                <span v-if="n.reason === 'collect_failed'" class="lb-chip lb-chip--bad" :title="n.detail">采集失败</span>
                <span v-else class="lb-chip">无记录</span>
              </div>
              <div class="ud__note">
                「无记录」与「真的没用」在账本里长得一样;「采集失败」是最近一轮同步在那台机器上失败了,
                那段用量不是零,是没采到。
              </div>
            </div>
            <div class="ud__note">外部代理与中转线路不可统计:它们的流量走别人的服务器或不经认证,面板拿不到用户级计数。</div>
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
                    ? { text: '成功', shape: 'check', fg: 'var(--ok)', bg: 'var(--ok-bg)' }
                    : { text: '失败', shape: 'cross', fg: 'var(--bad)', bg: 'var(--bad-bg)' }
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
  gap: 2px;
  min-width: 0;
}

.ud__title {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}

.ud__name {
  font-size: 20px;
  font-weight: 700;
  letter-spacing: -0.02em;
}

.ud__sub {
  font-size: 12.5px;
  font-weight: 400;
  color: var(--text3);
}

.ud__extra {
  display: flex;
  align-items: center;
  gap: 8px;
}
.ud__extra :deep(.ant-btn-sm) {
  height: 32px;
  padding: 0 14px;
  font-size: 13px;
}
.ud__extra :deep(.ud__more.ant-btn) {
  width: 32px;
  height: 32px;
  min-width: 32px;
  padding: 0;
}

.ud__skel {
  display: flex;
  flex-direction: column;
  gap: 20px;
  padding-top: 8px;
}

.ud__banner {
  align-items: center;
  padding: 14px 18px;
  margin-bottom: 16px;
}
.ud__banner-text {
  font-size: 13.5px;
  line-height: 1.5;
}

.ud__grid {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.ud__card {
  background: var(--surface);
  border-radius: var(--r-card);
  box-shadow: var(--shadow);
  overflow: hidden;
}

.ud__card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 18px 22px 0;
  font-size: 15px;
  font-weight: 600;
}
.ud__card-links {
  display: flex;
  gap: 14px;
  font-weight: 500;
  font-size: 13px;
}
.ud__count {
  color: var(--text3);
  font-weight: 400;
}

.ud__card-head--plain {
  margin-top: 20px;
  padding: 0 4px 10px;
}

.ud__card-body {
  display: flex;
  flex-direction: column;
  gap: 16px;
  padding: 16px 22px 20px;
}

.ud__facts {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 12px;
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
  font-size: 12px;
  color: var(--text3);
}

.ud__facts b {
  font-size: 13.5px;
  font-weight: 500;
  font-variant-numeric: tabular-nums;
  overflow-wrap: anywhere;
}

.ud__spark {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.ud__spark-cap {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 12px;
  font-weight: 400;
  color: var(--text3);
}

.ud__note {
  padding: 12px 14px;
  background: var(--surface2);
  border-radius: var(--r-group);
  font-size: 13px;
  line-height: 1.7;
  color: var(--text2);
}

.ud__note--info {
  background: var(--brand-bg);
  color: var(--text);
}

.ud__note--danger {
  background: var(--bad-bg);
  color: var(--text);
}

.ud__acct-ops {
  display: flex;
  gap: 16px;
  font-size: 13px;
  font-weight: 500;
}

.ud__danger {
  color: var(--bad);
}
.ud__danger:hover {
  color: var(--bad);
  opacity: 0.8;
}

.ud__nodes {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

.ud__node {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  padding: 6px 12px;
  background: var(--surface2);
  border-radius: var(--r-pill);
  font-size: 13px;
  font-weight: 500;
}

.ud__node em {
  font-style: normal;
  font-size: 11.5px;
  font-weight: 400;
  color: var(--text3);
}

.ud__bynode {
  background: var(--surface);
  border-radius: var(--r-card);
  box-shadow: var(--shadow);
  overflow: hidden;
}

.ud__bynode-row {
  display: grid;
  grid-template-columns: 1.4fr 0.8fr 1.4fr;
  align-items: center;
  gap: 10px;
  padding: 14px 20px;
  font-size: 13px;
}

.ud__bynode-row + .ud__bynode-row {
  border-top: 1px solid var(--sep2);
}

.ud__bynode-dir {
  font-size: 12px;
  color: var(--text3);
  text-align: right;
}

.ud__range {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
  margin-bottom: 12px;
}
.ud__range-custom {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.ud__range-stats {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 12px;
  margin-bottom: 12px;
}
.ud__range-stat-label {
  font-size: 12px;
  color: var(--text3);
}
.ud__range-stat-value {
  font-size: 16px;
  font-weight: 600;
}
.ud__bynode-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  flex-wrap: wrap;
}
.ud__bynode-tools {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.ud__nodata {
  margin-top: 10px;
  background: var(--surface2);
  border-radius: var(--r-group);
  padding: 8px 14px;
}
.ud__nodata-row {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 10px;
  padding: 6px 0;
  font-size: 13px;
}
@media (max-width: 767px) {
  .ud__range-stats {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}

.ud__logs {
  display: flex;
  flex-direction: column;
  background: var(--surface);
  border-radius: var(--r-card);
  box-shadow: var(--shadow);
  overflow: hidden;
}

.ud__log {
  display: grid;
  grid-template-columns: auto 1fr auto;
  align-items: start;
  gap: 12px;
  padding: 14px 20px;
}

.ud__log + .ud__log {
  border-top: 1px solid var(--sep2);
}

.ud__log-body {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.ud__log-action {
  font-size: 13.5px;
  font-weight: 600;
}

.ud__log-detail {
  font-size: 12.5px;
  line-height: 1.6;
  color: var(--text2);
}

.ud__muted {
  color: var(--text3);
}

/* 窄屏:四列事实压成两列,抽屉本身由 AntD 撑满宽度。 */
@media (max-width: 767px) {
  .ud__facts {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }

  .ud__card-head {
    padding: 16px 16px 0;
  }
  .ud__card-body {
    padding: 14px 16px 16px;
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
