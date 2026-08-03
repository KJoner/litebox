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

  const init: RequestInit = { method, credentials: 'same-origin', headers: {} }
  if (body !== undefined) {
    init.headers = { 'Content-Type': 'application/json' }
    init.body = JSON.stringify(body)
  }

  const response = await fetch(url, init)
  if (response.status === 204) return undefined as T

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

// ---------- 类型 ----------

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

export type UserStatus =
  | 'ACTIVE'
  | 'DISABLED'
  | 'EXPIRED'
  | 'QUOTA_EXCEEDED'
  | 'DEPLOY_PENDING'
  | 'DEPLOY_FAILED'

export interface ProxyUser {
  id: number
  user_code: string
  display_name: string
  remark: string
  status: UserStatus
  quota_bytes: number
  used_uplink: number
  used_downlink: number
  used_total: number
  expires_at: string | null
  reset_cycle: 'NONE' | 'MONTHLY'
  reset_day: number
  last_reset_at: string | null
  node_ids: number[]
  created_at: string
  updated_at: string
  sub_last_access_at: string | null
  sub_last_access_ip: string
  sub_last_user_agent: string
  sub_access_count: number
  // 仅详情接口返回
  uuid?: string
  sub_token?: string
  subscription_url?: string
}

export type NodeStatus = 'PENDING' | 'ONLINE' | 'OFFLINE' | 'DISABLED' | 'DEPLOY_FAILED'

export interface Node {
  id: number
  name: string
  host: string
  ssh_port: number
  ssh_user: string
  /** 客户端连接的公网端口,写进订阅 */
  proxy_port: number
  /** sing-box 在节点上监听的端口,NAT / nginx 转发时与 proxy_port 不同 */
  listen_port: number
  api_port: number
  arch: string
  singbox_version: string
  singbox_build_tags: string
  reality_dest: string
  reality_dest_port: number
  reality_public_key: string
  reality_short_id: string
  handshake_max_record_size: number
  handshake_checked_at: string | null
  status: NodeStatus
  last_heartbeat_at: string | null
  config_revision: number
  deployed_config_sha256: string
  created_at: string
  updated_at: string
}

export interface NodeUpdateEffect {
  /** 连接参数变了,面板已丢弃该节点的 SSH 长连接 */
  ssh_changed: boolean
  /** 节点上跑的配置已与期望状态不一致,需要重新部署才生效 */
  needs_deploy: boolean
  changes: string[]
}

export interface ProbeResult {
  arch: string
  kernel: string
  os_name: string
  mem_total_mb: number
  singbox_path: string
  singbox_version: string
  build_tags: string[]
  has_v2ray_api: boolean
  systemd_version: string
  problems: string[]
}

export interface DestCheckResult {
  server: string
  port: number
  usable: boolean
  tls13: boolean
  curve_name: string
  alpn: string
  max_record_size: number
  record_sizes: number[]
  cert_issuer: string
  cert_chain_len: number
  problems: string[]
  warnings: string[]
  checked_at: string
}

export interface DeployStep {
  name: string
  status: 'SUCCESS' | 'FAILED' | 'SKIPPED'
  duration_ms: number
  detail?: string
}

export interface DeployResult {
  node_id: number
  revision: number
  config_sha256: string
  status: 'SUCCESS' | 'FAILED' | 'ROLLED_BACK'
  steps: DeployStep[]
  error_message?: string
  rollback_result?: string
  started_at: string
  finished_at: string
}

export interface DeploymentRecord {
  id: number
  node_id: number
  revision: number
  config_sha256: string
  status: string
  started_at: string
  finished_at: string | null
  error_message: string
  rollback_result: string
  steps: DeployStep[]
}

export interface ConfigDiff {
  node_id: number
  revision: number
  desired_sha256: string
  remote_sha256: string
  in_sync: boolean
  desired_users: string[] | null
  diff: {
    changed: boolean
    users: { added: string[] | null; removed: string[] | null; uuid_reset: string[] | null }
    node_attr: string[] | null
    summary: string
  }
}

export interface DailyPoint {
  day: string
  uplink: number
  downlink: number
  total: number
}

export interface UserNodeTraffic {
  user_code: string
  node_id: number
  node_name: string
  uplink: number
  downlink: number
  total: number
}

export interface UserTraffic {
  user_code: string
  used_uplink: number
  used_downlink: number
  used_total: number
  quota_bytes: number
  by_node: UserNodeTraffic[]
  daily: DailyPoint[]
}

export interface SyncResult {
  node_id: number
  batch_id: string
  restarted: boolean
  counters_read: number
  entries_added: number
  bytes_added: number
  synced_at: string
}

export interface TrafficStatus {
  last_run?: string
  failing_nodes: { node_id: number; error: string }[]
}

