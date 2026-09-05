<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { message, Modal } from 'ant-design-vue'
import {
  api,
  ApiError,
  type CloudAccount,
  type CloudTestResult,
  type PanelSettings,
} from '@/api/client'
import { LbEmptyState, LbQuotaBar, LbTimeText } from '@/components/lb'
import { formatBytes } from '@/utils/format'
import { cloudUsageLevel, GIB } from './cloudMeta'

/**
 * 设置页里的「云账号」区块(V17)。
 *
 * 一个账号 = 一对阿里云 AccessKey。CDT 的计数器是账号级 × 业务区域的,
 * 所以额度与阈值挂在这里(国际 / 内地两个池子),而"超了怎么办"挂在节点上。
 * Secret 永远不回显:编辑时留空表示不改。
 */
const accounts = ref<CloudAccount[]>([])
const lastRun = ref('')
const loading = ref(false)
const loadError = ref('')

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const r = await api.cloudAccounts()
    accounts.value = r.items
    lastRun.value = r.last_run
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '加载云账号失败'
  } finally {
    loading.value = false
  }
}

// ---------- 时区与轮询间隔 ----------

const settings = ref<PanelSettings | null>(null)
const tz = ref('')
const pollSec = ref<number | null>(null)
const savedTz = ref('')
const savedPoll = ref<number | null>(null)
const savingSettings = ref(false)
const settingsDirty = computed(() => tz.value !== savedTz.value || pollSec.value !== savedPoll.value)

async function loadSettings() {
  try {
    const s = await api.settings()
    settings.value = s
    tz.value = s.cloud_timezone
    savedTz.value = s.cloud_timezone
    pollSec.value = s.cloud_poll_interval_sec || null
    savedPoll.value = s.cloud_poll_interval_sec || null
  } catch {
    settings.value = null
  }
}

async function saveSettings() {
  savingSettings.value = true
  try {
    const s = await api.updateSettings({
      cloud_timezone: tz.value,
      cloud_poll_interval_sec: pollSec.value ?? 0,
    })
    settings.value = s
    tz.value = s.cloud_timezone
    savedTz.value = s.cloud_timezone
    pollSec.value = s.cloud_poll_interval_sec || null
    savedPoll.value = s.cloud_poll_interval_sec || null
    message.success('已保存,下一轮轮询起生效')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    savingSettings.value = false
  }
}

// ---------- 新建 / 编辑 ----------

const editing = ref<CloudAccount | null>(null)
const open = ref(false)
const saving = ref(false)
const formError = ref('')
const form = reactive({
  name: '',
  access_key_id: '',
  access_key_secret: '',
  quota_intl_gib: 200,
  quota_cn_gib: 20,
  threshold_percent: 90,
  enabled: true,
})

function openCreate() {
  editing.value = null
  Object.assign(form, {
    name: '',
    access_key_id: '',
    access_key_secret: '',
    quota_intl_gib: 200,
    quota_cn_gib: 20,
    threshold_percent: 90,
    enabled: true,
  })
  formError.value = ''
  testResult.value = null
  open.value = true
}

function openEdit(a: CloudAccount) {
  editing.value = a
  Object.assign(form, {
    name: a.name,
    access_key_id: a.access_key_id,
    access_key_secret: '',
    quota_intl_gib: Math.round((a.cdt_quota_intl_bytes / GIB) * 100) / 100,
    quota_cn_gib: Math.round((a.cdt_quota_cn_bytes / GIB) * 100) / 100,
    threshold_percent: a.threshold_percent,
    enabled: a.enabled,
  })
  formError.value = ''
  testResult.value = null
  open.value = true
}

function payload() {
  const body: Record<string, unknown> = {
    name: form.name,
    access_key_id: form.access_key_id.trim(),
    cdt_quota_intl_bytes: Math.round(form.quota_intl_gib * GIB),
    cdt_quota_cn_bytes: Math.round(form.quota_cn_gib * GIB),
    threshold_percent: form.threshold_percent,
    enabled: form.enabled,
  }
  // 留空 = 不改。新建时后端会要求它非空。
  if (form.access_key_secret.trim() !== '') body.access_key_secret = form.access_key_secret.trim()
  return body
}

