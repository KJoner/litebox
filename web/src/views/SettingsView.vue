<script setup lang="ts">
import { computed, nextTick, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AccessTier,
  type Node,
  type NotifyKind,
  type NotifyResult,
  type NotifySettings,
  type PanelSettings,
  type ProxyUser,
} from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import { LbCopyField, LbEmptyState, LbInfoTip, LbSectionTitle, lbDangerConfirm } from '@/components/lb'
import CloudAccountsPanel from '@/components/cloud/CloudAccountsPanel.vue'
import BoceSettingsCard from '@/components/boce/BoceSettingsCard.vue'
import OrderMigrationModal from '@/components/node/OrderMigrationModal.vue'

/**
 * 系统设置。全站唯一的「分组表单」页,也是唯一需要「还原」按钮的地方 ——
 * 其余表单都在弹窗里,关掉即放弃。
 *
 * **每组一个保存按钮,不做全局保存。** 几组的生效方式完全不同:
 * 订阅地址改完立即生效、等级改动会触发重新部署、密码修改会撤销会话。
 * 一个全局「保存」意味着管理员改个域名就顺手把等级也提交了,而那会重启一批节点。
 *
 * 左侧锚点导航而不是 Tab:几组内容都不长,Tab 会让人为了确认一个值来回切;
 * 锚点让整页可滚、可 Ctrl+F。
 *
 * UI 2.0:每组 = 外置标题 + 生效徽标 + 一张设置卡;卡内分组列表不自带边框(卡里套框是双层框),
 * 行线贴满卡片,保存按钮在带底色的卡片底栏;帮助文案全部收进标题旁或字段旁的 ⓘ。
 */
const auth = useAuthStore()
const router = useRouter()
const route = useRoute()

const settings = ref<PanelSettings | null>(null)
const tiers = ref<AccessTier[]>([])
const users = ref<ProxyUser[]>([])
const nodes = ref<Node[]>([])
const loadError = ref<string>('')

const sections = [
  { id: 'sub', label: '订阅地址' },
  { id: 'probe', label: '拨测目标' },
  { id: 'notify', label: '监控与推送' },
  { id: 'expiry', label: '到期与续费提醒' },
  { id: 'probe-public', label: '公共探测目标' },
  { id: 'tcpprobe', label: '网络探测' },
  { id: 'boce', label: 'Boce 外部拨测' },
  { id: 'cloud', label: '云账号' },
  { id: 'key', label: '面板 SSH 公钥' },
  { id: 'tier', label: '访问等级' },
  { id: 'pwd', label: '管理员密码' },
]
const activeSection = ref('sub')

function jump(id: string) {
  activeSection.value = id
  document.getElementById(`set-${id}`)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
}

// ---------- 订阅地址 ----------

const baseURL = ref('')
const savedBaseURL = ref('')
const savingBaseURL = ref(false)
const baseURLDirty = computed(() => baseURL.value !== savedBaseURL.value)

async function saveBaseURL() {
  savingBaseURL.value = true
  try {
    const s = await api.updateSettings({ subscription_base_url: baseURL.value })
    settings.value = { ...(settings.value as PanelSettings), ...s }
    baseURL.value = s.subscription_base_url
    savedBaseURL.value = s.subscription_base_url
    message.success('已保存,立即生效')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    savingBaseURL.value = false
  }
}

// ---------- 拨测目标 ----------

const probeURL = ref('')
const savedProbeURL = ref('')
const savingProbeURL = ref(false)
const probeURLDirty = computed(() => probeURL.value !== savedProbeURL.value)

async function saveProbeURL() {
  savingProbeURL.value = true
  try {
    const s = await api.updateSettings({ probe_url: probeURL.value })
    settings.value = { ...(settings.value as PanelSettings), ...s }
    probeURL.value = s.probe_url
    savedProbeURL.value = s.probe_url
    message.success('已保存,下一次部署起生效')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    savingProbeURL.value = false
  }
}

// ---------- 到期与续费提醒(V20) ----------

const expiryForm = reactive({ lead_days: '', send_time: '', timezone: '' })
const savedExpiry = reactive({ lead_days: '', send_time: '', timezone: '' })
const savingExpiry = ref(false)
const expiryDirty = computed(
  () =>
    expiryForm.lead_days !== savedExpiry.lead_days ||
    expiryForm.send_time !== savedExpiry.send_time ||
    expiryForm.timezone !== savedExpiry.timezone,
)

function fillExpiry(s: PanelSettings) {
  expiryForm.lead_days = s.expiry_lead_days ?? ''
  expiryForm.send_time = s.expiry_send_time ?? ''
  expiryForm.timezone = s.expiry_timezone ?? ''
  Object.assign(savedExpiry, { ...expiryForm })
}

async function saveExpiry() {
  savingExpiry.value = true
  try {
    const s = await api.updateSettings({
      expiry_lead_days: expiryForm.lead_days,
      expiry_send_time: expiryForm.send_time,
      expiry_timezone: expiryForm.timezone,
    })
    settings.value = { ...(settings.value as PanelSettings), ...s }
    fillExpiry(s)
    message.success('已保存,下一轮提醒起生效')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    savingExpiry.value = false
  }
}

function restoreExpiry() {
  Object.assign(expiryForm, { ...savedExpiry })
}

// ---------- TCP 探测(资产卡片) ----------

/** 数值留空 = 用默认;后端收 0 / 空串表示改回默认。 */
const tcpForm = reactive({ enabled: true, interval: '', timeout: '', alert_after: '', quiet: '' })
const savedTcp = reactive({ enabled: true, interval: '', timeout: '', alert_after: '', quiet: '' })
const savingTcp = ref(false)
const tcpDirty = computed(() => JSON.stringify(tcpForm) !== JSON.stringify(savedTcp))

function fillTcp(s: PanelSettings) {
  tcpForm.enabled = s.probe_enabled ?? true
  tcpForm.interval = s.probe_interval_sec ? String(s.probe_interval_sec) : ''
  tcpForm.timeout = s.probe_timeout_ms ? String(s.probe_timeout_ms) : ''
  tcpForm.alert_after = s.probe_alert_after ?? ''
  tcpForm.quiet = s.probe_quiet_hours ?? ''
  Object.assign(savedTcp, { ...tcpForm })
}

