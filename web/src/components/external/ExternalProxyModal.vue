<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  LOCKABLE_FIELD_LABEL,
  type AccessTier,
  type ExternalProtocol,
  type ExternalProxy,
} from '@/api/client'

/**
 * 新增 / 编辑一条外部代理。
 *
 * 新增有两条路:**粘贴分享链接**(主入口 —— 管理员实际拿到的东西
 * 就是一条链接)与手工填表。编辑时地址与凭据是只读的:
 * 从订阅源导入的条目上那是上游的事实,改了等于故意保留一个连不上的地址。
 */
const props = defineProps<{
  open: boolean
  /** null = 新建 */
  proxy: ExternalProxy | null
  tiers: AccessTier[]
}>()

const emit = defineEmits<{
  (e: 'update:open', v: boolean): void
  (e: 'saved'): void
}>()

const isEdit = computed(() => props.proxy !== null)
const submitting = ref(false)
const serverError = ref('')
const parsing = ref(false)

/** 新建时的两种填法。粘链接是默认 —— 手填九个字段太苦。 */
const mode = ref<'uri' | 'manual'>('uri')

/**
 * 这条线路的协议。
 *
 * 新建时由「解析」得出,而不是让管理员自己选:他手上只有一条链接,
 * 而链接的 scheme 已经把答案写在最前面了 —— 多一个下拉只是多一个填错的机会,
 * 填错的表现还很难查(按 VMess 存下一条 Trojan,订阅照常下发,用户连不上)。
 */
const protocol = ref<ExternalProtocol>('SHADOWSOCKS')
/** 解析结果里的几句人话,给管理员一眼确认「解出来的和我预期的一样」。 */
const parsedNote = ref('')

/** Shadowsocks 之外的协议不按字段拼链接,那几个输入框整个不显示。 */
const isSS = computed(() => protocol.value === 'SHADOWSOCKS')

const blank = {
  uri: '',
  name: '',
  display_name: '',
  server: '',
  port: null as number | null,
  method: '2022-blake3-aes-128-gcm',
  password: '',
  plugin: '',
  plugin_opts: '',
  access_tier_id: 1,
  subscription_enabled: true,
  sort_order: 0,
  public_remark: '',
  maintenance_message: '',
  expires_at: '' as string,
}
const form = reactive({ ...blank })

/** 外部代理必须支持传统 AEAD —— 那是别人配好的线路,我们只负责转发。 */
const methods = [
  '2022-blake3-aes-128-gcm',
  '2022-blake3-aes-256-gcm',
  '2022-blake3-chacha20-poly1305',
  'aes-128-gcm',
  'aes-192-gcm',
  'aes-256-gcm',
  'chacha20-ietf-poly1305',
  'xchacha20-ietf-poly1305',
]

watch(
  () => props.open,
  (open) => {
    if (!open) return
    serverError.value = ''
    mode.value = 'uri'
    parsedNote.value = ''
    const p = props.proxy
    if (!p) {
      protocol.value = 'SHADOWSOCKS'
      Object.assign(form, blank, { access_tier_id: props.tiers[0]?.id ?? 1 })
      return
    }
    protocol.value = p.protocol
    Object.assign(form, blank, {
      name: p.name,
      // 编辑时填的是「覆盖值」:留空表示跟随上游/原名。
      display_name: p.display_name_override,
      server: p.server,
      port: p.port,
      access_tier_id: p.access_tier_id,
      subscription_enabled: p.subscription_enabled,
      sort_order: p.sort_order,
      public_remark: p.public_remark,
      maintenance_message: p.maintenance_message,
      expires_at: p.expires_at ?? '',
    })
  },
)

/** 导入来的条目地址与凭据只读 —— 要改先「转为手工条目」。 */
const endpointReadonly = computed(() => props.proxy?.origin === 'IMPORTED')