async function submit() {
  saving.value = true
  formError.value = ''
  try {
    if (editing.value) {
      await api.updateCloudAccount(editing.value.id, payload())
      message.success('已保存')
    } else {
      await api.createCloudAccount(payload())
      message.success('云账号已添加,下一轮轮询会开始采样;也可以在列表里点「刷新」立刻采一次')
    }
    form.access_key_secret = ''
    open.value = false
    await load()
  } catch (err) {
    formError.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    saving.value = false
  }
}

// ---------- 测试 ----------

const testing = ref(false)
const testResult = ref<CloudTestResult | null>(null)

/** 用表单里的凭据(或库里的 Secret)当场查一次 CDT 用量 —— 那正是管理员想确认的东西。 */
async function test() {
  testing.value = true
  testResult.value = null
  formError.value = ''
  try {
    const body: { account_id?: number; access_key_id?: string; access_key_secret?: string } = {
      access_key_id: form.access_key_id.trim(),
      access_key_secret: form.access_key_secret.trim(),
    }
    if (editing.value && !body.access_key_secret) body.account_id = editing.value.id
    testResult.value = await api.testCloudAccount(body)
  } catch (err) {
    formError.value = err instanceof ApiError ? err.message : '测试失败'
  } finally {
    testing.value = false
  }
}

// ---------- 列表操作 ----------

const busy = ref<Record<number, string>>({})

async function refresh(a: CloudAccount) {
  busy.value[a.id] = 'refresh'
  try {
    await api.refreshCloudAccount(a.id)
    message.success('已重新采样')
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '刷新失败')
  } finally {
    delete busy.value[a.id]
  }
}

function remove(a: CloudAccount) {
  Modal.confirm({
    title: `删除云账号「${a.name}」?`,
    content:
      a.bound_nodes > 0
        ? `还有 ${a.bound_nodes} 台节点绑定在这个账号上,先到节点编辑里解绑。`
        : '面板不再轮询这个账号。历史采样一并删除,节点不受影响。',
    okText: '删除',
    okType: 'danger',
    cancelText: '取消',
    onOk: async () => {
      try {
        await api.deleteCloudAccount(a.id)
        message.success('已删除')
        await load()
      } catch (err) {
        message.error(err instanceof ApiError ? err.message : '删除失败')
      }
    },
  })
}

onMounted(() => {
  void load()
  void loadSettings()
})
</script>

