<script setup lang="ts">
import { reactive, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { ApiError } from '@/api/client'
import { usePortalStore } from '@/stores/portal'
import { LbIcon } from '@/components/lb'

/**
 * 用户登录 · 路径 / · 首页。
 *
 * 首页给用户而不是给管理员:用户有十个、管理员只有一个。
 * 管理员开通账号后手边只有面板地址,发首页是最自然的动作。
 * 已登录的管理员访问它会被守卫直接送回后台,不会被晾在这里。
 *
 * 与管理员登录页同构(共用 base.css 的 .lb-auth*),适用范围写「用户中心」,错误条互相导流到「管理后台」。
 */
const portal = usePortalStore()
const router = useRouter()
const route = useRoute()

const form = reactive({ username: '', password: '' })
const loading = ref(false)
const error = ref('')
/** login_enabled=false 与整个账号被停用是两回事,文案必须不同。 */
const loginClosed = ref(false)
const showPassword = ref(false)

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
  <!-- 版式在 styles/base.css 的 .lb-auth*,与管理员登录页共用一份。 -->
  <div class="lb-auth">
    <div class="lb-auth__col">
      <div class="lb-auth__brand">
        <span class="lb-auth__logo">LB</span>
        <span class="lb-auth__name">LiteBox</span>
      </div>

      <div class="lb-auth__card">
        <div>
          <h1 class="lb-auth__title">登录 LiteBox</h1>
          <div class="lb-auth__scope">用户中心</div>
        </div>

        <div v-if="error" class="lb-error-strip">
          <span class="lb-auth__error-icon"><LbIcon name="alert-triangle" :size="15" /></span>
          <div>
            <b>{{ error }}</b>
            <span v-if="loginClosed" class="lb-auth__error-text">
              你的订阅可能仍然可用 —— 客户端里已导入的订阅不受影响。需要恢复登录请联系管理员。
            </span>
            <span v-else class="lb-auth__error-text">
              如果你是管理员,请到 <RouterLink to="/login">管理后台</RouterLink> 登录。
            </span>
          </div>
        </div>

        <form class="lb-auth__form" @submit.prevent="onSubmit">
          <label class="lb-auth__field">
            <span class="lb-auth__label">登录账号</span>
            <input
              v-model="form.username"
              class="lb-auth__input"
              type="text"
              placeholder="管理员分配的账号"
              autocomplete="username"
              autocapitalize="off"
              spellcheck="false"
              :disabled="loading"
            />
          </label>
          <div class="lb-auth__field">
            <span class="lb-auth__label">
              <label for="pg-password">密码</label>
              <button type="button" class="lb-auth__toggle" @click="showPassword = !showPassword">
                {{ showPassword ? '隐藏' : '显示' }}
              </button>
            </span>
            <input
              id="pg-password"
              v-model="form.password"
              class="lb-auth__input"
              :type="showPassword ? 'text' : 'password'"
              placeholder="请输入密码"
              autocomplete="current-password"
              :disabled="loading"
            />
          </div>
          <button type="submit" class="lb-auth__submit" :disabled="loading || !form.username || !form.password">
            {{ loading ? '登录中…' : '登录' }}
          </button>
        </form>
      </div>

      <div class="lb-auth__foot">
        <div>忘记密码请联系管理员重置。</div>
        <div>管理员请前往 <RouterLink to="/login">管理后台</RouterLink></div>
      </div>
    </div>
  </div>
</template>