async function saveTcp() {
  savingTcp.value = true
  try {
    const s = await api.updateSettings({
      probe_enabled: tcpForm.enabled,
      probe_interval_sec: tcpForm.interval.trim() ? Number(tcpForm.interval) : 0,
      probe_timeout_ms: tcpForm.timeout.trim() ? Number(tcpForm.timeout) : 0,
      probe_alert_after: tcpForm.alert_after.trim(),
      probe_quiet_hours: tcpForm.quiet.trim(),
    })
    settings.value = { ...(settings.value as PanelSettings), ...s }
    fillTcp(s)
    message.success('已保存,下一轮探测起生效')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    savingTcp.value = false
  }
}

function restoreTcp() {
  Object.assign(tcpForm, { ...savedTcp })
}

// ---------- 访问等级 ----------

const editingTier = ref<AccessTier | null>(null)
const tierForm = reactive({ name: '', level: 10, description: '' })
const savingTier = ref(false)

/** 在用数量纯读现有接口算出来,不加后端。 */
function tierUsage(t: AccessTier) {
  return {
    users: users.value.filter((u) => u.access_tier_id === t.id).length,
    // 数的是【入口】不是机器:等级已经降到入口上(迁移 0020),
    // 一台机器上可以既有普通组入口又有 VIP 入口。
    inbounds: nodes.value.reduce(
      (sum, n) => sum + (n.inbounds ?? []).filter((i) => i.access_tier_id === t.id).length,
      0,
    ),
  }
}

function openTier(t: AccessTier) {
  editingTier.value = t
  tierForm.name = t.name
  tierForm.level = t.level
  tierForm.description = t.description
}

async function saveTier() {
  if (!editingTier.value) return
  savingTier.value = true
  try {
    await api.updateAccessTier(editingTier.value.id, {
      name: tierForm.name,
      level: tierForm.level,
      description: tierForm.description,
    })
    editingTier.value = null
    message.success('已保存。改 level 会改变可用节点集合,受影响节点已排入自动重新部署')
    await loadAll()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    savingTier.value = false
  }
}

// ---------- 管理员密码 ----------

const pwd = reactive({ old: '', next: '', confirm: '' })
const savingPwd = ref(false)

const pwdError = computed(() => {
  if (pwd.next && pwd.next.length < 8) return '新密码长度至少 8 位'
  if (pwd.confirm && pwd.next !== pwd.confirm) return '两次输入的新密码不一致'
  return ''
})

async function changePassword() {
  if (pwdError.value || !pwd.old || !pwd.next) {
    message.warning(pwdError.value || '请填写原密码与新密码')
    return
  }
  savingPwd.value = true
  try {
    const result = await api.changePassword(pwd.old, pwd.next)
    message.success(result.message)
    pwd.old = ''
    pwd.next = ''
    pwd.confirm = ''
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      // 401 可能是原密码错误,也可能是会话已失效,两者要区分处理。
      if (err.message.includes('原密码')) {
        message.error(err.message)
      } else {
        auth.clear()
        await router.replace({ name: 'login' })
      }
    } else {
      message.error(err instanceof ApiError ? err.message : '修改密码失败')
    }
  } finally {
    savingPwd.value = false
  }
}

// ---------- 监控与推送 ----------

const notify = ref<NotifySettings | null>(null)
const savingNotify = ref(false)
const testingNotify = ref(false)
const notifyError = ref('')
const testResults = ref<NotifyResult[]>([])

/**
 * 三个凭据输入框。
 *
 * **它们永远是空的** —— 后端从不回显凭据。所以「没动这一栏」与
 * 「我要清空它」必须分得开:留空表示不改(发 null),点了「清除」
 * 才发空串。用一个普通字符串的话,管理员改一下分组名就会把推送地址
 * 一起清掉,而界面上什么都不会说。
 */
const secrets = reactive({
  bark_url: '',
  telegram_api_base: '',
  telegram_proxy_key: '',
})
const clearing = reactive({
  bark_url: false,
  telegram_api_base: false,
  telegram_proxy_key: false,
})

function secretPayload(key: keyof typeof secrets): string | null {
  if (clearing[key]) return ''
  const v = secrets[key].trim()
  return v === '' ? null : v
}

/** 勾选的事件。**空数组表示全开**,所以界面上默认全勾。 */
const chosenKinds = ref<NotifyKind[]>([])

function toggleKind(k: NotifyKind) {
  const i = chosenKinds.value.indexOf(k)
  if (i >= 0) chosenKinds.value = chosenKinds.value.filter((x) => x !== k)
  else chosenKinds.value = [...chosenKinds.value, k]
}

async function loadNotify() {
  notifyError.value = ''
  try {
    const n = await api.notifySettings()
    notify.value = n
    chosenKinds.value = n.kinds.length ? [...n.kinds] : n.available_kinds.map((k) => k.kind)
    secrets.bark_url = ''
    secrets.telegram_api_base = ''
    secrets.telegram_proxy_key = ''
    clearing.bark_url = false
    clearing.telegram_api_base = false
    clearing.telegram_proxy_key = false
  } catch (err) {
    notifyError.value = err instanceof ApiError ? err.message : '加载推送设置失败'
  }
}

async function saveNotify() {
  const n = notify.value
  if (!n) return
  savingNotify.value = true
  notifyError.value = ''
  try {
    const saved = await api.updateNotifySettings({
      enabled: n.enabled,
      bark_enabled: n.bark_enabled,
      bark_url: secretPayload('bark_url'),
      bark_group: n.bark_group,
      bark_sound: n.bark_sound,
      telegram_enabled: n.telegram_enabled,
      telegram_api_base: secretPayload('telegram_api_base'),
      telegram_proxy_key: secretPayload('telegram_proxy_key'),
      telegram_chat_id: n.telegram_chat_id,
      telegram_thread_id: n.telegram_thread_id,
      // 全勾等于不限制:发空数组,这样以后新增的事件类型会自动被收到。
      kinds: chosenKinds.value.length === n.available_kinds.length ? [] : chosenKinds.value,
      auto_recover: n.auto_recover,
    })
    notify.value = saved
    chosenKinds.value = saved.kinds.length
      ? [...saved.kinds]
      : saved.available_kinds.map((k) => k.kind)
    secrets.bark_url = ''
    secrets.telegram_api_base = ''
    secrets.telegram_proxy_key = ''
    clearing.bark_url = false
    clearing.telegram_api_base = false
    clearing.telegram_proxy_key = false
    message.success('已保存,下一条通知就走新配置')
  } catch (err) {
    notifyError.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    savingNotify.value = false
  }
}

