<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import { ApiError } from '@/api/client'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const router = useRouter()
const route = useRoute()

const form = reactive({ username: '', password: '' })
const loading = ref(false)

async function onSubmit() {
  loading.value = true
  try {
    await auth.login(form.username, form.password)
    const redirect = typeof route.query.redirect === 'string' ? route.query.redirect : '/dashboard'
    await router.replace(redirect)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '登录失败,请稍后重试')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="login-page">
    <a-card class="login-card" :bordered="false">
      <h1 class="login-title">LiteBox</h1>
      <p class="login-subtitle">sing-box 轻量管理面板</p>

      <a-form layout="vertical" @submit.prevent="onSubmit">
        <a-form-item label="用户名">
          <a-input
            v-model:value="form.username"
            placeholder="请输入用户名"
            autocomplete="username"
            size="large"
          />
        </a-form-item>
        <a-form-item label="密码">
          <a-input-password
            v-model:value="form.password"
            placeholder="请输入密码"
            autocomplete="current-password"
            size="large"
            @press-enter="onSubmit"
          />
        </a-form-item>
        <a-button
          type="primary"
          size="large"
          block
          :loading="loading"
          :disabled="!form.username || !form.password"
          @click="onSubmit"
        >
          登录
        </a-button>
      </a-form>
    </a-card>
  </div>
</template>

<style scoped>
.login-page {
  height: 100%;
  display: flex;
  align-items: center;
  justify-content: center;
  background: #f0f2f5;
}

.login-card {
  width: 380px;
  max-width: calc(100vw - 32px);
  box-shadow: 0 2px 12px rgb(0 0 0 / 8%);
}

.login-title {
  margin: 0;
  font-size: 28px;
  text-align: center;
}

.login-subtitle {
  margin: 4px 0 24px;
  text-align: center;
  color: rgb(0 0 0 / 45%);
}
</style>
