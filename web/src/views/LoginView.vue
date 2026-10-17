<script setup lang="ts">
import { reactive, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { ApiError } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import { LbIcon } from '@/components/lb'

/**
 * 管理员登录。与用户登录页是同一套布局的两个变体,
 * **关键在于它们必须能互相导流** —— 走错门是这个产品最高频的用户困惑。
 *
 * 管理员把面板首页发给用户是常事,用户在这里输账号只会得到
 * 「用户名或密码错误」,看起来完全就是密码发错了,然后来问管理员。
 * 所以错误提示里主动给出走错门的可能。
 *
 * UI 2.0:居中 400 宽卡片,标签在输入框上方,版式与用户登录页共用 base.css 的 .lb-auth*。
 */
const auth = useAuthStore()
const router = useRouter()
const route = useRoute()

const form = reactive({ username: '', password: '' })
const loading = ref(false)
const error = ref('')
/** 凭据错误时才提示「你可能走错门了」;锁定、网络错误提这个只会误导。 */
const wrongDoor = ref(false)
const showPassword = ref(false)

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
  <!-- 版式在 styles/base.css 的 .lb-auth*,与用户登录页共用一份。 -->
  <div class="lb-auth">
    <div class="lb-auth__col">
      <div class="lb-auth__brand">
        <span class="lb-auth__logo">LB</span>
        <span class="lb-auth__name">LiteBox</span>
      </div>

      <div class="lb-auth__card">
        <div>
          <h1 class="lb-auth__title">登录 LiteBox</h1>
          <div class="lb-auth__scope">管理后台 · 管理员专用</div>
        </div>

        <div v-if="error" class="lb-error-strip">
          <span class="lb-auth__error-icon"><LbIcon name="alert-triangle" :size="15" /></span>
          <div>
            <b>{{ error }}</b>
            <span v-if="wrongDoor" class="lb-auth__error-text">
              如果你是普通用户,请到
              <RouterLink to="/">用户中心</RouterLink>
              登录 —— 这里只认管理员账号。
            </span>
          </div>
        </div>

        <form class="lb-auth__form" @submit.prevent="onSubmit">
          <label class="lb-auth__field">
            <span class="lb-auth__label">用户名</span>
            <input
              v-model="form.username"
              class="lb-auth__input"
              type="text"
              placeholder="admin"
              autocomplete="username"
              autocapitalize="off"
              spellcheck="false"
              :disabled="loading"
            />
          </label>
          <div class="lb-auth__field">
            <span class="lb-auth__label">
              <label for="lg-password">密码</label>
              <button type="button" class="lb-auth__toggle" @click="showPassword = !showPassword">
                {{ showPassword ? '隐藏' : '显示' }}
              </button>
            </span>
            <input
              id="lg-password"
              v-model="form.password"
              class="lb-auth__input"
              :type="showPassword ? 'text' : 'password'"
              placeholder="请输入密码"
              autocomplete="current-password"
              :disabled="loading"
            />
          </div>
          <!-- Enter 与按钮等效,提交中两个输入框同时禁用,不做二次提交。 -->
          <button
            type="submit"
            class="lb-auth__submit"
            :disabled="loading || !form.username || !form.password"
          >
            {{ loading ? '登录中…' : '登录' }}
          </button>
        </form>
      </div>

      <div class="lb-auth__foot">
        普通用户请前往
        <RouterLink to="/">用户中心</RouterLink>
      </div>
    </div>
  </div>
</template>