/** 先保存再测:测的必须是刚填进去的那份,不是上一次保存的那份。 */
async function testNotify() {
  testingNotify.value = true
  testResults.value = []
  try {
    await saveNotify()
    const r = await api.testNotify()
    testResults.value = r.results
  } catch (err) {
    notifyError.value = err instanceof ApiError ? err.message : '发送失败'
  } finally {
    testingNotify.value = false
  }
}

// ---------- 取数 ----------

async function loadAll() {
  loadError.value = ''
  try {
    const s = await api.settings()
    settings.value = s
    baseURL.value = s.subscription_base_url
    savedBaseURL.value = s.subscription_base_url
    probeURL.value = s.probe_url ?? ''
    savedProbeURL.value = s.probe_url ?? ''
    fillExpiry(s)
    fillTcp(s)
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '加载设置失败'
  }
  // 等级表要的三份数据各自降级:等级读不到就不显示那一组,不影响改域名与改密码。
  api.accessTiers().then((r) => (tiers.value = r.items)).catch(() => (tiers.value = []))
  api.users().then((r) => (users.value = r.items)).catch(() => (users.value = []))
  api.nodes().then((r) => (nodes.value = r.items)).catch(() => (nodes.value = []))
  // 推送设置读不到不影响这一页的其余部分 —— 与等级表同样的降级。
  void loadNotify()
}

onMounted(async () => {
  await loadAll()
  // 卡片、节点详情里的「去配置」带着 #probe-public 这类锚点进来:直接跳到那一节。
  const id = route.hash.replace(/^#/, '')
  if (id && sections.some((x) => x.id === id)) {
    await nextTick()
    jump(id)
  }
})

// ---------- 订阅排序方案(V20) ----------

const orderOpen = ref(false)
const switchingOrder = ref(false)
const orderScheme = computed(() => settings.value?.subscription_order_scheme ?? 'LEGACY')

/** 切回旧方案不走迁移:密排过的值在旧方案下顺序相同,只是外部代理回到整块。 */
function switchBackLegacy() {
  lbDangerConfirm({
    title: '切回旧排序方案?',
    okText: '切回',
    okType: 'danger',
    impacts: [
      '外部代理不再按自己的全局排序值插在机器之间,整块回到自建节点之后(或之前,看下面那项设置)',
      '节点与入口的排序号不变:旧方案只看相对大小,顺序与现在一致',
      '不进节点配置、不部署、不重启服务;用户下次拉订阅时看到的是旧方案的顺序',
    ],
    onOk: () => {
      void doSwitchBackLegacy()
    },
  })
}

async function doSwitchBackLegacy() {
  switchingOrder.value = true
  try {
    settings.value = await api.updateSettings({ subscription_order_scheme: 'LEGACY' })
    message.success('已切回旧排序方案')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '切换失败')
  } finally {
    switchingOrder.value = false
  }
}

const baseURLTip = computed(
  () =>
    `用户订阅链接的前缀,必须带 http:// 或 https://。配置文件里的默认值是 ${
      settings.value?.config_base_url || 'http://127.0.0.1:8080'
    },留空则用它 —— 那个地址只能在本机访问,用户拉不到订阅。改域名不会让已发出的订阅失效:Token 不变,用户不必重新导入;但客户端在下次拉订阅之前用的仍是旧地址,旧域名要留一段时间。`,
)

const probeTip = computed(
  () =>
    `每次下发配置,面板都会从节点上经被测的那个入口真的去取一次这个地址,回 2xx / 3xx 才算通。留空用默认的 ${
      settings.value?.default_probe_url || 'https://www.gstatic.com/generate_204'
    }。直连、链式、nginx / realm 转发、Mieru 打的都是同一个地址 —— 它验的是「用户连上之后能不能上网」,不再碰节点的 sshd。节点所在地区连不上 Google 的话,换成任何一个返回 204 的地址。https 会校验证书,链路上有东西劫持 TLS 时会被报出来。`,
)
</script>