<template>
  <div class="ca">
    <div class="ca__lead">
      CDT(云数据传输)的计数器是<strong>账号级</strong>的:阿里云按业务区域给出这个账号本月的累计公网流量,
      里面没有实例维度。同一个账号下几台实例共用同一个池子,所以额度与阈值填在账号上,
      「超了怎么办」在每台节点的编辑里选。国际站账号默认每月国际区域 200 GiB、内地区域 20 GiB 免费;
      CDT 的数据有延迟,阈值留余量。
    </div>

    <LbEmptyState v-if="loadError" variant="error" :title="loadError" @retry="load" />
    <LbEmptyState
      v-else-if="!loading && accounts.length === 0"
      variant="empty"
      title="还没有云账号"
      description="添加一对只授予 CDT 只读与 ECS 查询 / 启停权限的 RAM 子账号 AccessKey。"
    >
      <template #action>
        <a-button type="primary" @click="openCreate">添加云账号</a-button>
      </template>
    </LbEmptyState>

    <template v-else>
      <div v-for="a in accounts" :key="a.id" class="ca__item">
        <div class="ca__head">
          <span class="ca__name">{{ a.name }}</span>
          <span class="lb-mono ca__ak">{{ a.access_key_id_masked }}</span>
          <span v-if="!a.enabled" class="ca__chip ca__chip--off">已停用</span>
          <span v-if="a.state.consecutive_failures >= 3" class="ca__chip ca__chip--bad">
            连续 {{ a.state.consecutive_failures }} 轮查不到
          </span>
          <span class="ca__spacer" />
          <a-button size="small" :loading="busy[a.id] === 'refresh'" @click="refresh(a)">刷新</a-button>
          <a-button size="small" @click="openEdit(a)">编辑</a-button>
          <a-button size="small" danger @click="remove(a)">删除</a-button>
        </div>
        <div class="ca__pools">
          <div class="ca__pool">
            <div class="ca__pool-title">
              {{ a.intl_label }}
              <span v-if="a.intl_over" class="ca__chip ca__chip--bad">已达阈值</span>
            </div>
            <LbQuotaBar
              :used-bytes="a.state.sampled_at ? a.state.intl_bytes : null"
              :quota-bytes="a.cdt_quota_intl_bytes"
              :warning-level="cloudUsageLevel({ usage_percent: a.intl_percent, over: a.intl_over, quota_bytes: a.cdt_quota_intl_bytes })"
            />
          </div>
          <div class="ca__pool">
            <div class="ca__pool-title">
              {{ a.cn_label }}
              <span v-if="a.cn_over" class="ca__chip ca__chip--bad">已达阈值</span>
            </div>
            <LbQuotaBar
              :used-bytes="a.state.sampled_at ? a.state.cn_bytes : null"
              :quota-bytes="a.cdt_quota_cn_bytes"
              :warning-level="cloudUsageLevel({ usage_percent: a.cn_percent, over: a.cn_over, quota_bytes: a.cdt_quota_cn_bytes })"
            />
          </div>
        </div>
        <div class="ca__meta">
          阈值 {{ a.threshold_percent }}% · 绑定 {{ a.bound_nodes }} 台节点 ·
          <template v-if="a.state.sampled_at">
            采样 <LbTimeText :value="a.state.sampled_at" />
          </template>
          <template v-else>还没采样过</template>
          <template v-if="a.state.last_error"> · 上次错误:{{ a.state.last_error }}</template>
        </div>
      </div>
      <div class="ca__foot">
        <a-button type="primary" @click="openCreate">添加云账号</a-button>
        <span v-if="lastRun" class="ca__meta">上一轮轮询 <LbTimeText :value="lastRun" /></span>
      </div>
    </template>

    <div class="ca__settings">
      <div class="ca__settings-title">轮询与定时</div>
      <a-row :gutter="12">
        <a-col :xs="24" :sm="12">
          <div class="ca__label">定时开关机的时区(IANA 名)</div>
          <a-input v-model:value="tz" :placeholder="settings?.default_cloud_timezone || 'Asia/Shanghai'" />
          <div class="ca__help">
            节点上「定时开机 / 停机」填的 HH:MM 按这个时区解释。这是面板里唯一一处不按 UTC 的时间。
            留空用 {{ settings?.default_cloud_timezone || 'Asia/Shanghai' }}。
          </div>
        </a-col>
        <a-col :xs="24" :sm="12">
          <div class="ca__label">轮询间隔(秒)</div>
          <a-input-number
            v-model:value="pollSec"
            :min="60"
            :max="86400"
            :placeholder="String(settings?.default_cloud_poll_interval_sec || 300)"
            style="width: 100%"
          />
          <div class="ca__help">
            每隔这么久查一次 CDT 用量与每台实例的状态。CDT 的数据本身有延迟,拉得更勤只会反复读到同一份数字。
          </div>
        </a-col>
      </a-row>
      <div class="ca__actions">
        <a-button type="primary" :disabled="!settingsDirty" :loading="savingSettings" @click="saveSettings">
          保存轮询与时区
        </a-button>
      </div>
    </div>

    <a-modal
      :open="open"
      :title="editing ? `编辑云账号 · ${editing.name}` : '添加云账号'"
      :width="520"
      :confirm-loading="saving"
      :ok-text="editing ? '保存' : '添加'"
      cancel-text="取消"
      :mask-closable="false"
      @cancel="open = false"
      @ok="submit"
    >
      <a-alert v-if="formError" type="error" show-icon class="ca__err" :message="formError" />
      <a-form layout="vertical">
        <a-form-item label="名称" required>
          <a-input v-model:value="form.name" placeholder="例如 阿里云国际站主账号" :maxlength="64" />
        </a-form-item>
        <a-form-item label="AccessKey ID" required>
          <a-input v-model:value="form.access_key_id" class="lb-mono" placeholder="LTAI…" />
        </a-form-item>
        <a-form-item label="AccessKey Secret" :required="!editing">
          <a-input-password
            v-model:value="form.access_key_secret"
            autocomplete="new-password"
            :placeholder="editing ? '留空表示不改' : ''"
          />
          <div class="ca__help">
            主密钥加密存储,永不回显。建议用一个只授予 CDT 只读、ECS 查询与启停权限的 RAM 子账号。
          </div>
        </a-form-item>
        <a-row :gutter="12">
          <a-col :span="8">
            <a-form-item label="国际区域额度(GiB)">
              <a-input-number v-model:value="form.quota_intl_gib" :min="0" :precision="2" style="width: 100%" />
            </a-form-item>
          </a-col>
          <a-col :span="8">
            <a-form-item label="内地区域额度(GiB)">
              <a-input-number v-model:value="form.quota_cn_gib" :min="0" :precision="2" style="width: 100%" />
            </a-form-item>
          </a-col>
          <a-col :span="8">
            <a-form-item label="阈值(%)">
              <a-input-number v-model:value="form.threshold_percent" :min="1" :max="100" style="width: 100%" />
            </a-form-item>
          </a-col>
        </a-row>
        <div class="ca__help">
          额度填 0 表示不限(那一类不触发阈值)。中国香港按阿里云的定价归<strong>国际</strong>区域。
        </div>
        <a-form-item label="启用">
          <a-switch v-model:checked="form.enabled" />
          <span class="ca__help ca__help--inline">停用后面板不再轮询它,绑定的实例也不再受监控。</span>
        </a-form-item>
        <div class="ca__test">
          <a-button :loading="testing" @click="test">测试:当场查一次 CDT 用量</a-button>
          <div v-if="testResult" class="ca__test-result">
            国际 <b class="lb-mono">{{ formatBytes(testResult.intl_bytes) }}</b> · 内地
            <b class="lb-mono">{{ formatBytes(testResult.cn_bytes) }}</b>
            <div v-for="r in testResult.regions" :key="r.business_region_id" class="lb-mono ca__test-row">
              {{ r.business_region_id }} = {{ formatBytes(r.bytes) }}
            </div>
            <div v-if="testResult.regions.length === 0" class="ca__help">这个账号本月还没有 CDT 流量记录。</div>
          </div>
        </div>
      </a-form>
    </a-modal>
  </div>
