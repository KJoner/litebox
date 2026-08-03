<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import { api, ApiError, type PanelSettings } from '@/api/client'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const router = useRouter()

const form = reactive({ oldPassword: '', newPassword: '', confirmPassword: '' })
const loading = ref(false)

// ---------- 面板设置 ----------

const settings = ref<PanelSettings | null>(null)
const baseURL = ref('')
const savingBaseURL = ref(false)

async function loadSettings() {
  try {
    const s = await api.settings()
    settings.value = s
    baseURL.value = s.subscription_base_url
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载设置失败')
  }
}

async function saveBaseURL() {
  savingBaseURL.value = true
  try {
    const s = await api.updateSettings({ subscription_base_url: baseURL.value })
    settings.value = { ...(settings.value as PanelSettings), ...s }
    baseURL.value = s.subscription_base_url
    message.success('已保存。已发出去的订阅地址不会失效,但客户端要重新拉一次订阅才会用上新域名')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    savingBaseURL.value = false
  }
}

async function copyPanelKey() {
  const key = settings.value?.panel_public_key
  if (!key) return
  try {
    await navigator.clipboard.writeText(key)
    message.success('公钥已复制')
  } catch {
    // 非 HTTPS 下 clipboard API 不可用,这时让用户自己选中复制。
    message.warning('浏览器不允许自动复制,请手动选中上面的内容')
  }
}

onMounted(loadSettings)

async function onChangePassword() {
  if (form.newPassword.length < 8) {
    message.warning('新密码长度至少 8 位')
    return
  }
  if (form.newPassword !== form.confirmPassword) {
    message.warning('两次输入的新密码不一致')
    return
  }

  loading.value = true
  try {
    const result = await api.changePassword(form.oldPassword, form.newPassword)
    message.success(result.message)
    form.oldPassword = ''
    form.newPassword = ''
    form.confirmPassword = ''
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
    loading.value = false
  }
}
</script>

<template>
  <a-card title="订阅地址" class="settings-card">
    <a-form layout="vertical">
      <a-form-item
        label="站点根地址"
        extra="用户订阅链接的前缀,必须带 http:// 或 https://。这里改完立即生效,不用重启面板。"
      >
        <a-input v-model:value="baseURL" placeholder="https://panel.example.com" />
      </a-form-item>
      <a-button type="primary" :loading="savingBaseURL" @click="saveBaseURL">保存</a-button>
    </a-form>

    <p v-if="settings" class="note-text">
      订阅地址形如 <code>{{ baseURL || settings.config_base_url }}/sub/&lt;token&gt;</code>。
      配置文件里的默认值是 <code>{{ settings.config_base_url }}</code>,这里留空过就用它。
    </p>
    <a-alert
      class="note"
      type="info"
      show-icon
      message="改域名不会让已发出的订阅失效"
      description="订阅 Token 不变,用户不必重新导入;但客户端在下次拉订阅之前用的仍是旧地址,所以旧域名要留一段时间。"
    />
  </a-card>

  <a-card title="面板 SSH 公钥" class="settings-card">
    <p class="note-text">
      新增节点时装进节点 <code>authorized_keys</code> 的就是这一行。
      面板对节点的所有操作都用它,轮换或吊销时不必动你自己的日常密钥。
    </p>
    <a-typography-paragraph v-if="settings?.panel_public_key" class="key-box" copyable>
      {{ settings.panel_public_key }}
    </a-typography-paragraph>
    <a-empty v-else :image="undefined" description="尚未生成(首次连接节点时自动生成)" />
    <a-button v-if="settings?.panel_public_key" size="small" @click="copyPanelKey">复制</a-button>
    <a-alert
      class="note"
      type="warning"
      show-icon
      message="只允许密钥登录的节点"
      description="这类机器没法用密码引导,先手工把上面这行追加到节点的 ~/.ssh/authorized_keys,再在面板里新增节点并选「主控本机私钥」或直接点详情里的「重新引导」。"
    />
  </a-card>

  <a-card title="修改密码" class="settings-card">
    <a-form layout="vertical">
      <a-form-item label="当前用户">
        <a-input :value="auth.admin?.username" disabled />
      </a-form-item>
      <a-form-item label="原密码">
        <a-input-password v-model:value="form.oldPassword" autocomplete="current-password" />
      </a-form-item>
      <a-form-item label="新密码" extra="长度至少 8 位">
        <a-input-password v-model:value="form.newPassword" autocomplete="new-password" />
      </a-form-item>
      <a-form-item label="确认新密码">
        <a-input-password v-model:value="form.confirmPassword" autocomplete="new-password" />
      </a-form-item>
      <a-button type="primary" :loading="loading" @click="onChangePassword">保存</a-button>
    </a-form>

    <a-alert
      class="note"
      type="warning"
      show-icon
      message="修改密码后,其他设备上的登录会话将立即失效,当前设备保持登录。"
    />
  </a-card>
</template>

<style scoped>
.settings-card {
  max-width: 560px;
  margin-bottom: 16px;
}

.note-text {
  color: rgb(0 0 0 / 65%);
  font-size: 13px;
  line-height: 1.8;
}

.key-box {
  padding: 8px 12px;
  background: rgb(0 0 0 / 3%);
  border-radius: 4px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px;
  word-break: break-all;
}

.note {
  margin-top: 16px;
}
</style>
