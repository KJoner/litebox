<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AccessTier,
  type ProxyPreviewResult,
  type ProxySource,
} from '@/api/client'
import { formatBytes, formatUTCTime } from '@/utils/format'

/**
 * 新增 / 编辑订阅源。
 *
 * 新增走三步向导:填地址 → 预览(不落库,逐条可勾选)→ 确认导入。
 * 建源与首次导入放在一个动作里 —— 分两步的话,导入失败会留下一个空的源,
 * 而管理员多半会再建一个,于是同一个机场出现两条记录。
 */
const props = defineProps<{
  open: boolean
  /** null = 新建 */
  source: ProxySource | null
  tiers: AccessTier[]
}>()

const emit = defineEmits<{
  (e: 'update:open', v: boolean): void
  (e: 'saved'): void
}>()

const isEdit = computed(() => props.source !== null)
const step = ref(0)
const busy = ref(false)
const serverError = ref('')

const blank = {
  name: '',
  url: '',
  name_prefix: '',
  default_access_tier_id: 1,
  default_subscription_enabled: true,
  auto_sync_enabled: false,
  sync_interval_minutes: 720,
  expires_at: '',
  enabled: true,
  remark: '',
  sort_order: 0,
}
const form = reactive({ ...blank })

const preview = ref<ProxyPreviewResult | null>(null)
/** 勾选集合,键是 identity_key —— 两次请求之间上游列表可能变,按下标会选错。 */
const selected = ref<Set<string>>(new Set())

watch(
  () => props.open,
  (open) => {
    if (!open) return
    step.value = 0
    serverError.value = ''
    preview.value = null
    selected.value = new Set()
    const s = props.source
    if (!s) {
      Object.assign(form, blank, { default_access_tier_id: props.tiers[0]?.id ?? 1 })
      return
    }
    Object.assign(form, blank, {
      name: s.name,
      // 订阅地址从不回显 —— 它含 token。留空表示保持原地址。
      url: '',
      name_prefix: s.name_prefix,
      default_access_tier_id: s.default_access_tier_id,
      default_subscription_enabled: s.default_subscription_enabled,
      auto_sync_enabled: s.auto_sync_enabled,
      sync_interval_minutes: s.sync_interval_minutes,
      expires_at: s.expires_at ?? '',
      enabled: s.enabled,
      remark: s.remark,
      sort_order: s.sort_order,
    })
  },
)

const suggestedCount = computed(
  () => preview.value?.items.filter((i) => i.suggested).length ?? 0,
)
const announcementCount = computed(
  () => preview.value?.items.filter((i) => i.announcement).length ?? 0,
)

async function doPreview() {
  if (!form.name.trim()) {
    serverError.value = '请填写源名称'
    return
  }
  if (!form.url.trim()) {
    serverError.value = '请填写订阅地址'
    return
  }
  busy.value = true
  serverError.value = ''
  try {
    const r = await api.previewProxySource(form.url.trim())
    preview.value = r
    // 疑似公告的条目默认不勾,但仍然全部列出 —— 识别规则一定会误伤。
    selected.value = new Set(r.items.filter((i) => i.suggested).map((i) => i.identity_key))
    step.value = 1
  } catch (err) {
    // 后端的错误信息里已经写清了「识别到什么格式、为什么不支持」——
    // 报「解析失败」会让管理员以为是地址填错了,两者要做的事完全不同。
    serverError.value = err instanceof ApiError ? err.message : '拉取订阅失败'
  } finally {
    busy.value = false
  }
}

function toggle(key: string, on: boolean) {
  const next = new Set(selected.value)
  if (on) next.add(key)
  else next.delete(key)
  selected.value = next
}

function selectAll(on: boolean) {
  selected.value = on
    ? new Set((preview.value?.items ?? []).map((i) => i.identity_key))
    : new Set()
}

function close() {
  emit('update:open', false)
}

function payload() {
  return {
    name: form.name,
    url: form.url.trim(),
    name_prefix: form.name_prefix,
    default_access_tier_id: form.default_access_tier_id,
    default_subscription_enabled: form.default_subscription_enabled,
    auto_sync_enabled: form.auto_sync_enabled,
    sync_interval_minutes: form.sync_interval_minutes,
    expires_at: form.expires_at || null,
    enabled: form.enabled,
    remark: form.remark,
    sort_order: form.sort_order,
  }
}