<template>
  <div class="lb-page st">
    <!-- 1280 以下锚点栏收起,各组直接顺排。 -->
    <nav class="st__anchors">
      <div class="st__anchors-title">本页</div>
      <button
        v-for="s in sections"
        :key="s.id"
        class="st__anchor"
        :class="{ 'st__anchor--on': activeSection === s.id }"
        @click="jump(s.id)"
      >
        {{ s.label }}
      </button>
    </nav>

    <div class="st__main">
      <div class="lb-page__head">
        <div class="lb-page__title-wrap">
          <h1 class="lb-page__title">系统设置</h1>
          <div class="lb-page__summary">每一组单独保存。改动的生效方式各不相同,标在各组标题旁。</div>
        </div>
      </div>

      <div v-if="loadError" class="lb-card lb-card--flush">
        <LbEmptyState variant="error" :title="loadError" @retry="loadAll" />
      </div>

      <!-- ① 订阅地址 -->
      <section id="set-sub">
        <LbSectionTitle title="订阅地址" effect="now" />
        <div class="lb-card st__card">
          <div class="lb-group">
            <div class="lb-group__row st__row">
              <span class="lb-group__label">
                <span class="st__req">*</span> 站点根地址
                <LbInfoTip :text="baseURLTip" :width="320" />
              </span>
              <a-input v-model:value="baseURL" placeholder="https://box.example.com" />
            </div>
            <div class="lb-group__row st__row">
              <span class="lb-group__label st__label--muted">生成后的地址</span>
              <div class="lb-group__value lb-group__value--mono st__code">
                {{ baseURL || settings?.config_base_url || 'https://box.example.com' }}/sub/&lt;token&gt;
              </div>
            </div>
          </div>

          <div class="st__actions">
            <span v-if="baseURLDirty" class="st__dirty">已改动</span>
            <a-button v-if="baseURLDirty" size="small" class="lb-btn-ghost lb-btn-ghost--text" @click="baseURL = savedBaseURL">
              还原
            </a-button>
            <a-button
              type="primary"
              size="small"
              :disabled="!baseURLDirty"
              :loading="savingBaseURL"
              @click="saveBaseURL"
            >
              保存订阅地址
            </a-button>
          </div>
        </div>
      </section>

      <!-- ② 拨测目标 -->
      <section id="set-probe">
        <LbSectionTitle title="拨测目标">
          <template #badge>
            <span class="lb-effect lb-effect--deploy">下一次部署起生效</span>
          </template>
        </LbSectionTitle>
        <div class="lb-card st__card">
          <div class="lb-group">
            <div class="lb-group__row st__row">
              <span class="lb-group__label">
                拨测地址
                <LbInfoTip :text="probeTip" :width="340" />
              </span>
              <a-input
                v-model:value="probeURL"
                :placeholder="settings?.default_probe_url || 'https://www.gstatic.com/generate_204'"
              />
            </div>
          </div>

          <div class="st__actions">
            <span v-if="probeURLDirty" class="st__dirty">已改动</span>
            <a-button v-if="probeURLDirty" size="small" class="lb-btn-ghost lb-btn-ghost--text" @click="probeURL = savedProbeURL">
              还原
            </a-button>
            <a-button
              type="primary"
              size="small"
              :disabled="!probeURLDirty"
              :loading="savingProbeURL"
              @click="saveProbeURL"
            >
              保存拨测目标
            </a-button>
          </div>
        </div>
      </section>

      <!-- ③ 监控与推送 -->
      <section id="set-notify">
        <LbSectionTitle title="监控与推送" effect="now" />
        <div class="lb-card st__card">
          <div v-if="notifyError" class="lb-error-strip">{{ notifyError }}</div>

          <div v-if="notify" class="lb-group">
            <div class="lb-group__row st__row st__row--switch">
              <span class="lb-group__label">
                服务巡检自动恢复
                <LbInfoTip
                  :width="320"
                  text="面板每隔几分钟连一次每台机器,看 sing-box 与 nginx 还在不在跑。只在「服务确实没跑」时才动手 —— 那一刻没有在线连接会被踢掉,重启是零代价的。先直接拉起,拉不起来再重新下发配置。「配置有差异」永远不会触发自动部署:那会重启 sing-box,在你没准备好的时候断掉全部人。SSH 连不上时什么都不做。"
                />
              </span>
              <a-switch v-model:checked="notify.auto_recover" />
            </div>
            <div class="lb-group__row st__row st__row--switch">
              <span class="lb-group__label">
                消息推送
                <LbInfoTip text="总开关。关掉之后下面两个渠道都不发。" :width="240" />
              </span>
              <a-switch v-model:checked="notify.enabled" />
            </div>
            <div class="lb-group__row st__row st__row--top">
              <span class="lb-group__label">
                推送哪些事
                <LbInfoTip
                  :width="300"
                  text="全勾等于不限制,以后新增的事件类型会自动收到。「自动恢复成功」建议留着:只报警不报恢复的话,你半夜爬起来打开面板发现一切正常,下次就不会再爬起来了。"
                />
              </span>
              <div class="st__kinds">
                <button
                  v-for="k in notify.available_kinds"
                  :key="k.kind"
                  type="button"
                  class="st__kind"
                  :class="{ 'st__kind--on': chosenKinds.includes(k.kind) }"
                  :aria-pressed="chosenKinds.includes(k.kind)"
                  @click="toggleKind(k.kind)"
                >
                  {{ k.label }}
                </button>
              </div>
            </div>
          </div>

          <template v-if="notify">
            <!-- Bark -->
            <div class="st__sub-head">
              Bark
              <a-switch v-model:checked="notify.bark_enabled" size="small" />
              <span v-if="notify.bark_configured" class="lb-chip lb-chip--ok">已配置</span>
            </div>
            <div class="lb-group" :class="{ 'st__group--off': !notify.bark_enabled }">
              <div class="lb-group__row st__row">
                <span class="lb-group__label">
                  推送地址
                  <LbInfoTip
                    warn
                    :width="300"
                    text="整条地址就是凭据(设备 Key 在路径里),所以它主密钥加密存储、永远不回显,也不进日志与审计。留空表示不改;要清掉已保存的地址,点右侧「清除」。"
                  />
                </span>
                <div class="st__secret">
                  <a-input
                    v-model:value="secrets.bark_url"
                    :disabled="clearing.bark_url"
                    :placeholder="clearing.bark_url ? '保存后将清除已保存的地址' : notify.bark_configured ? '留空表示不改' : 'https://bark.example.com/你的设备Key'"
                  />
                  <button
                    v-if="notify.bark_configured"
                    type="button"
                    class="st__clear"
                    :class="{ 'st__clear--on': clearing.bark_url }"
                    @click="clearing.bark_url = !clearing.bark_url"
                  >
                    {{ clearing.bark_url ? '取消清除' : '清除' }}
                  </button>
                </div>
              </div>
              <div class="lb-group__row st__row">
                <span class="lb-group__label">分组</span>
                <a-input v-model:value="notify.bark_group" placeholder="LiteBox" />
              </div>
              <div class="lb-group__row st__row">
                <span class="lb-group__label">提示音</span>
                <a-input v-model:value="notify.bark_sound" placeholder="留空用 Bark 的默认音" />
              </div>
            </div>

            <!-- Telegram -->
            <div class="st__sub-head">
              Telegram
              <a-switch v-model:checked="notify.telegram_enabled" size="small" />
              <span v-if="notify.telegram_configured" class="lb-chip lb-chip--ok">已配置</span>
            </div>
            <div class="lb-group" :class="{ 'st__group--off': !notify.telegram_enabled }">
              <div class="lb-group__row st__row">
                <span class="lb-group__label">
                  API 地址
                  <LbInfoTip
                    warn
                    :width="300"
                    text="填到 sendMessage 之前那一段,方法名由面板拼。自建反代同理(https://tgapi.example.com/你的路径)。里面含 bot token,与 Bark 地址同级对待:加密存储、永远不回显。留空表示不改。"
                  />
                </span>
                <div class="st__secret">
                  <a-input
                    v-model:value="secrets.telegram_api_base"
                    :disabled="clearing.telegram_api_base"
                    :placeholder="clearing.telegram_api_base ? '保存后将清除已保存的地址' : notify.telegram_configured ? '留空表示不改' : 'https://api.telegram.org/bot你的Token'"
                  />
                  <button
                    v-if="notify.telegram_configured"
                    type="button"
                    class="st__clear"
                    :class="{ 'st__clear--on': clearing.telegram_api_base }"
                    @click="clearing.telegram_api_base = !clearing.telegram_api_base"
                  >
                    {{ clearing.telegram_api_base ? '取消清除' : '清除' }}
                  </button>
                </div>
              </div>
              <div class="lb-group__row st__row">
                <span class="lb-group__label">
                  代理密钥
                  <LbInfoTip text="走 X-TG-Proxy-Key 请求头,官方 API 不需要。留空表示不改。" :width="260" />
                </span>
                <div class="st__secret">
                  <a-input-password
                    v-model:value="secrets.telegram_proxy_key"
                    :disabled="clearing.telegram_proxy_key"
                    placeholder="官方 API 不需要"
                  />
                  <button
                    v-if="notify.telegram_configured"
                    type="button"
                    class="st__clear"
                    :class="{ 'st__clear--on': clearing.telegram_proxy_key }"
                    @click="clearing.telegram_proxy_key = !clearing.telegram_proxy_key"
                  >
                    {{ clearing.telegram_proxy_key ? '取消清除' : '清除' }}
                  </button>
                </div>
              </div>
              <div class="lb-group__row st__row st__row--pair">
                <span class="lb-group__label"><span class="st__req">*</span> chat_id / 话题 ID</span>
                <a-input v-model:value="notify.telegram_chat_id" placeholder="-1001234567890" />
                <a-input v-model:value="notify.telegram_thread_id" placeholder="留空发到主话题" />
              </div>
            </div>

            <div v-if="testResults.length" class="st__results">
              <div v-for="r in testResults" :key="r.channel" class="st__result">
                <span :class="r.ok ? 'st__ok' : 'st__danger'">{{ r.ok ? '成功' : '失败' }}</span>
                <b>{{ r.channel }}</b>
                <span v-if="r.error" class="st__result-err">{{ r.error }}</span>
              </div>
            </div>

            <div class="st__actions">
              <LbInfoTip
                class="st__actions-tip"
                :width="260"
                text="「发送测试」会先保存再发 —— 测的必须是你刚填进去的那份。测试消息不受上面的事件开关限制。"
              />
              <a-button size="small" class="lb-btn-ghost lb-btn-ghost--text" :loading="testingNotify" @click="testNotify">
                发送测试
              </a-button>
              <a-button type="primary" size="small" :loading="savingNotify" @click="saveNotify">保存</a-button>
            </div>
          </template>
        </div>
      </section>

      <!-- ③½ 到期与续费提醒(V20) -->
      <section id="set-expiry">
        <LbSectionTitle title="到期与续费提醒">
          <template #badge>
            <span class="lb-effect lb-effect--now">下一轮提醒起生效</span>
          </template>
        </LbSectionTitle>
        <div class="lb-card st__card">
          <div class="lb-group">
            <div class="lb-group__row st__row">
              <span class="lb-group__label">
                提前几天提醒
                <LbInfoTip
                  :width="340"
                  text="自建节点、外部代理、代理源登记了商家到期时间之后,按这里的天数提醒。逗号分隔,每个值 1~365。错过的提醒只补当前最相关的一次:系统恢复时只剩 2 天,不会同时补发「提前 7 天」和「提前 3 天」两条旧消息。单个对象可以在它自己的到期档案里覆盖这一项。走「监控与推送」里的渠道,三种到期事件在那里可以单独关掉。"
                />
              </span>
              <a-input
                v-model:value="expiryForm.lead_days"
                :placeholder="`留空用默认:${settings?.default_expiry_lead_days || '7,3,1'}`"
              />
            </div>
            <div class="lb-group__row st__row">
              <span class="lb-group__label">
                每天几点发
                <LbInfoTip :width="300" text="两位小时的 HH:MM。提醒按「到期日往前数 N 天那一天的这个时间」发;已到期的在到期之后的第一个这个时间发一次。" />
              </span>
              <a-input
                v-model:value="expiryForm.send_time"
                :placeholder="`留空用默认:${settings?.default_expiry_send_time || '09:00'}`"
                style="max-width: 160px"
              />
            </div>
            <div class="lb-group__row st__row">
              <span class="lb-group__label">
                时区
                <LbInfoTip :width="300" text="提醒时间与「自然月」按哪个时区解释(IANA 名)。留空跟随上面云账号里的时区。" />
              </span>
              <a-input
                v-model:value="expiryForm.timezone"
                :placeholder="`留空跟随云实例时区(当前生效:${settings?.effective_expiry_timezone || 'Asia/Shanghai'})`"
              />
            </div>
          </div>

          <div class="st__actions">
            <span v-if="expiryDirty" class="st__dirty">已改动</span>
            <a-button v-if="expiryDirty" size="small" class="lb-btn-ghost lb-btn-ghost--text" @click="restoreExpiry">
              还原
            </a-button>
            <a-button type="primary" size="small" :disabled="!expiryDirty" :loading="savingExpiry" @click="saveExpiry">
              保存到期提醒
            </a-button>
          </div>
        </div>
      </section>

      <!-- 公共探测目标(出站网络质量):已经搬到独立的「探测目标库」页,这里只留一个入口。
           锚点保留:旧的「去配置」链接与书签带着 #probe-public 进来时,至少能落在一句指路的话上。 -->
      <section id="set-probe-public">
        <LbSectionTitle title="公共探测目标(出站)" />
        <div class="lb-card set__pp" data-testid="public-probe-moved">
          <div>
            公共探测目标搬到了单独的「探测目标库」:自定义目标、Zstatic 预置、TcpQuality 同步、按地区 / 运营商 / 来源筛选、
            批量应用到节点,以及第三方来源的授权确认与限额都在那里。下面的「网络探测」仍是全局开关与告警设置。
          </div>
          <RouterLink to="/probe-library" class="set__pp-link">打开探测目标库</RouterLink>
        </div>
      </section>

      <!-- 网络探测:全局开关、告警,以及入口连通性的间隔与超时 -->
      <section id="set-tcpprobe">
        <LbSectionTitle title="网络探测">
          <template #badge>
            <span class="lb-effect lb-effect--now">下一轮探测起生效</span>
          </template>
        </LbSectionTitle>
        <div class="lb-card st__card">
          <div class="lb-group">
            <div class="lb-group__row st__row">
              <span class="lb-group__label">
                启用
                <LbInfoTip
                  :width="360"
                  text="两类探测一起开关:出站网络质量(每台机器在本机测上面的公共目标)与入口连通性(主控对节点详情里登记的端口做 TCP 建连)。打开仪表盘不会触发探测。关掉之后目标、开关与历史都保留,只是不再探测、不再告警。"
                />
              </span>
              <a-switch v-model:checked="tcpForm.enabled" />
            </div>
            <div class="lb-group__row st__row">
              <span class="lb-group__label">
                入口探测间隔(秒)
                <LbInfoTip :width="300" text="只管入口连通性(主控 → 节点),15~3600 秒;出站探测的间隔在每个公共目标上单独设。卡片上的点阵每个点至少 2 分钟,再勤只是在目标机器的连接日志里多几行。" />
              </span>
              <a-input
                v-model:value="tcpForm.interval"
                inputmode="numeric"
                :placeholder="`留空用默认:${settings?.default_probe_interval_sec ?? 60}`"
                style="max-width: 200px"
              />
            </div>
            <div class="lb-group__row st__row">
              <span class="lb-group__label">入口单次超时(毫秒)</span>
              <a-input
                v-model:value="tcpForm.timeout"
                inputmode="numeric"
                :placeholder="`留空用默认:${settings?.default_probe_timeout_ms ?? 2000}`"
                style="max-width: 200px"
              />
            </div>
            <div class="lb-group__row st__row">
              <span class="lb-group__label">
                连续失败几次推送
                <LbInfoTip
                  :width="320"
                  text="0~60,0 表示不推送,两类探测共用。连接失败、ICMP 整轮全丢与 DNS 解析失败算一次失败;部分丢包算可达,采集失败(面板连不上这台机器)、工具不可用、目标受限都不改变计数。进入失败只推一条、恢复推一条,面板重启不会重推。探测失败不会自动重启、重新部署或停机。"
                />
              </span>
              <a-input
                v-model:value="tcpForm.alert_after"
                inputmode="numeric"
                :placeholder="`留空用默认:${settings?.default_probe_alert_after ?? 3}`"
                style="max-width: 200px"
              />
            </div>
            <div class="lb-group__row st__row">
              <span class="lb-group__label">
                免打扰时段
                <LbInfoTip
                  :width="320"
                  text="HH:MM-HH:MM(可以跨午夜),按到期提醒的时区解释。时段里只记状态不推送;结束后还在失败的补推一条,已推过失败、在时段里恢复的补推一条恢复。"
                />
              </span>
              <a-input v-model:value="tcpForm.quiet" placeholder="例如 23:00-07:00,留空不设" style="max-width: 240px" />
            </div>
          </div>
          <div class="st__actions">
            <span v-if="tcpDirty" class="st__dirty">已改动</span>
            <a-button v-if="tcpDirty" size="small" class="lb-btn-ghost lb-btn-ghost--text" @click="restoreTcp">还原</a-button>
            <a-button type="primary" size="small" :disabled="!tcpDirty" :loading="savingTcp" @click="saveTcp">
              保存网络探测
            </a-button>
          </div>
        </div>
      </section>

      <!-- Boce 外部入站拨测(付费,V21) -->
      <section id="set-boce">
        <LbSectionTitle
          title="Boce 外部拨测"
          tip="付费的第三方入站拨测:从 Boce 各省各运营商(以及海外)的拨测点对节点的订阅入口做 TCPing 或 Ping,请求由主控发起,节点上什么都不装。默认关、没有预算时不创建任何付费检测;任务在节点详情的「网络监测」里配。"
        >
          <template #badge>
            <span class="lb-effect lb-effect--now">下一拍起生效</span>
          </template>
        </LbSectionTitle>
        <BoceSettingsCard />
      </section>

      <!-- 订阅排序方案(V20) -->
      <section id="set-order">
        <LbSectionTitle title="订阅排序">
          <template #badge>
            <span class="lb-effect lb-effect--now">用户下次拉订阅起生效</span>
          </template>
        </LbSectionTitle>
        <div class="lb-card st__card">
          <div class="lb-group">
            <div class="lb-group__row st__row">
              <span class="lb-group__label">
                当前方案
                <LbInfoTip
                  :width="360"
                  text="旧方案:先按机器的排序号、再按入口序号,外部代理整块排在自建节点之前或之后,插不进机器之间。全局排序:自建入口的全局排序值 = 节点排序号 × 1000 + 入口序号(节点 1~1000、入口 0~999),外部代理自带一个非负的全局排序值,直接与它比较;同值时自建在前。迁移会先把现有的相对顺序原样密排进新的取值范围,预览里逐行可见。"
                />
              </span>
              <span class="st__order-state">
                <b>{{ orderScheme === 'GLOBAL' ? '全局排序(GLOBAL)' : '旧方案(LEGACY)' }}</b>
                <span v-if="orderScheme !== 'GLOBAL' && settings" class="st__order-dim">
                  · 外部代理整块排在自建节点{{ settings.subscription_external_position === 'BEFORE' ? '之前' : '之后' }}
                </span>
              </span>
            </div>
          </div>
          <div class="st__actions">
            <a-button
              v-if="orderScheme === 'GLOBAL'"
              size="small"
              class="lb-btn-ghost lb-btn-ghost--text"
              :loading="switchingOrder"
              @click="switchBackLegacy"
            >
              切回旧方案
            </a-button>
            <a-button type="primary" size="small" @click="orderOpen = true">
              {{ orderScheme === 'GLOBAL' ? '重新密排(预览)' : '预览并迁移到全局排序' }}
            </a-button>
          </div>
        </div>
      </section>

      <!-- ④ 云账号(阿里云 CDT,V17) -->
      <section id="set-cloud">
        <LbSectionTitle title="云账号(阿里云 CDT)" effect="now" />
        <CloudAccountsPanel />
      </section>

      <!-- ⑤ 面板 SSH 公钥 -->
      <section id="set-key">
        <LbSectionTitle
          title="面板 SSH 公钥"
          effect="readonly"
          tip="新增节点时装进节点 authorized_keys 的就是这一行。面板对节点的所有操作都用它,轮换或吊销时不必动你自己的日常密钥。只允许密钥登录的节点没法用密码引导:先手工把这一行追加到节点的 ~/.ssh/authorized_keys,再在面板里新增节点并选「主控本机私钥」。这是公钥,可以公开 —— 与订阅地址不同,它贴到哪里都不构成泄露。"
        />
        <div class="lb-card st__card">
          <LbCopyField
            v-if="settings?.panel_public_key"
            :value="settings.panel_public_key"
          />
          <div v-else class="st__muted">尚未生成 —— 公钥在首次连接节点时自动生成。</div>
        </div>
      </section>

      <!-- ⑥ 访问等级 -->
      <section id="set-tier">
        <LbSectionTitle
          title="访问等级"
          effect="deploy"
          tip="等级是数值比较:节点 level ≤ 用户 level 即可用。程序内一律按 code 判断,名称只是显示用。删除等级前必须先把使用它的用户与节点迁走 —— 在用数量非 0 时不提供删除。"
        />
        <div class="lb-card lb-card--flush">
          <div class="st__tiers-scroll">
            <table class="st__tiers">
              <thead>
                <tr>
                  <th>code</th><th>名称</th><th>level</th><th>说明</th><th>在用</th><th />
                </tr>
              </thead>
              <tbody>
                <tr v-for="t in tiers" :key="t.id">
                  <td class="lb-mono st__tier-code">{{ t.code }}</td>
                  <td class="st__tier-name">{{ t.name }}</td>
                  <td class="lb-tabular">{{ t.level }}</td>
                  <td class="st__tier-desc">{{ t.description || '—' }}</td>
                  <td class="lb-tabular st__tier-use">{{ tierUsage(t).users }} 用户 / {{ tierUsage(t).inbounds }} 入口</td>
                  <td class="st__tier-act"><button type="button" class="lb-linkbtn" @click="openTier(t)">编辑</button></td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </section>

      <!-- ⑦ 管理员密码 -->
      <section id="set-pwd">
        <LbSectionTitle
          title="管理员密码"
          effect="sessions"
          tip="面板只有一个管理员账号,用户名不可改。新密码至少 8 位。修改后其他设备上的登录会话立即失效,当前设备保持登录;节点与用户配置不受影响。"
        />
        <div class="lb-card st__card">
          <!-- 这三个框都是「写入即消失」,但连着挂三枚「不会回显」徽标只是噪音,
               所以不套 LbSensitiveField —— 那个组件是为「留空即不变」那类语义准备的。 -->
          <div class="lb-group">
            <div class="lb-group__row st__row">
              <span class="lb-group__label">当前用户</span>
              <a-input :value="auth.admin?.username" disabled />
            </div>
            <div class="lb-group__row st__row">
              <span class="lb-group__label">原密码</span>
              <a-input-password v-model:value="pwd.old" autocomplete="current-password" />
            </div>
            <div class="lb-group__row st__row">
              <span class="lb-group__label">新密码</span>
              <a-input-password
                v-model:value="pwd.next"
                autocomplete="new-password"
                :placeholder="pwd.next ? `${pwd.next.length} 位` : '至少 8 位'"
              />
            </div>
            <div class="lb-group__row st__row">
              <span class="lb-group__label">确认新密码</span>
              <a-input-password v-model:value="pwd.confirm" autocomplete="new-password" />
            </div>
          </div>

          <!-- 就地校验,不用等提交。吐司飘在角上、输入框没有任何标记是最难查的那种错。 -->
          <div v-if="pwdError" class="lb-error-strip">{{ pwdError }}</div>

          <div class="st__actions">
            <a-button
              type="primary"
              danger
              size="small"
              :loading="savingPwd"
              :disabled="!pwd.old || !pwd.next || !!pwdError"
              @click="changePassword"
            >
              修改密码
            </a-button>
          </div>
        </div>
      </section>
    </div>
  </div>

  <!-- 编辑等级 Sheet:三栏头由 antd-tune.css 给,这里只放分组列表与一句注解。 -->
  <a-modal
    :open="editingTier !== null"
    :title="`编辑访问等级 · ${editingTier?.code}`"
    :width="460"
    :confirm-loading="savingTier"
    ok-text="保存"
    cancel-text="取消"
    @update:open="(v: boolean) => { if (!v) editingTier = null }"
    @ok="saveTier"
  >
    <div class="st__sheet">
      <div class="lb-group">
        <div class="lb-group__row st__row st__row--sheet">
          <span class="lb-group__label">
            code
            <LbInfoTip text="程序内按它判断,不可修改。" :width="220" />
          </span>
          <a-input :value="editingTier?.code" disabled />
        </div>
        <div class="lb-group__row st__row st__row--sheet">
          <span class="lb-group__label">
            名称
            <LbInfoTip text="只用于显示,改名不影响任何逻辑。" :width="220" />
          </span>
          <a-input v-model:value="tierForm.name" />
        </div>
        <div class="lb-group__row st__row st__row--sheet">
          <span class="lb-group__label">
            level
            <LbInfoTip
              :width="280"
              text="节点 level ≤ 用户 level 即可用。调高会让一批用户失去节点、调低会多开机器 —— 两种都会触发受影响节点自动重新部署。"
            />
          </span>
          <a-input-number v-model:value="tierForm.level" :min="0" :max="999" style="width: 160px" />
        </div>
        <div class="lb-group__row st__row st__row--sheet">
          <span class="lb-group__label">说明</span>
          <a-input v-model:value="tierForm.description" />
        </div>
      </div>
      <div class="st__sheet-note">改 level 会改变可用节点集合,受影响节点自动重新部署</div>
  </div>
  </a-modal>
  <OrderMigrationModal v-model:open="orderOpen" @applied="loadAll" />
