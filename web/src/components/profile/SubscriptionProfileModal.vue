<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  profileKindHint,
  profileKindLabel,
  type ProfileKind,
  type ProfilePlaceholder,
  type ProfilePlaceholderInfo,
  type ProfilePreview,
  type ProxyUser,
  type SubscriptionProfile,
} from '@/api/client'

/**
 * 配置文件模板编辑器。
 *
 * **编辑器是原生 textarea,不引 CodeMirror / Monaco。** 面板要 embed 进
 * 单个二进制,一个编辑器组件就是几百 KB,而这一页管理员一个月打开不了几次。
 * 编辑器真正提供的价值是「错误在第几行」,那由后端的语法自检给出。
 *
 * 上传走前端 FileReader 填进同一个 textarea,不加 multipart 接口 ——
 * 上传和粘贴本来就是同一件事,走两条路会有两套大小限制、两套编码处理。
 */
const props = defineProps<{
  open: boolean
  /** null 表示新建 */
  profile: SubscriptionProfile | null
  info: ProfilePlaceholderInfo | null
  users: ProxyUser[]
  baseUrl: string
}>()

const emit = defineEmits<{
  (e: 'update:open', v: boolean): void
  (e: 'saved'): void
}>()

const form = ref({
  kind: 'CLASH' as ProfileKind,
  name: '',
  display_name: '',
  filename: '',
  content: '',
  singbox_landing_detour: '',
  description: '',
  remark: '',
  enabled: true,
  sort_order: 0,
})

const saving = ref(false)
const editing = computed(() => props.profile !== null)
const editorRef = ref<HTMLTextAreaElement | null>(null)

// 预览
const preview = ref<ProfilePreview | null>(null)
const previewError = ref('')
const previewing = ref(false)
const previewUserID = ref<number | undefined>(undefined)
const previewOpen = ref(false)

watch(
  () => props.open,
  (open) => {
    if (!open) return
    preview.value = null
    previewError.value = ''
    previewOpen.value = false
    const p = props.profile
    form.value = p
      ? {
          kind: p.kind,
          name: p.name,
          display_name: p.display_name,
          filename: p.filename,
          content: p.content ?? '',
          singbox_landing_detour: p.singbox_landing_detour,
          description: p.description,
          remark: p.remark,
          enabled: p.enabled,
          sort_order: p.sort_order,
        }
      : {
          kind: 'CLASH',
          name: '',
          display_name: '',
          filename: props.info?.default_filenames.CLASH ?? 'config.yaml',
          content: '',
          singbox_landing_detour: '',
          description: '',
          remark: '',
          enabled: true,
          sort_order: 0,
        }
  },
)

// 换类型时把文件名跟着换掉 —— 但只在管理员没自己改过的时候。
watch(
  () => form.value.kind,
  (kind, old) => {
    if (!props.info || editing.value || !old) return
    const wasDefault = form.value.filename === props.info.default_filenames[old]
    if (wasDefault || !form.value.filename) {
      form.value.filename = props.info.default_filenames[kind]
    }
  },
)

/** 只列出当前类型能用的占位符 —— 列全了管理员会照着写一个用不了的。 */
const usable = computed<ProfilePlaceholder[]>(
  () => props.info?.items.filter((p) => p.kinds.includes(form.value.kind)) ?? [],
)

/** 落地关键词由后端给,前端不写死 —— 两处分叉的表现是页面上教的和实际判定的不一样。 */
const landingKeywords = computed(() => props.info?.landing_keywords ?? ['落地', 'landing'])
const maxBytes = computed(() => props.info?.max_bytes ?? 256 * 1024)
const contentBytes = computed(() => new Blob([form.value.content]).size)
const tooBig = computed(() => contentBytes.value > maxBytes.value)

const urlShape = computed(() => {
  const id = props.profile ? props.profile.id : '<id>'
  const name = form.value.filename || 'config'
  return `${props.baseUrl || 'https://box.example.com'}/sub/<token>/profile/${id}/${name}`
})

/** 点占位符插到光标处。手抄一遍就多一次拼错的机会,而拼错要到保存时才知道。 */
async function insert(name: string) {
  const el = editorRef.value
  const token = `$(${name})`
  if (!el) {
    form.value.content += token
    return
  }
  const start = el.selectionStart
  const end = el.selectionEnd
  form.value.content = form.value.content.slice(0, start) + token + form.value.content.slice(end)
  await nextTick()
  el.focus()
  el.selectionStart = el.selectionEnd = start + token.length
}