async function doImport() {
  busy.value = true
  serverError.value = ''
  try {
    const r = await api.importProxySource({
      ...payload(),
      selected_keys: [...selected.value],
    })
    close()
    emit('saved')
    if (r.error) {
      // 源建好了但导入失败 —— 必须分开说,否则管理员会再建一个源。
      message.warning(`代理源已创建,但首次导入失败:${r.error}。可在源卡片上重试同步。`)
    } else {
      const parts = [`新增 ${r.result.added}`]
      if (r.result.skipped) parts.push(`跳过 ${r.result.skipped}`)
      message.success(`导入完成:${parts.join(',')}`)
    }
  } catch (err) {
    serverError.value = err instanceof ApiError ? err.message : '导入失败'
  } finally {
    busy.value = false
  }
}

async function doSave() {
  if (!form.name.trim()) {
    serverError.value = '请填写源名称'
    return
  }
  busy.value = true
  serverError.value = ''
  try {
    await api.updateProxySource(props.source!.id, payload())
    close()
    emit('saved')
    message.success('已保存')
  } catch (err) {
    serverError.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <a-modal
    :open="props.open"
    :title="isEdit ? `编辑代理源 · ${props.source?.name}` : '添加代理源'"
    :width="step === 1 ? 760 : 560"
    :mask-closable="false"
    :footer="null"
    @cancel="close"
  >
    <a-alert
      v-if="serverError"
      type="error"
      show-icon
      class="ps__err"
      :message="serverError"
      description="没有任何改动被保存,表单内容已保留。"
    />

    <!-- 第一步:填地址 -->
    <a-form v-if="step === 0" layout="vertical">
      <a-form-item label="源名称" required>
        <a-input v-model:value="form.name" placeholder="只在管理后台出现,例如「甲机场」" />
      </a-form-item>

      <a-form-item :label="isEdit ? '订阅地址(留空表示不改)' : '订阅地址'" :required="!isEdit">
        <a-input-password v-model:value="form.url" placeholder="https://..." />
        <div class="ps__help">
          它含 token,等同密码:面板用主密钥加密存储,<strong>永不回显、不进审计日志</strong>。
        </div>
      </a-form-item>

      <a-form-item label="条目名前缀">
        <a-input v-model:value="form.name_prefix" placeholder="例如「[甲] 」,最多 16 字" />
        <div class="ps__help">
          拼在每条的展示名前面。<strong>不自动加分隔符</strong> —— 想要「[甲] 香港01」就在
          前缀末尾自己带一个空格,想紧贴就不带。前缀是渲染时拼的,改了立刻对全部条目生效。
        </div>
      </a-form-item>

      <a-row :gutter="12">
        <a-col :span="12">
          <a-form-item label="新条目的默认访问等级">
            <a-select v-model:value="form.default_access_tier_id">
              <a-select-option v-for="t in props.tiers" :key="t.id" :value="t.id">
                {{ t.name }}
              </a-select-option>
            </a-select>
          </a-form-item>
        </a-col>
        <a-col :span="12">
          <a-form-item label="到期时间">
            <a-input v-model:value="form.expires_at" placeholder="留空不过期" />
          </a-form-item>
        </a-col>
      </a-row>
      <div class="ps__help ps__help--row">
        源到期后,<strong>它下面全部条目一起退出订阅</strong> —— 机场账号到期后那些节点
        就是连不上的,留在订阅里只会让用户以为是自己的问题。数据全部保留。
      </div>

      <a-form-item label="新条目默认下发到订阅">
        <a-switch v-model:checked="form.default_subscription_enabled" />
      </a-form-item>

      <a-form-item label="自动同步">
        <a-switch v-model:checked="form.auto_sync_enabled" />
        <a-input-number
          v-if="form.auto_sync_enabled"
          v-model:value="form.sync_interval_minutes"
          :min="30"
          :step="30"
          addon-after="分钟"
          style="width: 180px; margin-left: 12px"
        />
        <div class="ps__help">
          默认关。打开之前建议先手工同步一次看看结果 —— 同步可能让一批条目退出订阅。
        </div>
      </a-form-item>

      <a-form-item label="备注">
        <a-input v-model:value="form.remark" />
      </a-form-item>

      <div class="ps__foot">
        <a-button @click="close">取消</a-button>
        <a-button v-if="isEdit" type="primary" :loading="busy" @click="doSave">保存</a-button>
        <a-button v-else type="primary" :loading="busy" @click="doPreview">下一步:预览</a-button>
      </div>
    </a-form>

    <!-- 第二步:预览(不落库) -->
    <div v-else-if="step === 1 && preview">
      <div class="ps__summary">
        <div>
          识别为 <strong>{{ preview.format_label }}</strong>,共解析出
          <strong>{{ preview.items.length }}</strong> 条,已勾选
          <strong>{{ selected.size }}</strong> 条。
        </div>
        <div v-if="announcementCount" class="ps__note">
          其中 {{ announcementCount }} 条疑似<strong>公告而非节点</strong>(机场常把
          「剩余流量」「套餐到期」这类信息伪装成节点),已默认不勾选 ——
          但仍然全部列出,识别规则一定会误伤,请自己确认。
        </div>
        <div v-if="preview.skipped.length" class="ps__note">
          跳过
          {{ preview.skipped.reduce((n, g) => n + g.count, 0) }} 条:
          {{ preview.skipped.map((g) => `${g.label} ${g.count} 条`).join('、') }}
          —— 本版本只支持 Shadowsocks。
        </div>
        <div v-if="preview.parse_errors.length" class="ps__note ps__note--warn">
          {{ preview.parse_errors.length }} 条 ss:// 链接解析失败:
          <div v-for="(e, i) in preview.parse_errors.slice(0, 3)" :key="i" class="lb-mono ps__errline">
            {{ e }}
          </div>
        </div>
        <div v-if="preview.upstream" class="ps__note">
          上游信息:已用 {{ formatBytes(preview.upstream.used_bytes) }} /
          {{ preview.upstream.total_bytes ? formatBytes(preview.upstream.total_bytes) : '不限' }}
          <template v-if="preview.upstream.expires_at">
            · 到期 {{ formatUTCTime(preview.upstream.expires_at) }}
          </template>
          <br />
          <span class="ps__dim">
            这是整个机场账号的总量,<strong>不会进任何用户的流量统计</strong>。
          </span>
        </div>
      </div>

      <div class="ps__actions">
        <a-button size="small" @click="selectAll(true)">全选</a-button>
        <a-button size="small" @click="selectAll(false)">全不选</a-button>
        <a-button size="small" @click="selectAll(false); (preview.items.filter(i => i.suggested)).forEach(i => toggle(i.identity_key, true))">
          只选推荐的 {{ suggestedCount }} 条
        </a-button>
      </div>

      <div class="ps__list">
        <div v-for="item in preview.items" :key="item.identity_key" class="ps__row">
          <a-checkbox
            :checked="selected.has(item.identity_key)"
            @change="(e: any) => toggle(item.identity_key, e.target.checked)"
          />
          <div class="ps__row-main">
            <div class="ps__row-name">
              {{ form.name_prefix }}{{ item.name || '(无名)' }}
              <span v-if="item.announcement" class="ps__tag ps__tag--warn">疑似公告</span>
              <span v-if="item.existing" class="ps__tag">已存在</span>
            </div>
            <div class="ps__row-addr lb-mono">
              {{ item.method }} · {{ item.server }}:{{ item.port }}
            </div>
          </div>
        </div>
      </div>

      <div class="ps__note">
        <strong>没勾选的条目也会入库</strong>,但标记为「已排除」且不进订阅 ——
        不入库的话,下次同步它们会作为「新增」再进来一遍,你每次都得重新排除。
      </div>

      <div class="ps__foot">
        <a-button @click="step = 0">上一步</a-button>
        <a-button type="primary" :loading="busy" @click="doImport">
          创建代理源并导入 {{ selected.size }} 条
        </a-button>
      </div>
    </div>
  </a-modal>
</template>

<style scoped>
.ps__err {
  margin-bottom: 16px;
}
.ps__help {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.7;
  color: #6b7480;
}
.ps__help--row {
  margin-top: -12px;
  margin-bottom: 16px;
}
.ps__summary {
  padding: 12px 14px;
  margin-bottom: 12px;
  background: #f1f3f5;
  border-radius: 6px;
  font-size: 13px;
  line-height: 1.8;
}
.ps__note {
  margin-top: 6px;
  font-size: 12px;
  line-height: 1.75;
  color: #576070;
}
.ps__note--warn {
  color: #92610a;
}
.ps__errline {
  font-size: 11px;
  word-break: break-all;
}
.ps__dim {
  color: #6b7480;
}
.ps__actions {
  display: flex;
  gap: 8px;
  margin-bottom: 8px;
}
.ps__list {
  max-height: 320px;
  overflow-y: auto;
  border: 1px solid #e3e6ea;
  border-radius: 6px;
}
.ps__row {
  display: flex;
  gap: 10px;
  align-items: center;
  padding: 7px 11px;
  border-bottom: 1px solid #edeff2;
}
.ps__row:last-child {
  border-bottom: none;
}
.ps__row-main {
  min-width: 0;
}
.ps__row-name {
  font-size: 13px;
}
.ps__row-addr {
  font-size: 11px;
  color: #6b7480;
}
.ps__tag {
  display: inline-block;
  margin-left: 6px;
  padding: 0 5px;
  border-radius: 3px;
  background: #f1f3f5;
  color: #576070;
  font-size: 10.5px;
}
.ps__tag--warn {
  background: #fcf3e3;
  color: #92610a;
}
.ps__foot {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  margin-top: 16px;
}
</style>
