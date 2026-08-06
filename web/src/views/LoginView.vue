<script setup lang="ts">
import { reactive, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { ApiError } from '@/api/client'
import { useAuthStore } from '@/stores/auth'

/**
 * 管理员登录。与用户登录页是同一套布局的两个变体,
 * **关键在于它们必须能互相导流** —— 走错门是这个产品最高频的用户困惑。
 *
 * 管理员把面板首页发给用户是常事,用户在这里输账号只会得到
 * 「用户名或密码错误」,看起来完全就是密码发错了,然后来问管理员。
 * 所以错误提示里主动给出走错门的可能。
 */
const auth = useAuthStore()
const router = useRouter()
const route = useRoute()

const form = reactive({ username: '', password: '' })
const loading = ref(false)
const error = ref('')
/** 凭据错误时才提示「你可能走错门了」;锁定、网络错误提这个只会误导。 */
const wrongDoor = ref(false)

async function onSubmit() {
  if (loading.value) return
  loading.value = true
  error.value = ''
  wrongDoor.value = false
  try {
    await auth.login(form.username, form.password)
    const redirect = typeof route.query.redirect === 'string' ? route.query.redirect : '/dashboard'
    await router.replace(redirect)
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '登录失败,请稍后重试'
    wrongDoor.value = err instanceof ApiError && err.status === 401
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="lg">
    <div class="lg__card">
      <div class="lg__brand">
        <span class="lg__logo">LiteBox</span>
        <span class="lg__scope">管理后台</span>
      </div>
      <div class="lg__hint">管理员专用</div>

      <div v-if="error" class="lg__error">
        {{ error }}
        <template v-if="wrongDoor">
          如果你是普通用户,请到
          <RouterLink to="/">用户中心</RouterLink>
          登录 —— 这里只认管理员账号。
        </template>
      </div>

      <form class="lg__form" @submit.prevent="onSubmit">
        <label class="lg__field">
          <span class="lg__label">用户名</span>
          <a-input
            v-model:value="form.username"
            placeholder="admin"
            autocomplete="username"
            size="large"
            :disabled="loading"
          />
        </label>
        <label class="lg__field">
          <span class="lg__label">密码</span>
          <a-input-password
            v-model:value="form.password"
            placeholder="请输入密码"
            autocomplete="current-password"
            size="large"
            :disabled="loading"
            @press-enter="onSubmit"
          />
        </label>
        <!-- Enter 与按钮等效,提交中两个输入框同时禁用,不做二次提交。 -->
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

      <div class="lg__foot">
        普通用户请前往
        <RouterLink to="/">用户中心</RouterLink>
      </div>
    </div>
  </div>
</template>

<style scoped>
.lg {
  min-height: 100%;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px 16px;
  background: #f6f7f9;
}

.lg__card {
  width: 380px;
  max-width: calc(100vw - 32px);
  padding: 28px 28px 22px;
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

.lg__brand {
  display: flex;
  align-items: baseline;
  gap: 8px;
}

.lg__logo {
  font-size: 20px;
  font-weight: 600;
  letter-spacing: -0.01em;
}

.lg__scope {
  padding: 1px 7px;
  background: #f1f3f5;
  border: 1px solid #dfe3e8;
  border-radius: 3px;
  font-size: 11.5px;
  color: #5c6672;
}

.lg__hint {
  margin: 4px 0 20px;
  font-size: 12.5px;
  color: #6b7480;
}

.lg__error {
  margin-bottom: 16px;
  padding: 10px 12px;
  background: #fdecea;
  border: 1px solid #f3cfc9;
  border-radius: 6px;
  font-size: 12px;
  line-height: 1.75;
  color: #8e2117;
}

.lg__form {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.lg__field {
  display: flex;
  flex-direction: column;
  gap: 5px;
}

.lg__label {
  font-size: 12.5px;
  font-weight: 500;
}

.lg__foot {
  margin-top: 18px;
  padding-top: 14px;
  border-top: 1px solid #edeff2;
  text-align: center;
  font-size: 12px;
  color: #6b7480;
}

/* 键盘弹起时垂直居中会把标题顶出屏幕,窄屏改为顶部留 15vh。 */
@media (max-width: 767px) {
  .lg {
    align-items: flex-start;
    padding-top: 15vh;
  }

  .lg__card {
    width: calc(100vw - 32px);
  }

  .lg__form :deep(.ant-input),
  .lg__form :deep(.ant-input-affix-wrapper) {
    min-height: 44px;
  }

  .lg__form :deep(.ant-btn) {
    height: 46px;
  }
}
</style>