// ---------- 接口 ----------

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

  auditLogs: (params: { limit?: number; targetType?: string; targetId?: string } = {}) =>
    request<{ items: AuditLog[] }>('/api/audit-logs', {
      query: {
        limit: params.limit ?? 100,
        target_type: params.targetType,
        target_id: params.targetId,
      },
    }),

  // 用户
  users: () => request<{ items: ProxyUser[] }>('/api/users'),
  user: (id: number) => request<ProxyUser>(`/api/users/${id}`),
  createUser: (body: Record<string, unknown>) =>
    request<ProxyUser>('/api/users', { method: 'POST', body }),
  updateUser: (id: number, body: Record<string, unknown>) =>
    request<ProxyUser>(`/api/users/${id}`, { method: 'PATCH', body }),
  deleteUser: (id: number) =>
    request<{ message: string }>(`/api/users/${id}`, { method: 'DELETE' }),
  setUserEnabled: (id: number, enabled: boolean) =>
    request<ProxyUser>(`/api/users/${id}/enabled`, { method: 'POST', body: { enabled } }),
  resetUserTraffic: (id: number) =>
    request<ProxyUser>(`/api/users/${id}/reset-traffic`, { method: 'POST' }),
  regenerateUserUUID: (id: number) =>
    request<ProxyUser>(`/api/users/${id}/regenerate-uuid`, { method: 'POST' }),
  regenerateSubToken: (id: number) =>
    request<ProxyUser>(`/api/users/${id}/regenerate-sub-token`, { method: 'POST' }),
  userTraffic: (id: number, days = 30) =>
    request<UserTraffic>(`/api/users/${id}/traffic`, { query: { days } }),

  // 节点
  nodes: () => request<{ items: Node[] }>('/api/nodes'),
  node: (id: number) => request<Node>(`/api/nodes/${id}`),
  createNode: (body: Record<string, unknown>) =>
    request<Node>('/api/nodes', { method: 'POST', body }),
  updateNode: (id: number, body: Record<string, unknown>) =>
    request<{ node: Node; effect: NodeUpdateEffect }>(`/api/nodes/${id}`, { method: 'PUT', body }),
  deleteNode: (id: number) =>
    request<{ message: string }>(`/api/nodes/${id}`, { method: 'DELETE' }),
  setNodeEnabled: (id: number, enabled: boolean) =>
    request<{ message: string }>(`/api/nodes/${id}/enabled`, { method: 'POST', body: { enabled } }),
  testNodeSSH: (id: number) =>
    request<{ ok: boolean; uname: string }>(`/api/nodes/${id}/test-ssh`, { method: 'POST' }),
  probeNode: (id: number) => request<ProbeResult>(`/api/nodes/${id}/probe`, { method: 'POST' }),
  installNode: (id: number) =>
    request<{ binary_path: string; binary_sha256: string; service_name: string; installed: boolean; detail: string }>(
      `/api/nodes/${id}/install`,
      { method: 'POST' },
    ),
  checkNodeDest: (id: number, dest: string, apply = false) =>
    request<DestCheckResult>(`/api/nodes/${id}/dest-check`, {
      method: 'POST',
      body: { dest, port: 443, apply },
    }),
  scanNodeDests: (id: number) =>
    request<{ items: DestCheckResult[] }>(`/api/nodes/${id}/dest-scan`, { method: 'POST' }),
  deployNode: (id: number) =>
    request<DeployResult>(`/api/nodes/${id}/deploy`, { method: 'POST' }),
  restartNode: (id: number) =>
    request<{ message: string }>(`/api/nodes/${id}/restart`, { method: 'POST' }),
  resetNodeHostKey: (id: number) =>
    request<{ message: string }>(`/api/nodes/${id}/reset-host-key`, { method: 'POST' }),
  nodeDeployments: (id: number, limit = 20) =>
    request<{ items: DeploymentRecord[] }>(`/api/nodes/${id}/deployments`, { query: { limit } }),
  nodeConfigDiff: (id: number) => request<ConfigDiff>(`/api/nodes/${id}/config-diff`),
  nodeTraffic: (id: number, days = 30) =>
    request<{ node_id: number; daily: DailyPoint[] }>(`/api/nodes/${id}/traffic`, {
      query: { days },
    }),
  destCandidates: () =>
    request<{ items: string[]; max_record_size: number }>('/api/dest-candidates'),

  // 流量与部署
  deployments: (limit = 50) =>
    request<{ items: DeploymentRecord[] }>('/api/deployments', { query: { limit } }),
  syncNodeTraffic: (id: number) =>
    request<SyncResult>(`/api/nodes/${id}/sync-traffic`, { method: 'POST' }),
  trafficStatus: () => request<TrafficStatus>('/api/traffic/status'),
  nodesTodayTraffic: () =>
    request<{ items: { node_id: number; bytes: number }[] }>('/api/traffic/nodes-today'),
}
