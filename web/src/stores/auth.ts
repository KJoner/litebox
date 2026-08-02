import { defineStore } from 'pinia'
import { ref } from 'vue'
import { api, ApiError, type Admin } from '@/api/client'

export const useAuthStore = defineStore('auth', () => {
  const admin = ref<Admin | null>(null)
  // resolved 表示是否已经向后端确认过一次登录状态。
  // 路由守卫要靠它区分"尚未确认"和"确认为未登录"。
  const resolved = ref(false)

  async function resolve(): Promise<void> {
    if (resolved.value) return
    try {
      admin.value = await api.me()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        admin.value = null
      } else {
        throw err
      }
    } finally {
      resolved.value = true
    }
  }

  async function login(username: string, password: string): Promise<void> {
    admin.value = await api.login(username, password)
    resolved.value = true
  }

  async function logout(): Promise<void> {
    try {
      await api.logout()
    } finally {
      admin.value = null
      resolved.value = true
    }
  }

  // 会话在后端失效时(过期、改密后被踢),前端要同步清空状态。
  function clear(): void {
    admin.value = null
    resolved.value = true
  }

  return { admin, resolved, resolve, login, logout, clear }
})