</template>

<style scoped>
.set__pp {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-3);
  font-size: var(--fs-sm);
  color: var(--text2);
  line-height: 1.6;
}
.set__pp-link {
  flex: none;
  font-weight: 600;
}
.st {
  flex-direction: row;
  align-items: flex-start;
  flex-wrap: wrap;
}

.st__anchors {
  position: sticky;
  top: 112px;
  flex: none;
  width: 180px;
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding-top: 8px;
}

.st__anchors-title {
  padding: 8px 12px 6px;
  font-size: var(--fs-xs);
  font-weight: 500;
  color: var(--text3);
}

.st__anchor {
  height: 32px;
  padding: 0 12px;
  border: none;
  border-radius: var(--r-btn);
  background: transparent;
  color: var(--text2);
  font-size: var(--fs-sm);
  font-family: inherit;
  font-weight: 500;
  text-align: left;
  cursor: pointer;
  transition: background var(--dur-fast), color var(--dur-fast);
}

.st__anchor:hover {
  background: var(--fill);
  color: var(--text);
}

.st__anchor--on,
.st__anchor--on:hover {
  background: var(--brand-bg);
  color: var(--brand);
  font-weight: 600;
}

.st__main {
  flex: 1;
  min-width: 0;
  max-width: 820px;
  display: flex;
  flex-direction: column;
  gap: var(--sp-6);
}