const lockedLabels = computed(() =>
  (props.proxy?.locked_list ?? []).map((f) => LOCKABLE_FIELD_LABEL[f] ?? f),
)

/** 粘贴链接后自动填表。密码不回填 —— 它在剪贴板里,用户自己有。 */
async function parseURI() {
  const uri = form.uri.trim()
  if (!uri) return
  parsing.value = true
  serverError.value = ''
  try {
    const r = await api.parseProxyURI(uri)
    protocol.value = r.protocol
    form.server = r.server
    form.port = r.port
    form.method = r.method
    form.plugin = r.plugin
    form.plugin_opts = r.plugin_opts
    if (!form.display_name) form.display_name = r.display_name
    if (!form.name) form.name = r.display_name
    const bits = [r.protocol_label]
    if (r.transport) bits.push(r.transport)
    if (r.tls) bits.push('TLS')
    // 「能不能当出口」在这里就说清楚。等他配到入口的出口那一步才被拒的话,
    // 他早忘了这条线路是什么协议,而报错出现在另一个页面上。
    if (!r.dialable_by_node) {
      bits.push('走 QUIC,节点拨不了它 —— 只能直连给用户用,不能当入口的出口,也不能被 nginx 透传')
    }
    parsedNote.value = bits.join(' · ')
    message.success(`已解析:${r.protocol_label} ${r.server}:${r.port}`)
  } catch (err) {
    serverError.value = err instanceof ApiError ? err.message : '链接解析失败'
  } finally {
    parsing.value = false
  }
}

function close() {
  emit('update:open', false)
}

