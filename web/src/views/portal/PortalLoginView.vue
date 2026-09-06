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
 * V18:与管理员登录页完全同构,胶囊改为「用户中心」,错误条互相导流到「管理后台」。
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
  <div class="pg">
    <div class="pg__col">
      <div class="pg__brand">
        <span class="pg__logo">LB</span>
        <div>
          <h1 class="pg__title">登录 LiteBox</h1>
          <span class="pg__scope">用户中心</span>
        </div>
      </div>

      <div class="pg__card">
        <div v-if="error" class="lb-error-strip pg__error">
          <span class="pg__error-icon"><LbIcon name="alert-triangle" :size="15" /></span>
          <div>
            <b>{{ error }}</b>
            <span v-if="loginClosed" class="pg__error-text">
              你的订阅可能仍然可用 —— 客户端里已导入的订阅不受影响。需要恢复登录请联系管理员。
            </span>
            <span v-else class="pg__error-text">
              如果你是管理员,请到 <RouterLink to="/login">管理后台</RouterLink> 登录。
            </span>
          </div>
        </div>

        <form class="pg__form" @submit.prevent="onSubmit">
          <div class="pg__group">
            <label class="pg__row">
              <span class="pg__label">登录账号</span>
              <input
                v-model="form.username"
                class="pg__input"
                type="text"
                placeholder="管理员分配的账号"
                autocomplete="username"
                autocapitalize="off"
                spellcheck="false"
                :disabled="loading"
              />
            </label>
            <label class="pg__row pg__row--pw">
              <span class="pg__label">密码</span>
              <input
                v-model="form.password"
                class="pg__input"
                :type="showPassword ? 'text' : 'password'"
                placeholder="请输入密码"
                autocomplete="current-password"
                :disabled="loading"
              />
              <a class="pg__toggle" tabindex="0" @click.prevent="showPassword = !showPassword" @keydown.enter.prevent="showPassword = !showPassword">
                {{ showPassword ? '隐藏' : '显示' }}
              </a>
            </label>
          </div>
          <button type="submit" class="pg__submit" :disabled="loading || !form.username || !form.password">
            {{ loading ? '登录中…' : '登录' }}
          </button>
        </form>
      </div>

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
  box-sizing: border-box;
  background: var(--bg);
  animation: lb-fadeup 0.4s var(--ease);
}

.pg__col {
  width: 400px;
  max-width: 100%;
  display: flex;
  flex-direction: column;
  gap: 22px;
}

.pg__brand {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 14px;
  text-align: center;
}

.pg__logo {
  width: 56px;
  height: 56px;
  border-radius: 17px;
  background: linear-gradient(145deg, var(--brand), var(--brand-hover));
  display: inline-flex;
  align-items: center;
  justify-content: center;
  color: #fff;
  font-size: 20px;
  font-weight: 700;
  letter-spacing: -0.02em;
  box-shadow: 0 8px 24px rgba(37, 99, 184, 0.3);
}

.pg__title {
  margin: 0;
  font-size: 28px;
  font-weight: 700;
  letter-spacing: -0.03em;
  line-height: 1.1;
}

.pg__scope {
  margin-top: 8px;
  display: inline-flex;
  align-items: center;
  padding: 3px 10px;
  border-radius: var(--r-pill);
  background: var(--fill);
  font-size: 12.5px;
  font-weight: 500;
  color: var(--text2);
}

.pg__card {
  background: var(--surface);
  border-radius: var(--r-sheet);
  box-shadow: var(--shadow);
  padding: 22px 22px 20px;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.pg__error {
  background: rgba(180, 41, 29, 0.08);
}
.pg__error-icon {
  flex: none;
  width: 28px;
  height: 28px;
  border-radius: 9px;
  background: var(--bad-bg);
  color: var(--bad);
  display: inline-flex;
  align-items: center;
  justify-content: center;
}
.pg__error-text {
  color: var(--text2);
}

.pg__form {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.pg__group {
  background: var(--surface2);
  border-radius: var(--r-group);
  overflow: hidden;
}

.pg__row {
  display: grid;
  grid-template-columns: 72px minmax(0, 1fr);
  align-items: center;
  gap: 12px;
  height: 50px;
  padding: 0 16px;
  cursor: text;
}
.pg__row--pw {
  grid-template-columns: 72px minmax(0, 1fr) auto;
}
.pg__row + .pg__row {
  border-top: 1px solid var(--sep);
}

.pg__label {
  font-size: 13.5px;
  font-weight: 500;
}

.pg__input {
  width: 100%;
  min-width: 0;
  height: 100%;
  border: none;
  background: transparent;
  color: var(--text);
  font-size: 15px;
  font-family: inherit;
  outline: none;
}
.pg__input::placeholder {
  color: var(--text3);
}
.pg__input:disabled {
  color: var(--text3);
}
.pg__input[type='password'] {
  letter-spacing: 0.18em;
}

.pg__toggle {
  font-size: 12.5px;
  font-weight: 500;
  white-space: nowrap;
  user-select: none;
}

.pg__submit {
  height: 46px;
  border: none;
  border-radius: 14px;
  background: var(--brand);
  color: #fff;
  font-size: 15px;
  font-weight: 600;
  font-family: inherit;
  cursor: pointer;
  transition: background 0.2s, transform 0.15s var(--ease), opacity 0.2s;
  box-shadow: 0 6px 16px rgba(37, 99, 184, 0.28);
}
.pg__submit:hover:not(:disabled) {
  background: var(--brand-hover);
}
.pg__submit:active:not(:disabled) {
  transform: scale(0.98);
}
.pg__submit:disabled {
  opacity: 0.4;
  cursor: default;
}

.pg__foot {
  text-align: center;
  font-size: 13px;
  line-height: 1.9;
  color: var(--text3);
}
.pg__foot a {
  font-weight: 500;
}

/* 键盘弹起时垂直居中会把标题顶出屏幕,窄屏改为顶部留 15vh。 */
@media (max-width: 767px) {
  .pg {
    align-items: flex-start;
    padding-top: 15vh;
  }
  .pg__row {
    height: 52px;
  }
}
</style>
