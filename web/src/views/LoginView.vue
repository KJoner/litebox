<script setup lang="ts">
import { reactive, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
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

      <!--
        这里是管理员登录页,查的是管理员账号表。普通用户在这里输账号
        只会得到「用户名或密码错误」,看起来完全就是密码发错了 ——
        而管理员把面板首页发给用户是很自然的事。给一个明确的出口。
      -->
      <div class="portal-entry">
        普通用户请前往
        <RouterLink to="/user/login">用户中心</RouterLink>
      </div>
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

.portal-entry {
  margin-top: 16px;
  text-align: center;
  font-size: 12px;
  color: rgb(0 0 0 / 45%);
}
</style>
