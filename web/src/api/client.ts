// API 客户端。会话通过 HttpOnly Cookie 传递,因此所有请求都要带 credentials。

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

interface RequestOptions {
  method?: string
  body?: unknown
  query?: Record<string, string | number | undefined>
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body, query } = options

  let url = path
  if (query) {
    const params = new URLSearchParams()
    for (const [key, value] of Object.entries(query)) {
      if (value !== undefined) params.set(key, String(value))
    }
    const qs = params.toString()
    if (qs) url += `?${qs}`
  }

  const init: RequestInit = {
    method,
    credentials: 'same-origin',
    headers: {},
  }
  if (body !== undefined) {
    init.headers = { 'Content-Type': 'application/json' }
    init.body = JSON.stringify(body)
  }

  const response = await fetch(url, init)

  if (response.status === 204) {
    return undefined as T
  }

  let payload: unknown
  const text = await response.text()
  if (text) {
    try {
      payload = JSON.parse(text)
    } catch {
      throw new ApiError(response.status, '服务器返回了无法解析的响应')
    }
  }

  if (!response.ok) {
    const message =
      payload && typeof payload === 'object' && 'error' in payload
        ? String((payload as { error: unknown }).error)
        : `请求失败(HTTP ${response.status})`
    throw new ApiError(response.status, message)
  }

  return payload as T
}

export interface Admin {
  id: number
  username: string
}

export interface DashboardSummary {
  user_total: number
  user_active: number
  node_total: number
  node_online: number
  traffic_today: number
  traffic_month: number
  quota_exceeded: number
  expiring_soon: number
  failed_deploys: number
}

export interface AuditLog {
  id: number
  admin_user_id: number | null
  action: string
  target_type: string
  target_id: string
  detail: string
  client_ip: string
  succeeded: boolean
  created_at: string
}

export const api = {
  login: (username: string, password: string) =>
    request<Admin>('/api/auth/login', { method: 'POST', body: { username, password } }),

  logout: () => request<{ message: string }>('/api/auth/logout', { method: 'POST' }),

  me: () => request<Admin>('/api/auth/me'),

  changePassword: (oldPassword: string, newPassword: string) =>
    request<{ message: string }>('/api/auth/password', {
      method: 'POST',
      body: { old_password: oldPassword, new_password: newPassword },
    }),

  dashboardSummary: () => request<DashboardSummary>('/api/dashboard/summary'),

  auditLogs: (limit = 50, offset = 0) =>
    request<{ items: AuditLog[] }>('/api/audit-logs', { query: { limit, offset } }),
}
