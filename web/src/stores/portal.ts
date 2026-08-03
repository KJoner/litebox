import { defineStore } from 'pinia'
import { ref } from 'vue'
import { portalApi, ApiError, type PortalIdentity } from '@/api/client'

// 门户身份与管理员身份分开存:同一个浏览器里两者可以并存,
// 混在一个 store 里会让"退出后台"顺带把门户也退掉。
export const usePortalStore = defineStore('portal', () => {
  const identity = ref<PortalIdentity | null>(null)
  const resolved = ref(false)

  async function resolve(): Promise<void> {
    if (resolved.value) return
    try {
      identity.value = await portalApi.me()
    } catch (err) {
      // /auth/me 在强制改密期间也是放行的,所以这里拿到的 401 就是真的未登录。
      // 强制改密体现为 identity.must_change_password,由路由守卫处理。
      if (err instanceof ApiError && err.status === 401) {
        identity.value = null
      } else {
        throw err
      }
    } finally {
      resolved.value = true
    }
  }

  async function login(username: string, password: string): Promise<void> {
    identity.value = await portalApi.login(username, password)
    resolved.value = true
  }

  async function logout(): Promise<void> {
    try {
      await portalApi.logout()
    } finally {
      identity.value = null
      resolved.value = true
    }
  }

  function clear(): void {
    identity.value = null
    resolved.value = true
  }

  // 改密成功后清掉强制改密标志,免得用户被一直挡在改密页。
  function passwordChanged(): void {
    if (identity.value) identity.value.must_change_password = false
  }

  return { identity, resolved, resolve, login, logout, clear, passwordChanged }
})