</template>

<style scoped>
.ca__lead {
  font-size: 12px;
  color: #6b7480;
  line-height: 1.7;
  margin-bottom: 12px;
}
.ca__item {
  border: 1px solid #e3e6ea;
  border-radius: 8px;
  padding: 10px 12px;
  margin-bottom: 10px;
}
.ca__head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 8px;
}
.ca__name {
  font-weight: 500;
}
.ca__ak {
  font-size: 12px;
  color: #6b7480;
}
.ca__spacer {
  flex: 1;
}
.ca__chip {
  font-size: 11px;
  padding: 0 6px;
  border-radius: 10px;
  border: 1px solid #efdcb4;
  background: #fcf3e3;
}
.ca__chip--bad {
  border-color: #f3cfc9;
  background: #fdecea;
}
.ca__chip--off {
  border-color: #dfe3e8;
  background: #f1f3f5;
}
.ca__pools {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 12px;
}
@media (max-width: 767px) {
  .ca__pools {
    grid-template-columns: 1fr;
  }
}
.ca__pool-title {
  font-size: 12px;
  margin-bottom: 4px;
  display: flex;
  gap: 6px;
  align-items: center;
}
.ca__meta {
  font-size: 12px;
  color: #6b7480;
  margin-top: 6px;
}
.ca__foot {
  display: flex;
  align-items: center;
  gap: 12px;
  margin: 4px 0 12px;
}
.ca__settings {
  border-top: 1px dashed #e3e6ea;
  padding-top: 12px;
}
.ca__settings-title {
  font-weight: 500;
  margin-bottom: 8px;
}
.ca__label {
  font-size: 12px;
  margin-bottom: 4px;
}
.ca__help {
  font-size: 12px;
  color: #6b7480;
  line-height: 1.6;
  margin-top: 4px;
}
.ca__help--inline {
  margin-left: 8px;
}
.ca__actions {
  display: flex;
  justify-content: flex-end;
  margin-top: 8px;
}
.ca__err {
  margin-bottom: 12px;
}
.ca__test {
  margin-top: 4px;
}
.ca__test-result {
  margin-top: 8px;
  font-size: 12px;
}
.ca__test-row {
  color: #6b7480;
}
</style>
