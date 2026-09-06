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
 * V18:居中列宽 400,蓝渐变 Logo,表单是分组列表两行 50px,主按钮 46px 圆角 14。
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
  <div class="lg">
    <div class="lg__col">
      <div class="lg__brand">
        <span class="lg__logo">LB</span>
        <div>
          <h1 class="lg__title">登录 LiteBox</h1>
          <span class="lg__scope">管理后台 · 管理员专用</span>
        </div>
      </div>

      <div class="lg__card">
        <div v-if="error" class="lb-error-strip lg__error">
          <span class="lg__error-icon"><LbIcon name="alert-triangle" :size="15" /></span>
          <div>
            <b>{{ error }}</b>
            <span v-if="wrongDoor" class="lg__error-text">
              如果你是普通用户,请到
              <RouterLink to="/">用户中心</RouterLink>
              登录 —— 这里只认管理员账号。
            </span>
          </div>
        </div>

        <form class="lg__form" @submit.prevent="onSubmit">
          <div class="lg__group">
            <label class="lg__row">
              <span class="lg__label">用户名</span>
              <input
                v-model="form.username"
                class="lg__input"
                type="text"
                placeholder="admin"
                autocomplete="username"
                autocapitalize="off"
                spellcheck="false"
                :disabled="loading"
              />
            </label>
            <label class="lg__row lg__row--pw">
              <span class="lg__label">密码</span>
              <input
                v-model="form.password"
                class="lg__input"
                :type="showPassword ? 'text' : 'password'"
                placeholder="请输入密码"
                autocomplete="current-password"
                :disabled="loading"
              />
              <a class="lg__toggle" tabindex="0" @click.prevent="showPassword = !showPassword" @keydown.enter.prevent="showPassword = !showPassword">
                {{ showPassword ? '隐藏' : '显示' }}
              </a>
            </label>
          </div>
          <!-- Enter 与按钮等效,提交中两个输入框同时禁用,不做二次提交。 -->
          <button
            type="submit"
            class="lg__submit"
            :disabled="loading || !form.username || !form.password"
          >
            {{ loading ? '登录中…' : '登录' }}
          </button>
        </form>
      </div>

      <div class="lg__foot">
        普通用户请前往
        <RouterLink to="/">用户中心</RouterLink>
      </div>
    </div>
  </div>
</template>

<style scoped>
.lg {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px 16px;
  box-sizing: border-box;
  background: var(--bg);
  animation: lb-fadeup 0.4s var(--ease);
}

.lg__col {
  width: 400px;
  max-width: 100%;
  display: flex;
  flex-direction: column;
  gap: 22px;
}

.lg__brand {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 14px;
  text-align: center;
}

.lg__logo {
  width: 56px;
  height: 56px;
  border-radius: 17px;
  background: linear-gradient(145deg, var(--brand), var(--brand-hover));
  display: inline-flex;
  align-items: center;
  justify-content: center;
  color: var(--surface);
  font-size: 20px;
  font-weight: 700;
  letter-spacing: -0.02em;
  box-shadow: 0 8px 24px rgba(37, 99, 184, 0.3);
}

.lg__title {
  margin: 0;
  font-size: 28px;
  font-weight: 700;
  letter-spacing: -0.03em;
  line-height: 1.1;
}

.lg__scope {
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

.lg__card {
  background: var(--surface);
  border-radius: var(--r-sheet);
  box-shadow: var(--shadow);
  padding: 22px 22px 20px;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.lg__error {
  background: rgba(180, 41, 29, 0.08);
}
.lg__error-icon {
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
.lg__error-text {
  color: var(--text2);
}

.lg__form {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.lg__group {
  background: var(--surface2);
  border-radius: var(--r-group);
  overflow: hidden;
}

.lg__row {
  display: grid;
  grid-template-columns: 72px minmax(0, 1fr);
  align-items: center;
  gap: 12px;
  height: 50px;
  padding: 0 16px;
  cursor: text;
}
.lg__row--pw {
  grid-template-columns: 72px minmax(0, 1fr) auto;
}
.lg__row + .lg__row {
  border-top: 1px solid var(--sep);
}

.lg__label {
  font-size: 13.5px;
  font-weight: 500;
}

.lg__input {
  width: 100%;
  min-width: 0;
  height: 100%;
  border: none;
  background: transparent;
  color: var(--text);
  font-size: 15px;
  font-family: inherit;
  font-variant-numeric: tabular-nums;
  outline: none;
}
.lg__input::placeholder {
  color: var(--text3);
}
.lg__input:disabled {
  color: var(--text3);
}
.lg__input[type='password'] {
  letter-spacing: 0.18em;
}

.lg__toggle {
  font-size: 12.5px;
  font-weight: 500;
  white-space: nowrap;
  user-select: none;
}

.lg__submit {
  height: 46px;
  border: none;
  border-radius: 14px;
  background: var(--brand);
  color: var(--surface);
  font-size: 15px;
  font-weight: 600;
  font-family: inherit;
  cursor: pointer;
  transition: background 0.2s, transform 0.15s var(--ease), opacity 0.2s;
  box-shadow: 0 6px 16px rgba(37, 99, 184, 0.28);
}
.lg__submit:hover:not(:disabled) {
  background: var(--brand-hover);
}
.lg__submit:active:not(:disabled) {
  transform: scale(0.98);
}
.lg__submit:disabled {
  opacity: 0.4;
  cursor: default;
}

.lg__foot {
  text-align: center;
  font-size: 13px;
  color: var(--text3);
}
.lg__foot a {
  font-weight: 500;
}

/* 键盘弹起时垂直居中会把标题顶出屏幕,窄屏改为顶部留 15vh。 */
@media (max-width: 767px) {
  .lg {
    align-items: flex-start;
    padding-top: 15vh;
  }
  .lg__row {
    height: 52px;
  }
}
</style>