async function submit() {
  if (!form.name.trim()) {
    serverError.value = '请填写内部名称'
    return
  }
  submitting.value = true
  serverError.value = ''
  try {
    if (!isEdit.value) {
      await api.createExternalProxy({
        // 粘链接时把原文一起发过去:后端会原样保留它,
        // 订阅按 URI 格式下发时优先透传 —— 不认识的参数才不会被丢掉。
        uri: mode.value === 'uri' ? form.uri.trim() : '',
        name: form.name,
        display_name: form.display_name,
        protocol: protocol.value,
        server: form.server,
        port: form.port ?? 0,
        method: form.method,
        password: form.password,
        plugin: form.plugin,
        plugin_opts: form.plugin_opts,
        access_tier_id: form.access_tier_id,
        subscription_enabled: form.subscription_enabled,
        sort_order: form.sort_order,
        public_remark: form.public_remark,
        maintenance_message: form.maintenance_message,
        expires_at: form.expires_at || null,
      })
      message.success('已添加')
    } else {
      const id = props.proxy!.id
      // 手工条目才允许改地址与凭据。Shadowsocks 按字段改(填了新密码才发),
      // 别的协议只能整条链接换 —— 与新建同一条规矩。
      if (!endpointReadonly.value && !isSS.value && form.uri.trim()) {
        await api.replaceExternalProxyEndpoint(id, { uri: form.uri.trim() })
      } else if (!endpointReadonly.value && isSS.value && form.password) {
        await api.replaceExternalProxyEndpoint(id, {
          server: form.server,
          port: form.port ?? 0,
          method: form.method,
          password: form.password,
          plugin: form.plugin,
          plugin_opts: form.plugin_opts,
        })
      }
      const { effect } = await api.updateExternalProxy(id, {
        name: form.name,
        display_name: form.display_name,
        access_tier_id: form.access_tier_id,
        subscription_enabled: form.subscription_enabled,
        sort_order: form.sort_order,
        public_remark: form.public_remark,
        maintenance_message: form.maintenance_message,
        expires_at: form.expires_at || null,
      })
      if (effect.changes.length) {
        message.success(`已保存:${effect.changes.join(';')}`)
      } else {
        message.success('没有任何改动')
      }
    }
    close()
    emit('saved')
  } catch (err) {
    serverError.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <a-modal
    :open="props.open"
    :title="isEdit ? `编辑外部代理 · ${props.proxy?.final_display_name}` : '添加外部代理'"
    :width="560"
    :confirm-loading="submitting"
    :ok-text="isEdit ? '保存' : '添加'"
    cancel-text="取消"
    :mask-closable="false"
    @cancel="close"
    @ok="submit"
  >
    <a-alert
      v-if="serverError"
      type="error"
      show-icon
      class="ep__err"
      :message="serverError"
      description="没有被保存,表单内容已保留。"
    />

    <a-form layout="vertical">
      <template v-if="!isEdit">
        <a-form-item label="添加方式">
          <a-radio-group v-model:value="mode" button-style="solid">
            <a-radio-button value="uri">粘贴分享链接</a-radio-button>
            <a-radio-button value="manual">手工填写</a-radio-button>
          </a-radio-group>
          <div class="ep__help">
            粘链接是推荐做法:面板会<strong>原样保留原始链接</strong>,订阅按分享链接
            下发时直接透传它 —— 本面板不认识的参数(混淆插件、私有扩展)才不会被丢掉。
            丢掉之后用户能连上、网页能开,只有 UDP 不通,而这种问题很难查。
            <br />
            <strong>手工填写只支持 Shadowsocks。</strong>别的协议一律粘链接:
            VMess 的 base64(JSON)、VLESS/Trojan 的查询串各家写法都不一样,
            面板不去拼这类链接 —— 拼出来的那条丢掉的正是我们没解析的那些参数。
          </div>
        </a-form-item>

        <a-form-item v-if="mode === 'uri'" label="分享链接" required>
          <a-input-group compact>
            <a-input
              v-model:value="form.uri"
              style="width: calc(100% - 88px)"
              placeholder="ss://..."
            />
            <a-button :loading="parsing" style="width: 88px" @click="parseURI">解析</a-button>
          </a-input-group>
          <div class="ep__help">
            支持 <code>ss://</code>(两种方言)、<code>vmess://</code>、<code>vless://</code>、
            <code>trojan://</code>、<code>hysteria2://</code>(<code>hy2://</code>)、
            <code>tuic://</code>。
          </div>
          <a-alert
            v-if="parsedNote"
            type="info"
            show-icon
            class="ep__gap"
            :message="parsedNote"
            description="解析出来的只用于 sing-box 格式的订阅与「当作某个入口的出口」。分享链接与 base64 两种格式下发的是你粘进来的那条原文,一个字节都不改。"
          />
        </a-form-item>
      </template>

      <a-row :gutter="12">
        <a-col :span="12">
          <a-form-item label="内部名称" required>
            <a-input v-model:value="form.name" placeholder="只在管理后台出现" />
          </a-form-item>
        </a-col>
        <a-col :span="12">
          <a-form-item label="展示名称">
            <a-input
              v-model:value="form.display_name"
              :placeholder="props.proxy ? props.proxy.name_prefix + props.proxy.display_name : '留空同内部名称'"
            />
          </a-form-item>
        </a-col>
      </a-row>
      <div v-if="isEdit" class="ep__help ep__help--row">
        展示名称留空表示<strong>跟随上游</strong>(带上源前缀);填了就完全用你写的这个,
        前缀不再拼上去,同步也不会把它盖回去。
      </div>

      <a-alert
        v-if="lockedLabels.length"
        type="info"
        show-icon
        class="ep__gap"
        :message="`已锁定 ${lockedLabels.length} 项:${lockedLabels.join('、')}`"
        description="这些字段是你改过的,下次同步不会被上游覆盖。要让它重新跟随上游,把对应输入框清空即可。"
      />

      <a-row v-if="isSS" :gutter="12">
        <a-col :span="14">
          <a-form-item label="服务器地址">
            <a-input v-model:value="form.server" :disabled="endpointReadonly" />
          </a-form-item>
        </a-col>
        <a-col :span="10">
          <a-form-item label="端口">
            <a-input-number
              v-model:value="form.port"
              :min="1"
              :max="65535"
              :disabled="endpointReadonly"
              style="width: 100%"
            />
          </a-form-item>
        </a-col>
      </a-row>

      <a-form-item v-if="isSS" label="加密方法">
        <a-select v-model:value="form.method" :disabled="endpointReadonly">
          <a-select-option v-for="m in methods" :key="m" :value="m">{{ m }}</a-select-option>
        </a-select>
        <div class="ep__help">
          传统 AEAD(<code>chacha20-ietf-poly1305</code> 那几种)也收:那是别人配好的
          线路,我们只负责登记与转发,拦住它不会让任何人更安全。
          <strong>自建节点仍然只跑 2022 系列</strong> —— 那是两件事。
        </div>
      </a-form-item>

      <a-form-item v-if="isSS" :label="isEdit ? '密码(留空表示不改)' : '密码'">
        <a-input-password v-model:value="form.password" :disabled="endpointReadonly" />
      </a-form-item>

      <a-descriptions v-if="!isSS" :column="2" size="small" bordered class="ep__gap">
        <a-descriptions-item label="协议">{{ protocol }}</a-descriptions-item>
        <a-descriptions-item label="地址">{{ form.server }}:{{ form.port }}</a-descriptions-item>
      </a-descriptions>
      <div v-if="!isSS" class="ep__help ep__help--row">
        这类线路的地址与凭据只跟着<strong>分享链接</strong>走,面板不按字段拼它们。
        换了地址或密码,把新链接整条粘进来。
      </div>

      <a-form-item v-if="isEdit && !isSS && !endpointReadonly" label="更换分享链接">
        <a-input v-model:value="form.uri" placeholder="留空表示不改" />
        <div class="ep__help">
          填了就整条换掉:地址、端口、凭据与原文一起更新。
        </div>
      </a-form-item>

      <a-alert
        v-if="endpointReadonly"
        type="warning"
        show-icon
        class="ep__gap"
        message="这条来自订阅源,地址与凭据只读"
        description="它们是上游的事实,锁住等于故意保留一个连不上的地址。确实要手工改的话,先在列表里「转为手工条目」—— 那之后这一条不再被同步碰,也不会再跟着机场更新。"
      />

      <a-row :gutter="12">
        <a-col :span="12">
          <a-form-item label="访问等级">
            <a-select v-model:value="form.access_tier_id">
              <a-select-option v-for="t in props.tiers" :key="t.id" :value="t.id">
                {{ t.name }}
              </a-select-option>
            </a-select>
          </a-form-item>
        </a-col>
        <a-col :span="12">
          <a-form-item label="排序">
            <a-input-number v-model:value="form.sort_order" style="width: 100%" />
          </a-form-item>
        </a-col>
      </a-row>

      <a-form-item label="到期时间">
        <a-input v-model:value="form.expires_at" placeholder="留空表示不过期,例如 2026-12-31T00:00:00Z" />
        <div class="ep__help">
          到期后自动退出订阅,<strong>数据保留</strong>。填 RFC3339 的 UTC 时间。
        </div>
      </a-form-item>

      <a-form-item label="下发到用户订阅">
        <a-switch v-model:checked="form.subscription_enabled" />
      </a-form-item>

      <a-form-item label="公开备注">
        <a-input v-model:value="form.public_remark" placeholder="用户可见" />
      </a-form-item>
      <a-form-item label="维护说明">
        <a-input v-model:value="form.maintenance_message" placeholder="用户可见" />
      </a-form-item>
    </a-form>
  </a-modal>
</template>

<style scoped>
.ep__err {
  margin-bottom: 16px;
}
.ep__gap {
  margin-bottom: 20px;
}
.ep__help {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.7;
  color: #6b7480;
}
.ep__help--row {
  margin-top: -12px;
  margin-bottom: 16px;
}
</style>