.st__order-state {
  display: inline-flex;
  align-items: baseline;
  gap: 6px;
  font-size: var(--fs-base);
}
.st__order-dim {
  font-size: var(--fs-sm);
  color: var(--text3);
}

/*
 * 设置卡(UI 2.0):卡片里的分组列表不再自带边框 —— 卡片套一个带边框的分组是双层框,
 * 行与行改用细线分隔并贴满卡片左右;保存那一排是带 --surface2 底的卡片底栏。
 */
.st__card {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
  padding-top: var(--sp-2);
  padding-bottom: 0;
}
.st__card > .lb-group {
  margin: 0 calc(-1 * var(--sp-5));
  border: none;
  border-radius: 0;
  background: transparent;
}
.st__card > .lb-group :deep(.lb-group__row),
.st__card > .lb-group > .lb-group__row {
  padding-left: var(--sp-5);
  padding-right: var(--sp-5);
}
.st__card > .lb-group + .st__sub-head {
  margin-top: var(--sp-2);
}
.st__card > :last-child:not(.st__actions) {
  margin-bottom: var(--sp-5);
}

/* 分组列表里的输入框:白底、1px --sep、圆角 10。 */
.st__row :deep(.ant-input),
.st__row :deep(.ant-input-affix-wrapper),
.st__row :deep(.ant-input-number) {
  width: 100%;
}
.st__row :deep(.ant-input-number) {
  width: 160px;
}
.st__row--switch {
  grid-template-columns: minmax(0, 1fr) auto;
}
.st__row--top {
  align-items: start;
  padding-top: 12px;
  padding-bottom: 12px;
}
.st__row--top .lb-group__label {
  padding-top: 6px;
}
.st__row--pair {
  grid-template-columns: minmax(110px, 150px) minmax(0, 1fr) minmax(0, 1fr);
}
.st__row--sheet {
  grid-template-columns: 110px minmax(0, 1fr);
}

