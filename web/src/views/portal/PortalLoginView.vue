<script setup lang="ts">
import { reactive, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { ApiError } from '@/api/client'
import { usePortalStore } from '@/stores/portal'

/**
 * 用户登录 · 路径 / · 首页。
 *
 * 首页给用户而不是给管理员:用户有十个、管理员只有一个。
 * 管理员开通账号后手边只有面板地址,发首页是最自然的动作。
 * 已登录的管理员访问它会被守卫直接送回后台,不会被晾在这里。
 */
const portal = usePortalStore()
const router = useRouter()
const route = useRoute()

const form = reactive({ username: '', password: '' })
const loading = ref(false)
const error = ref('')
/** login_enabled=false 与整个账号被停用是两回事,文案必须不同。 */
const loginClosed = ref(false)

async function onSubmit() {
  if (loading.value) return
  loading.value = true
  error.value = ''
  loginClosed.value = false
  try {
    await portal.login(form.username, form.password)
    // 强制改密的用户直接送到安全设置页,不必先看一眼概览再被挡回来。
    if (portal.identity?.must_change_password) {
      await router.replace({ name: 'portal-security' })
      return
    }
    const redirect =
      typeof route.query.redirect === 'string' ? route.query.redirect : '/user/dashboard'
    await router.replace(redirect)
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '登录失败,请稍后重试'
    loginClosed.value = /登录已关闭|已关闭|已停用|禁用/.test(error.value)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="pg">
    <div class="pg__card">
      <div class="pg__brand">
        <span class="pg__logo">LiteBox</span>
        <span class="pg__scope">用户中心</span>
      </div>

      <div v-if="error" class="pg__error">
        {{ error }}
        <template v-if="loginClosed">
          你的订阅可能仍然可用 —— 客户端里已导入的订阅不受影响。需要恢复登录请联系管理员。
        </template>
      </div>

      <form class="pg__form" @submit.prevent="onSubmit">
        <label class="pg__field">
          <span class="pg__label">登录账号</span>
          <a-input
            v-model:value="form.username"
            placeholder="管理员分配的账号"
            autocomplete="username"
            size="large"
            :disabled="loading"
          />
        </label>
        <label class="pg__field">
          <span class="pg__label">密码</span>
          <a-input-password
            v-model:value="form.password"
            placeholder="请输入密码"
            autocomplete="current-password"
            size="large"
            :disabled="loading"
            @press-enter="onSubmit"
          />
        </label>
        <a-button
          type="primary"
          size="large"
          block
          html-type="submit"
          :loading="loading"
          :disabled="!form.username || !form.password"
        >
          {{ loading ? '登录中' : '登录' }}
        </a-button>
      </form>

      <div class="pg__foot">
        <div>忘记密码请联系管理员重置。</div>
        <div>管理员请前往 <RouterLink to="/login">管理后台</RouterLink></div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.pg {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px 16px;
  background: #f6f7f9;
}

.pg__card {
  width: 380px;
  max-width: calc(100vw - 32px);
  padding: 28px 28px 22px;
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

.pg__brand {
  display: flex;
  align-items: baseline;
  gap: 8px;
  margin-bottom: 22px;
}

.pg__logo {
  font-size: 20px;
  font-weight: 600;
  letter-spacing: -0.01em;
}

.pg__scope {
  padding: 1px 7px;
  background: #eef4fc;
  border: 1px solid #c9dcf3;
  border-radius: 3px;
  font-size: 11.5px;
  color: #1d4f96;
}

.pg__error {
  margin-bottom: 16px;
  padding: 10px 12px;
  background: #fdecea;
  border: 1px solid #f3cfc9;
  border-radius: 6px;
  font-size: 12px;
  line-height: 1.75;
  color: #8e2117;
}

.pg__form {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.pg__field {
  display: flex;
  flex-direction: column;
  gap: 5px;
}

.pg__label {
  font-size: 12.5px;
  font-weight: 500;
}

.pg__foot {
  margin-top: 18px;
  padding-top: 14px;
  border-top: 1px solid #edeff2;
  text-align: center;
  font-size: 12px;
  line-height: 1.9;
  color: #6b7480;
}

/* 键盘弹起时垂直居中会把标题顶出屏幕,窄屏改为顶部留 15vh。 */
@media (max-width: 767px) {
  .pg {
    align-items: flex-start;
    padding-top: 15vh;
  }

  .pg__card {
    width: calc(100vw - 32px);
  }

  .pg__form :deep(.ant-input),
  .pg__form :deep(.ant-input-affix-wrapper) {
    min-height: 44px;
  }

  .pg__form :deep(.ant-btn) {
    height: 46px;
  }
}
</style>