function pickFile(e: Event) {
  const file = (e.target as HTMLInputElement).files?.[0]
  if (!file) return
  if (file.size > maxBytes.value) {
    message.error(`文件 ${(file.size / 1024).toFixed(0)} KB,超过上限 ${maxBytes.value / 1024} KB`)
    return
  }
  const reader = new FileReader()
  reader.onload = () => {
    form.value.content = String(reader.result ?? '')
    if (!form.value.filename) form.value.filename = file.name
    message.success(`已读入 ${file.name}`)
  }
  reader.onerror = () => message.error('读取文件失败')
  reader.readAsText(file, 'utf-8')
  ;(e.target as HTMLInputElement).value = ''
}

async function runPreview() {
  previewing.value = true
  previewError.value = ''
  try {
    preview.value = await api.previewSubscriptionProfile({
      kind: form.value.kind,
      content: form.value.content,
      singbox_landing_detour: form.value.singbox_landing_detour,
      user_id: previewUserID.value ?? 0,
    })
    previewOpen.value = true
  } catch (err) {
    preview.value = null
    previewError.value = err instanceof ApiError ? err.message : '预览失败'
    previewOpen.value = true
  } finally {
    previewing.value = false
  }
}

async function save() {
  if (!form.value.name.trim()) {
    message.warning('请填写名称')
    return
  }
  saving.value = true
  try {
    const body = { ...form.value }
    if (props.profile) {
      await api.updateSubscriptionProfile(props.profile.id, body)
      message.success('已保存。用户的客户端要下次拉取才会拿到新内容')
    } else {
      await api.createSubscriptionProfile(body)
      message.success('已新增')
    }
    emit('saved')
    emit('update:open', false)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <a-drawer
    :open="open"
    :title="editing ? `编辑配置 · ${profile?.name}` : '新增配置文件'"
    width="920"
    :body-style="{ padding: '16px' }"
    @update:open="emit('update:open', $event)"
  >
    <div class="pm">
      <div class="pm__grid">
        <div class="pm__field">
          <label class="pm__label"><span class="pm__req">*</span> 客户端类型</label>
          <a-select v-model:value="form.kind" style="width: 100%">
            <a-select-option v-for="(label, k) in profileKindLabel" :key="k" :value="k">
              {{ label }}
            </a-select-option>
          </a-select>
          <div class="pm__help">{{ profileKindHint[form.kind] }}</div>
        </div>
        <div class="pm__field">
          <label class="pm__label"><span class="pm__req">*</span> 名称(内部)</label>
          <a-input v-model:value="form.name" placeholder="Clash 完整版" />
          <div class="pm__help">只在管理页出现,可以写「给谁用的」。</div>
        </div>
        <div class="pm__field">
          <label class="pm__label">展示名称</label>
          <a-input v-model:value="form.display_name" :placeholder="form.name || '留空即用内部名称'" />
          <div class="pm__help">门户上给用户看的标题。</div>
        </div>
        <div class="pm__field">
          <label class="pm__label"><span class="pm__req">*</span> 文件名</label>
          <a-input v-model:value="form.filename" placeholder="config.yaml" />
          <div class="pm__help">只允许英文字母、数字与 . - _;扩展名决定客户端怎么处理它。</div>
        </div>
        <div class="pm__field">
          <label class="pm__label">排序</label>
          <a-input-number v-model:value="form.sort_order" :min="0" style="width: 100%" />
        </div>
        <div class="pm__field">
          <label class="pm__label">状态</label>
          <div class="pm__switch">
            <a-switch v-model:checked="form.enabled" />
            <span>{{ form.enabled ? '启用 —— 全部用户的订阅页立即出现这一份' : '停用 —— 链接立即失效' }}</span>
          </div>
        </div>
      </div>

      <div class="pm__field">
        <label class="pm__label">给用户的一句话说明</label>
        <a-input v-model:value="form.description" placeholder="适合 iOS,含常见网站分流" />
      </div>

      <!-- 只有 sing-box 需要:落地节点要挂 detour 才能形成链路 -->
      <div v-if="form.kind === 'SINGBOX'" class="pm__field">
        <label class="pm__label">落地节点的前置出站</label>
        <a-input v-model:value="form.singbox_landing_detour" placeholder="前置节点" />
        <div class="pm__help">
          填了之后,名字里带<template v-for="(k, i) in landingKeywords" :key="k"
            ><span v-if="i">或</span><code>{{ k }}</code></template
          >的节点会自动挂上 <code>"detour"</code>,形成 客户端 → 前置 → 落地 的链路。
          <strong>这个名字必须是你模板里已有的分组 tag</strong> —— 指向一个不存在的 tag,
          sing-box 会直接启动失败。留空表示不挂。
        </div>
      </div>

      <!-- 占位符速查。点一下插到光标处 —— 手抄一遍就多一次拼错的机会。 -->
      <div class="pm__ph">
        <div class="pm__ph-head">
          <span>可用占位符</span>
          <span class="pm__ph-note">点击插入到光标处。写错的名字保存时会被拦下来。</span>
        </div>
        <div class="pm__ph-list">
          <button v-for="p in usable" :key="p.name" class="pm__chip" @click="insert(p.name)">
            <code>$({{ p.name }})</code>
            <span>{{ p.description }}</span>
          </button>
        </div>
      </div>

      <div class="pm__field">
        <div class="pm__editor-bar">
          <label class="pm__label"><span class="pm__req">*</span> 配置内容</label>
          <div class="pm__editor-tools">
            <span class="pm__size" :class="{ 'pm__size--bad': tooBig }">
              {{ (contentBytes / 1024).toFixed(1) }} / {{ (maxBytes / 1024).toFixed(0) }} KB
            </span>
            <label class="pm__upload">
              上传文件
              <input type="file" accept=".json,.yaml,.yml,.conf,.txt,.ini" @change="pickFile" />
            </label>
            <a-button size="small" :loading="previewing" @click="runPreview">预览渲染结果</a-button>
          </div>
        </div>
        <textarea
          ref="editorRef"
          v-model="form.content"
          class="pm__editor"
          spellcheck="false"
          placeholder="把你调好的整份配置粘贴进来,再把里面跟节点/订阅有关的地方换成上面的占位符"
        ></textarea>
      </div>

      <div class="pm__note">
        订阅地址形如
        <code class="lb-mono">{{ urlShape }}</code>
        <br />
        按 id 查找,所以改文件名不会让用户手里的链接失效。
      </div>

      <div class="pm__field">
        <label class="pm__label">内部备注</label>
        <a-input v-model:value="form.remark" placeholder="只在这一页显示,不会发给用户" />
      </div>

      <!-- 预览结果 -->
      <section v-if="previewOpen" class="pm__preview">
        <div class="pm__preview-head">
          <span>渲染结果</span>
          <div class="pm__preview-tools">
            <a-select
              v-model:value="previewUserID"
              size="small"
              style="width: 190px"
              placeholder="用示例节点渲染"
              allow-clear
            >
              <a-select-option v-for="u in users" :key="u.id" :value="u.id">
                {{ u.display_name }}({{ u.user_code }})
              </a-select-option>
            </a-select>
            <a-button size="small" :loading="previewing" @click="runPreview">重新渲染</a-button>
            <a-button size="small" type="text" @click="previewOpen = false">收起</a-button>
          </div>
        </div>

        <div v-if="previewError" class="pm__preview-error">{{ previewError }}</div>

        <template v-else-if="preview">
          <div class="pm__preview-facts">
            <span v-if="preview.sample_used">用的是<strong>示例节点</strong>,不是真实用户</span>
            <span v-else>按用户 <strong>{{ preview.user_code }}</strong> 渲染</span>
            <span>{{ preview.node_count }} 个节点 · 其中落地 {{ preview.landing_count }} 个</span>
          </div>
          <!--
            语法自检只报警告,不拦保存:我们的检查一定比 sing-box / mihomo 严格,
            拦下一份它们本来能接受的配置,比漏报一个语法错更糟。
          -->
          <div v-if="preview.warning" class="pm__preview-warn">
            <strong v-if="preview.warning.line">第 {{ preview.warning.line }} 行附近:</strong>
            {{ preview.warning.message }}
            <div class="pm__preview-warn-note">
              这只是提醒,不影响保存 —— 客户端的解析器比这里宽容,也可能更严。
            </div>
          </div>
          <pre class="pm__preview-body lb-mono">{{ preview.rendered }}</pre>
        </template>
      </section>
    </div>

    <template #footer>
      <div class="pm__footer">
        <a-button @click="emit('update:open', false)">取消</a-button>
        <a-button type="primary" :loading="saving" :disabled="tooBig" @click="save">
          {{ editing ? '保存' : '新增' }}
        </a-button>
      </div>
    </template>
  </a-drawer>
</template>

<style scoped>
.pm {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.pm__grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 12px;
}

.pm__field {
  display: flex;
  flex-direction: column;
  gap: 5px;
}

.pm__label {
  font-size: 12.5px;
  font-weight: 500;
  color: #15181c;
}

.pm__req {
  color: #b4291d;
}

.pm__help {
  font-size: 11.5px;
  line-height: 1.75;
  color: #6b7480;
}

.pm__help code,
.pm__note code {
  padding: 0 3px;
  background: #f1f3f5;
  border-radius: 3px;
}

.pm__switch {
  display: flex;
  align-items: center;
  gap: 8px;
  height: 32px;
  font-size: 11.5px;
  color: #6b7480;
}

.pm__ph {
  padding: 10px 12px;
  background: #f6f7f9;
  border: 1px solid #edeff2;
  border-radius: 6px;
}

.pm__ph-head {
  display: flex;
  align-items: baseline;
  gap: 10px;
  flex-wrap: wrap;
  margin-bottom: 8px;
  font-size: 12px;
  font-weight: 600;
}

.pm__ph-note {
  font-weight: 400;
  font-size: 11.5px;
  color: #6b7480;
}

.pm__ph-list {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

.pm__chip {
  display: flex;
  align-items: center;
  gap: 7px;
  padding: 5px 9px;
  background: #fff;
  border: 1px solid #dfe3e8;
  border-radius: 4px;
  font-size: 11.5px;
  color: #576070;
  cursor: pointer;
}

.pm__chip:hover {
  border-color: #c9dcf3;
  background: #eef4fc;
}

.pm__chip code {
  font-family: 'IBM Plex Mono', ui-monospace, SFMono-Regular, Menlo, monospace;
  color: #1d4f96;
}

.pm__editor-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.pm__editor-tools {
  display: flex;
  align-items: center;
  gap: 10px;
}

.pm__size {
  font-size: 11.5px;
  color: #6b7480;
}

.pm__size--bad {
  color: #b4291d;
  font-weight: 600;
}

.pm__upload {
  padding: 3px 9px;
  border: 1px solid #dfe3e8;
  border-radius: 4px;
  font-size: 12px;
  color: #576070;
  cursor: pointer;
}

.pm__upload:hover {
  border-color: #c9dcf3;
  color: #1d4f96;
}

.pm__upload input {
  display: none;
}

.pm__editor {
  width: 100%;
  min-height: 340px;
  padding: 10px 12px;
  border: 1px solid #e3e6ea;
  border-radius: 6px;
  font-family: 'IBM Plex Mono', ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px;
  line-height: 1.65;
  color: #15181c;
  resize: vertical;
  tab-size: 2;
}

.pm__editor:focus {
  outline: none;
  border-color: #2563b8;
}

.pm__note {
  padding: 10px 12px;
  background: #f6f7f9;
  border: 1px solid #edeff2;
  border-radius: 6px;
  font-size: 11.5px;
  line-height: 1.8;
  color: #576070;
  word-break: break-all;
}

.pm__preview {
  border: 1px solid #e3e6ea;
  border-radius: 6px;
  overflow: hidden;
}

.pm__preview-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  flex-wrap: wrap;
  padding: 9px 12px;
  background: #f6f7f9;
  border-bottom: 1px solid #edeff2;
  font-size: 12.5px;
  font-weight: 600;
}

.pm__preview-tools {
  display: flex;
  align-items: center;
  gap: 8px;
}

.pm__preview-facts {
  display: flex;
  gap: 16px;
  flex-wrap: wrap;
  padding: 9px 12px;
  font-size: 11.5px;
  color: #6b7480;
}

.pm__preview-error {
  padding: 11px 12px;
  background: #fdecea;
  border-bottom: 1px solid #f3cfc9;
  font-size: 12px;
  line-height: 1.8;
  color: #8e2117;
}

.pm__preview-warn {
  padding: 10px 12px;
  background: #fcf3e3;
  border-top: 1px solid #efdcb4;
  border-bottom: 1px solid #efdcb4;
  font-size: 11.5px;
  line-height: 1.8;
  color: #5c4405;
}

.pm__preview-warn-note {
  margin-top: 3px;
  opacity: 0.85;
}

.pm__preview-body {
  max-height: 380px;
  margin: 0;
  padding: 12px;
  overflow: auto;
  font-size: 11.5px;
  line-height: 1.6;
  white-space: pre;
}

.pm__footer {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}

@media (max-width: 767px) {
  .pm__grid {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