.st__label--muted {
  color: var(--text2);
}

.st__req {
  color: var(--bad);
}

.st__code {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--text2);
  font-size: var(--fs-sm);
}

.st__muted {
  font-size: var(--fs-sm);
  color: var(--text3);
}

/* 事件 chip 组:选中 --brand-bg / --brand,未选 --fill / --text3。 */
.st__kinds {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}
/* 事件开关:细边框矩形,选中 = 品牌浅底 + 品牌描边(与筛选条上的开关同一种形态)。 */
.st__kind {
  padding: 4px 10px;
  border: 1px solid var(--sep);
  border-radius: var(--r-btn);
  background: var(--surface);
  color: var(--text3);
  font-size: var(--fs-sm);
  font-weight: 500;
  font-family: inherit;
  cursor: pointer;
  transition: background var(--dur-fast), color var(--dur-fast), border-color var(--dur-fast);
}
.st__kind:hover {
  border-color: var(--fill2);
  color: var(--text);
}
.st__kind--on,
.st__kind--on:hover {
  background: var(--brand-bg);
  border-color: var(--brand-border);
  color: var(--brand);
}

.st__sub-head {
  display: flex;
  align-items: center;
  gap: 10px;
  margin: 0 calc(-1 * var(--sp-5));
  padding: 10px var(--sp-5) 0;
  border-top: 1px solid var(--sep2);
  font-size: var(--fs-base);
  font-weight: 600;
}

