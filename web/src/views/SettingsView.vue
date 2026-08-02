<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import { api, ApiError } from '@/api/client'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const router = useRouter()

const form = reactive({ oldPassword: '', newPassword: '', confirmPassword: '' })
const loading = ref(false)

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
  max-width: 480px;
}

.note {
  margin-top: 16px;
}
</style>
