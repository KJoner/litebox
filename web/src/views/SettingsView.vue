<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
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
import { LbCopyField, LbEmptyState } from '@/components/lb'

/**
 * 系统设置。全站唯一的「分组表单」页,也是唯一需要「还原」按钮的地方 ——
 * 其余表单都在弹窗里,关掉即放弃。
 *
 * **每组一个保存按钮,不做全局保存。** 三组的生效方式完全不同:
 * 订阅地址改完立即生效、等级改动会触发重新部署、密码修改会撤销会话。
 * 一个全局「保存」意味着管理员改个域名就顺手把等级也提交了,而那会重启一批节点。
 *
 * 左侧锚点导航而不是 Tab:四组内容都不长,Tab 会让人为了确认一个值来回切;
 * 锚点让整页可滚、可 Ctrl+F。
 */
const auth = useAuthStore()
const router = useRouter()

const settings = ref<PanelSettings | null>(null)
const tiers = ref<AccessTier[]>([])
const users = ref<ProxyUser[]>([])
const nodes = ref<Node[]>([])
const loadError = ref<string>('')

const sections = [
  { id: 'sub', label: '订阅地址' },
  { id: 'notify', label: '监控与推送' },
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

onMounted(loadAll)
</script>

<template>
  <div class="st">
    <!-- 1280 以下锚点栏收起,四组直接顺排。 -->
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
      <div class="st__head">
        <h2 class="st__title">系统设置</h2>
        <div class="st__sub">每一组单独保存。改动的生效方式各不相同,写在各组底部。</div>
      </div>

      <LbEmptyState v-if="loadError" variant="error" :title="loadError" @retry="loadAll" />

      <!-- ① 订阅地址 -->
      <section id="set-sub" class="st__card">
        <div class="st__card-head">
          <span>订阅地址</span>
          <span class="st__badge st__badge--ok">改完立即生效</span>
        </div>
        <div class="st__card-body">
          <div class="st__field">
            <label class="st__label"><span class="st__req">*</span> 站点根地址</label>
            <a-input v-model:value="baseURL" placeholder="https://box.example.com" />
            <div class="st__help">
              用户订阅链接的前缀,必须带 <code>http://</code> 或 <code>https://</code>。
            </div>
          </div>

          <div class="st__inner">
            <div class="st__inner-label">生成后的地址形如</div>
            <div class="lb-mono st__inner-code">
              {{ baseURL || settings?.config_base_url || 'https://box.example.com' }}/sub/&lt;token&gt;
            </div>
            <div class="st__inner-note">
              配置文件里的默认值是
              <code>{{ settings?.config_base_url || 'http://127.0.0.1:8080' }}</code>,留空则用它 ——
              那个地址只能在本机访问,用户拉不到订阅。
            </div>
          </div>

          <div class="st__note st__note--info">
            <strong>改域名不会让已发出的订阅失效。</strong>
            订阅 Token 不变,用户不必重新导入;但客户端在下次拉订阅之前用的仍是旧地址,
            旧域名要留一段时间。
          </div>

          <div class="st__actions">
            <span v-if="baseURLDirty" class="st__dirty">已改动</span>
            <a-button v-if="baseURLDirty" size="small" @click="baseURL = savedBaseURL">还原</a-button>
            <a-button
              type="primary"
              :disabled="!baseURLDirty"
              :loading="savingBaseURL"
              @click="saveBaseURL"
            >
              保存订阅地址
            </a-button>
          </div>
        </div>
      </section>


      <!-- ② 监控与推送 -->
      <section id="set-notify" class="st__card">
        <div class="st__card-head">
          <span>监控与推送</span>
          <span class="st__badge st__badge--ok">改完立即生效</span>
        </div>
        <div class="st__card-body">
          <div v-if="notifyError" class="st__note st__note--danger">{{ notifyError }}</div>

          <div class="st__field">
            <label class="st__label">服务巡检自动恢复</label>
            <a-switch v-if="notify" v-model:checked="notify.auto_recover" />
            <div class="st__help">
              面板每隔几分钟连一次每台机器,看 sing-box 与 nginx 还在不在跑。
              <strong>只在「服务确实没跑」时才动手</strong> —— 那一刻没有在线连接会被踢掉,
              所以重启是零代价的。先直接拉起,拉不起来再重新下发配置。
              <br />
              「配置有差异」<strong>永远不会</strong>触发自动部署:那会重启 sing-box,
              在你没准备好的时候断掉全部人。
              <br />
              SSH 连不上时什么都不做 —— 那时候服务是死是活我们并不知道,机器可能只是在重启。
            </div>
          </div>

          <div class="st__field">
            <label class="st__label">消息推送</label>
            <a-switch v-if="notify" v-model:checked="notify.enabled" />
            <div class="st__help">总开关。关掉之后下面两个渠道都不发。</div>
          </div>

          <template v-if="notify">
            <div class="st__field">
              <label class="st__label">推送哪些事</label>
              <a-checkbox-group v-model:value="chosenKinds">
                <a-checkbox v-for="k in notify.available_kinds" :key="k.kind" :value="k.kind">
                  {{ k.label }}
                </a-checkbox>
              </a-checkbox-group>
              <div class="st__help">
                「自动恢复成功」建议留着:只报警不报恢复的话,你半夜爬起来打开面板发现
                一切正常,下次就不会再爬起来了。
              </div>
            </div>

            <!-- Bark -->
            <div class="st__sub-head">
              Bark
              <a-switch v-model:checked="notify.bark_enabled" size="small" />
              <span v-if="notify.bark_configured" class="st__ok-chip">已配置</span>
            </div>
            <a-form layout="vertical">
              <a-form-item label="推送地址">
                <a-input
                  v-model:value="secrets.bark_url"
                  :disabled="clearing.bark_url"
                  placeholder="https://bark.example.com/你的设备Key"
                />
                <div class="st__help">
                  <strong>整条地址就是凭据</strong>(设备 Key 在路径里),所以它主密钥加密存储、
                  永远不回显,也不进日志与审计。
                  <span v-if="notify.bark_configured">留空表示不改。</span>
                  <a-checkbox v-model:checked="clearing.bark_url" class="st__clear">
                    清除已保存的地址
                  </a-checkbox>
                </div>
              </a-form-item>
              <a-row :gutter="12">
                <a-col :span="12">
                  <a-form-item label="分组">
                    <a-input v-model:value="notify.bark_group" placeholder="LiteBox" />
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item label="提示音">
                    <a-input v-model:value="notify.bark_sound" placeholder="留空用 Bark 的默认音" />
                  </a-form-item>
                </a-col>
              </a-row>
            </a-form>

            <!-- Telegram -->
            <div class="st__sub-head">
              Telegram
              <a-switch v-model:checked="notify.telegram_enabled" size="small" />
              <span v-if="notify.telegram_configured" class="st__ok-chip">已配置</span>
            </div>
            <a-form layout="vertical">
              <a-form-item label="API 地址">
                <a-input
                  v-model:value="secrets.telegram_api_base"
                  :disabled="clearing.telegram_api_base"
                  placeholder="https://api.telegram.org/bot你的Token"
                />
                <div class="st__help">
                  填到 <code>sendMessage</code> <strong>之前</strong>那一段,方法名由面板拼。
                  自建反代同理(<code>https://tgapi.example.com/你的路径</code>)。
                  里面含 bot token,与 Bark 地址同级对待。
                  <a-checkbox v-model:checked="clearing.telegram_api_base" class="st__clear">
                    清除已保存的地址
                  </a-checkbox>
                </div>
              </a-form-item>
              <a-form-item label="代理密钥">
                <a-input-password
                  v-model:value="secrets.telegram_proxy_key"
                  :disabled="clearing.telegram_proxy_key"
                  placeholder="走 X-TG-Proxy-Key 请求头,官方 API 不需要"
                />
                <div class="st__help">
                  <a-checkbox v-model:checked="clearing.telegram_proxy_key" class="st__clear">
                    清除已保存的密钥
                  </a-checkbox>
                </div>
              </a-form-item>
              <a-row :gutter="12">
                <a-col :span="12">
                  <a-form-item label="chat_id" required>
                    <a-input v-model:value="notify.telegram_chat_id" placeholder="-1001234567890" />
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item label="话题 ID">
                    <a-input
                      v-model:value="notify.telegram_thread_id"
                      placeholder="留空发到主话题"
                    />
                  </a-form-item>
                </a-col>
              </a-row>
            </a-form>

            <div v-if="testResults.length" class="st__results">
              <div v-for="r in testResults" :key="r.channel" class="st__result">
                <span :class="r.ok ? 'st__ok' : 'st__danger'">{{ r.ok ? '成功' : '失败' }}</span>
                <b>{{ r.channel }}</b>
                <span v-if="r.error" class="st__result-err">{{ r.error }}</span>
              </div>
            </div>

            <div class="st__note">
              「发送测试」会<strong>先保存再发</strong> —— 测的必须是你刚填进去的那份。
              测试消息不受上面的事件开关限制。
            </div>

            <div class="st__actions">
              <a-button :loading="testingNotify" @click="testNotify">发送测试</a-button>
              <a-button type="primary" :loading="savingNotify" @click="saveNotify">保存</a-button>
            </div>
          </template>
        </div>
      </section>

      <!-- ② 面板 SSH 公钥 -->
      <section id="set-key" class="st__card">
        <div class="st__card-head">
          <span>面板 SSH 公钥</span>
          <span class="st__badge">只读</span>
        </div>
        <div class="st__card-body">
          <div class="st__help st__help--lead">
            新增节点时装进节点 <code>authorized_keys</code> 的就是这一行。面板对节点的所有操作都用它,
            轮换或吊销时不必动你自己的日常密钥。
          </div>

          <LbCopyField
            v-if="settings?.panel_public_key"
            :value="settings.panel_public_key"
          />
          <div v-else class="st__note">
            尚未生成 —— 公钥在首次连接节点时自动生成。
          </div>

          <div class="st__note st__note--warn">
            <strong>只允许密钥登录的节点</strong>没法用密码引导。先手工把上面这行追加到节点的
            <code>~/.ssh/authorized_keys</code>,再在面板里新增节点并选「主控本机私钥」,
            或直接点详情里的「重新引导」。
          </div>

          <div class="st__help">
            这一行是公钥,可以公开 —— 与订阅地址不同,它贴到哪里都不构成泄露。
          </div>
        </div>
      </section>

      <!-- ③ 访问等级 -->
      <section id="set-tier" class="st__card">
        <div class="st__card-head">
          <span>访问等级</span>
          <span class="st__badge st__badge--warn">改动会触发重新部署</span>
        </div>
        <div class="st__card-body">
          <div class="st__help st__help--lead">
            等级是数值比较:<strong>节点 level ≤ 用户 level 即可用</strong>。
            程序内一律按 code 判断,名称只是显示用,改名不影响任何逻辑。
          </div>

          <div class="st__tiers">
            <div class="st__tier st__tier--head">
              <span>code</span><span>名称</span><span>level</span><span>说明</span><span>在用</span><span />
            </div>
            <div v-for="t in tiers" :key="t.id" class="st__tier">
              <span class="lb-mono">{{ t.code }}</span>
              <span>{{ t.name }}</span>
              <span class="lb-mono">{{ t.level }}</span>
              <span class="lb-ellipsis" :title="t.description">{{ t.description || '—' }}</span>
              <span class="lb-mono st__tier-use">
                {{ tierUsage(t).users }} 用户 / {{ tierUsage(t).inbounds }} 入口
              </span>
              <span><a @click="openTier(t)">编辑</a></span>
            </div>
          </div>

          <div class="st__help">
            删除等级前必须先把使用它的用户与节点迁走 —— 在用数量非 0 时不提供删除。
          </div>
        </div>
      </section>

      <!-- ④ 管理员密码 -->
      <section id="set-pwd" class="st__card">
        <div class="st__card-head">
          <span>管理员密码</span>
          <span class="st__badge st__badge--danger">会撤销其他会话</span>
        </div>
        <div class="st__card-body">
          <div class="st__field">
            <label class="st__label">当前用户</label>
            <a-input :value="auth.admin?.username" disabled />
            <div class="st__help">面板只有一个管理员账号,用户名不可改。</div>
          </div>

          <!-- 这三个框都是「写入即消失」,但连着挂三枚「不会回显」徽标只是噪音,
               所以不套 LbSensitiveField —— 那个组件是为「留空即不变」那类语义准备的。 -->
          <a-form layout="vertical" class="st__pwd">
            <a-form-item label="原密码" required>
              <a-input-password v-model:value="pwd.old" autocomplete="current-password" />
            </a-form-item>
            <a-form-item label="新密码" required>
              <a-input-password v-model:value="pwd.next" autocomplete="new-password" />
              <div class="st__help">
                {{ pwd.next ? `${pwd.next.length} 位` : '至少 8 位。' }}
              </div>
            </a-form-item>
            <a-form-item label="确认新密码" required>
              <a-input-password v-model:value="pwd.confirm" autocomplete="new-password" />
            </a-form-item>
          </a-form>

          <!-- 就地校验,不用等提交。吐司飘在角上、输入框没有任何标记是最难查的那种错。 -->
          <div v-if="pwdError" class="st__note st__note--danger">{{ pwdError }}</div>

          <div class="st__note">
            修改后其他设备上的登录会话立即失效,当前设备保持登录。节点与用户配置不受影响。
          </div>

          <div class="st__actions">
            <a-button
              type="primary"
              danger
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
    <a-form layout="vertical">
      <a-form-item label="code">
        <a-input :value="editingTier?.code" disabled />
        <div class="st__help">程序内按它判断,不可修改。</div>
      </a-form-item>
      <a-form-item label="名称">
        <a-input v-model:value="tierForm.name" />
        <div class="st__help">只用于显示,改名不影响任何逻辑。</div>
      </a-form-item>
      <a-form-item label="level">
        <a-input-number v-model:value="tierForm.level" :min="0" :max="999" style="width: 160px" />
        <div class="st__help">
          节点 level ≤ 用户 level 即可用。<strong>调高会让一批用户失去节点、调低会多开机器</strong> ——
          两种都会触发受影响节点自动重新部署。
        </div>
      </a-form-item>
      <a-form-item label="说明">
        <a-input v-model:value="tierForm.description" />
      </a-form-item>
    </a-form>
  </a-modal>
</template>

<style scoped>
.st {
  display: flex;
  align-items: flex-start;
  gap: 20px;
}

.st__anchors {
  position: sticky;
  top: 20px;
  flex: none;
  width: 180px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.st__anchors-title {
  padding: 6px 10px 4px;
  font-size: 10.5px;
  font-weight: 600;
  letter-spacing: 0.06em;
  color: #6b7480;
}

.st__anchor {
  height: 30px;
  padding: 0 10px;
  border: none;
  border-radius: 4px;
  background: transparent;
  color: #576070;
  font-size: 12.5px;
  font-family: inherit;
  text-align: left;
  cursor: pointer;
}

.st__anchor:hover {
  background: #f1f3f5;
}

.st__anchor--on {
  background: #eef4fc;
  color: #1d4f96;
  font-weight: 500;
}

.st__main {
  flex: 1;
  min-width: 0;
  max-width: 820px;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.st__head {
  display: flex;
  flex-direction: column;
  gap: 3px;
}

.st__title {
  margin: 0;
  font-size: 19px;
  font-weight: 600;
}

.st__sub {
  font-size: 12.5px;
  color: #6b7480;
}

.st__card {
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

.st__card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 16px;
  border-bottom: 1px solid #edeff2;
  font-size: 13px;
  font-weight: 600;
}

/* 生效方式的颜色跟着后果走:绿=立即、琥珀=需部署、红=会撤销会话。 */
.st__badge {
  padding: 1px 7px;
  border: 1px solid #dfe3e8;
  border-radius: 3px;
  background: #f1f3f5;
  font-size: 11px;
  font-weight: 500;
  color: #5c6672;
}

.st__badge--ok {
  background: #e9f5ee;
  border-color: #c3e3d0;
  color: #1b7a4b;
}

.st__badge--warn {
  background: #fcf3e3;
  border-color: #efdcb4;
  color: #92610a;
}

.st__badge--danger {
  background: #fdecea;
  border-color: #f3cfc9;
  color: #b4291d;
}

.st__card-body {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 16px;
}

.st__field {
  display: flex;
  flex-direction: column;
  gap: 6px;
  /* 输入框要占满一行,但开关不能 —— 它是点选控件,
     被 flex 拉满之后既难看,命中区也变成一整行。 */
  align-items: flex-start;
}
.st__field > :deep(.ant-input),
.st__field > :deep(.ant-input-affix-wrapper),
.st__field > :deep(.ant-select) {
  width: 100%;
}
.st__field > .st__help,
.st__field > .st__label {
  width: 100%;
}

.st__label {
  font-size: 13px;
  font-weight: 500;
}

.st__req {
  color: #b4291d;
}

.st__help {
  font-size: 12px;
  line-height: 1.65;
  color: #6b7480;
}

.st__help--lead {
  font-size: 12.5px;
  color: #576070;
  line-height: 1.75;
}

.st__help code,
.st__inner-note code,
.st__note code {
  padding: 1px 5px;
  background: #f1f3f5;
  border-radius: 3px;
  font-family: 'IBM Plex Mono', ui-monospace, monospace;
  font-size: 12px;
}

.st__inner {
  display: flex;
  flex-direction: column;
  gap: 7px;
  padding: 11px 13px;
  background: #f6f7f9;
  border: 1px solid #edeff2;
  border-radius: 6px;
}

.st__inner-label {
  font-size: 11.5px;
  color: #6b7480;
}

.st__inner-code {
  font-size: 12px;
  color: #15181c;
  overflow-x: auto;
}

.st__inner-note {
  padding-top: 7px;
  border-top: 1px solid #e3e6ea;
  font-size: 11.5px;
  line-height: 1.65;
  color: #6b7480;
}

.st__note {
  padding: 11px 13px;
  background: #f6f7f9;
  border: 1px solid #edeff2;
  border-radius: 6px;
  font-size: 11.5px;
  line-height: 1.7;
  color: #576070;
}

.st__note--info {
  background: #eef4fc;
  border-color: #c9dcf3;
  color: #1d4f96;
}

.st__note--warn {
  background: #fcf3e3;
  border-color: #efdcb4;
  color: #5c4405;
}

.st__note--danger {
  background: #fdecea;
  border-color: #f3cfc9;
  color: #8e2117;
}

.st__sub-head {
  display: flex;
  align-items: center;
  gap: 10px;
  margin: 18px 0 8px;
  font-weight: 600;
  border-top: 1px solid #E3E6EA;
  padding-top: 14px;
}
.st__ok-chip {
  font-size: 12px;
  font-weight: 400;
  color: #1B7A4B;
}
.st__clear {
  display: inline-flex;
  align-items: center;
  margin-top: 6px;
}
.st__results {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin: 12px 0;
}
.st__result {
  display: flex;
  align-items: baseline;
  gap: 8px;
  font-size: 13px;
}
.st__result-err {
  color: #B4291D;
}
.st__ok {
  color: #1B7A4B;
}
.st__danger {
  color: #B4291D;
}

.st__actions {
  display: flex;
  align-items: center;
  gap: 8px;
  padding-top: 4px;
  border-top: 1px solid #edeff2;
  margin-top: 2px;
  padding-top: 14px;
}

.st__dirty {
  margin-right: auto;
  font-size: 11.5px;
  color: #92610a;
}

.st__tiers {
  border: 1px solid #edeff2;
  border-radius: 6px;
  overflow: hidden;
}

.st__tier {
  display: grid;
  grid-template-columns: 0.7fr 0.8fr 0.5fr 1.4fr 1.1fr 0.5fr;
  align-items: center;
  gap: 10px;
  padding: 9px 12px;
  font-size: 12.5px;
}

.st__tier + .st__tier {
  border-top: 1px solid #edeff2;
}

.st__tier--head {
  background: #f6f7f9;
  font-size: 11px;
  font-weight: 600;
  color: #576070;
}

.st__tier-use {
  font-size: 11px;
  color: #6b7480;
}

.st__pwd :deep(.ant-form-item) {
  margin-bottom: 14px;
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
  .st__tier {
    grid-template-columns: 1fr 1fr;
  }
}
</style>