.st__group--off {
  opacity: 0.7;
}

.st__secret {
  display: flex;
  gap: 8px;
  min-width: 0;
}
.st__secret :deep(.ant-input-affix-wrapper),
.st__secret :deep(.ant-input) {
  flex: 1;
  min-width: 0;
}
.st__clear {
  flex: none;
  height: 36px;
  padding: 0 14px;
  border: none;
  border-radius: var(--r-input);
  background: var(--fill);
  color: var(--bad);
  font-size: var(--fs-sm);
  font-weight: 500;
  font-family: inherit;
  cursor: pointer;
  transition: background 0.2s;
}
.st__clear:hover {
  background: var(--fill2);
}
.st__clear--on {
  background: var(--bad-bg);
}

.st__results {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 12px 14px;
  background: var(--surface2);
  border-radius: var(--r-group);
}
.st__result {
  display: flex;
  align-items: baseline;
  gap: 8px;
  font-size: var(--fs-sm);
}
.st__result-err {
  color: var(--bad);
}
.st__ok {
  color: var(--ok);
  font-weight: 600;
}
.st__danger {
  color: var(--bad);
  font-weight: 600;
}

.st__actions {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 10px;
  margin: var(--sp-1) calc(-1 * var(--sp-5)) 0;
  padding: 10px var(--sp-5);
  border-top: 1px solid var(--sep2);
  background: var(--surface2);
  border-radius: 0 0 var(--r-card) var(--r-card);
}
.st__actions-tip {
  margin-right: auto;
}

.st__dirty {
  font-size: var(--fs-sm);
  font-weight: 500;
  color: var(--warn);
}

.st__tiers-scroll {
  overflow-x: auto;
}
.st__tiers {
  width: 100%;
  min-width: 640px;
  border-collapse: collapse;
  font-size: var(--fs-sm);
}
.st__tiers th {
  padding: 12px;
  text-align: left;
  font-size: var(--fs-xs);
  font-weight: 500;
  color: var(--text3);
  border-bottom: 1px solid var(--sep2);
}
.st__tiers th:first-child,
.st__tiers td:first-child {
  padding-left: 22px;
}
.st__tiers th:last-child,
.st__tiers td:last-child {
  padding-right: 22px;
}
.st__tiers td {
  padding: 14px 12px;
  border-bottom: 1px solid var(--sep2);
  vertical-align: middle;
}
.st__tiers tbody tr:last-child td {
  border-bottom: none;
}
.st__tiers tbody tr {
  transition: background 0.15s;
}
.st__tiers tbody tr:hover {
  background: var(--surface2);
}
.st__tier-code {
  font-size: var(--fs-sm);
  color: var(--text2);
}
.st__tier-name {
  font-weight: 600;
}
.st__tier-desc {
  color: var(--text2);
}
.st__tier-use {
  white-space: nowrap;
  color: var(--text2);
}
.st__tier-act {
  text-align: right;
}
.st__tier-act .lb-linkbtn {
  font-weight: 500;
}

.st__sheet {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.st__sheet-note {
  font-size: var(--fs-sm);
  color: var(--text3);
  text-align: center;
}

@media (max-width: 1279px) {
  .st {
    flex-direction: column;
  }

  .st__anchors {
    display: none;
  }

  .st__main {
    max-width: none;
    width: 100%;
  }
}

@media (max-width: 767px) {
  .st__row--switch {
    grid-template-columns: minmax(0, 1fr) auto;
  }
  .st__row--pair {
    grid-template-columns: 1fr;
  }
}
</style>
