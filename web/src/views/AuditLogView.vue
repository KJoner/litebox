<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type AuditLog, type Node, type ProxyUser } from '@/api/client'
import { LbEmptyState, LbFilterBar, LbRowCard, LbStatusTag, LbTimeText } from '@/components/lb'
import { useNarrow } from '@/composables/useNarrow'
import { usePagination } from '@/composables/usePagination'

/**
 * 审计日志:按时间回溯 —— 这件事是谁在什么时候做的。
 *
 * 三处与原实现不同:
 *   六分类分段筛选  27 个动作名要靠打字才能筛,而管理员想问的通常是
 *                  「今天有人动过用户吗」这种按类的问题。分类直接来自
 *                  动作名前缀(user./node./admin./traffic./settings.),
 *                  node.deploy 单独提成「部署」,不需要新字段。
 *   英文常量收进 title  它对管理员没用(他不看代码),但每行多 15px 高,
 *                  20 行就是 300px。
 *   detail 两行夹断  编辑用户能列出六项变更,原来 break-all 铺开会撑成五行。
 */
const narrow = useNarrow()
const logs = ref<AuditLog[]>([])
const nodes = ref<Node[]>([])
const users = ref<ProxyUser[]>([])
const loading = ref(true)
const loadError = ref<{ message: string; status?: number; at: string } | null>(null)
const expanded = ref<Set<number>>(new Set())

const actionNames: Record<string, string> = {
  'admin.login': '管理员登录',
  'admin.login_failed': '登录失败',
  'admin.logout': '管理员注销',
  'admin.change_password': '修改密码',
  'node.create': '新增节点',
  'node.update': '修改节点',
  'node.delete': '删除节点',
  'node.enable': '启用节点',
  'node.disable': '禁用节点',
  'node.probe': '探测节点',
  'node.dest_check': '检测握手目标',
  'node.bootstrap': '引导节点接入',
  'node.install': '安装 sing-box',
  'node.uninstall': '卸载节点服务',
  'node.deploy': '部署节点',
  'node.restart': '重启节点服务',
  'node.reset_host_key': '重置主机密钥',
  'user.create': '新增用户',
  'user.update': '编辑用户',
  'user.enable': '启用用户',
  'user.disable': '停用用户',
  'user.reset_traffic': '重置流量',
  'user.regenerate_uuid': '重新生成 UUID',
  'user.regenerate_sub_token': '重新生成订阅地址',
  'user.delete': '删除用户',
  'user.adjust': '续期 / 调整',
  'user.batch': '批量调整',
  'portal.account_set': '设置门户登录账号',
  'portal.account_delete': '删除门户登录账号',
  'portal.login_enable': '开启门户登录',
  'portal.login_disable': '关闭门户登录',
  'portal.revoke_sessions': '撤销门户会话',
  'traffic.sync': '同步流量',
  'settings.update': '修改面板设置',
  'tier.update': '修改访问等级',
}

type Category = 'user' | 'node' | 'deploy' | 'login' | 'traffic' | 'settings'

const categoryNames: Record<Category, string> = {
  user: '用户',
  node: '节点',
  deploy: '部署',
  login: '登录',
  traffic: '流量',
  settings: '设置',
}

/**
 * 分类来自动作名前缀,node.deploy 单独提出来。
 *
 * portal.* 归到「用户」而不是「登录」:它们是管理员对某个用户的门户账号做的操作,
 * 与 admin.login 那种「谁在登录面板」不是一回事。
 */
function categoryOf(action: string): Category {
  if (action === 'node.deploy') return 'deploy'
  if (action.startsWith('user.') || action.startsWith('portal.')) return 'user'
  if (action.startsWith('node.')) return 'node'
  if (action.startsWith('traffic.')) return 'traffic'
  if (action.startsWith('settings.') || action.startsWith('tier.')) return 'settings'
  return 'login'
}

/** 目标列显示节点名 / 用户编号,不显示裸 ID。 */
function targetOf(l: AuditLog): string {
  if (!l.target_id) return '—'
  if (l.target_type === 'node') {
    const n = nodes.value.find((x) => String(x.id) === l.target_id)
    return n ? n.display_name || n.name : `节点 ${l.target_id}`
  }
  if (l.target_type === 'user') {
    const u = users.value.find((x) => x.user_code === l.target_id)
    return u ? `${u.display_name}(${u.user_code})` : l.target_id
  }
  return l.target_id
}

/**
 * 登录失败的频次。孤立的一条「密码错误」是噪音,
 * 「该 IP 近 1 小时第 14 次」才是信号。
 */
function loginFailureBurst(l: AuditLog): number {
  if (l.action !== 'admin.login_failed' || !l.client_ip) return 0
  const t = new Date(l.created_at).getTime()
  return logs.value.filter(
    (x) =>
      x.action === 'admin.login_failed' &&
      x.client_ip === l.client_ip &&
      Math.abs(new Date(x.created_at).getTime() - t) <= 3600_000,
  ).length
}

// ---------- 筛选 ----------

const blankFilters = {
  keyword: '',
  category: undefined as Category | undefined,
  days: undefined as number | undefined,
  onlyFailed: false,
}
const filters = reactive({ ...blankFilters })

const activeFilterCount = computed(
  () =>
    (filters.keyword.trim() ? 1 : 0) +
    (filters.category !== undefined ? 1 : 0) +
    (filters.days !== undefined ? 1 : 0) +
    (filters.onlyFailed ? 1 : 0),
)

function clearFilters() {
  Object.assign(filters, blankFilters)
}

const visible = computed(() =>
  logs.value.filter((l) => {
    if (filters.onlyFailed && l.succeeded) return false
    if (filters.category !== undefined && categoryOf(l.action) !== filters.category) return false
    if (filters.days !== undefined) {
      if (new Date(l.created_at).getTime() < Date.now() - filters.days * 86400000) return false
    }
    const kw = filters.keyword.trim().toLowerCase()
    if (!kw) return true
    const hay = [actionNames[l.action] ?? l.action, l.action, targetOf(l), l.detail, l.client_ip]
      .join(' ')
      .toLowerCase()
    return hay.includes(kw)
  }),
)

const pager = usePagination('audit', () => visible.value.length)

/** 窄屏按天分组。今天 / 昨天用词而不是日期 —— 那是最常查的两天。 */
const grouped = computed(() => {
  const today = new Date().toISOString().slice(0, 10)
  const yesterday = new Date(Date.now() - 86400000).toISOString().slice(0, 10)
  const out: { day: string; label: string; items: AuditLog[] }[] = []
  // 分组建立在**当前页**之上,不是全部结果 —— 否则窄屏会一次铺出 200 条。
  for (const l of pager.slice(visible.value)) {
    const day = l.created_at.slice(0, 10)
    let g = out.find((x) => x.day === day)
    if (!g) {
      const suffix = day === today ? ' 今天' : day === yesterday ? ' 昨天' : ''
      g = { day, label: day.slice(5) + suffix, items: [] }
      out.push(g)
    }
    g.items.push(l)
  }
  return out
})

const counts = computed(() => {
  const out: Record<string, number> = { all: logs.value.length }
  for (const c of Object.keys(categoryNames)) {
    out[c] = logs.value.filter((l) => categoryOf(l.action) === c).length
  }
  return out
})

// ---------- 取数 ----------

async function load() {
  loading.value = true
  loadError.value = null
  expanded.value = new Set()
  try {
    logs.value = (await api.auditLogs({ limit: 200 })).items
  } catch (err) {
    loadError.value = {
      message: err instanceof ApiError ? err.message : '加载审计日志失败',
      status: err instanceof ApiError ? err.status : undefined,
      at: new Date().toLocaleTimeString(),
    }
    logs.value = []
  } finally {
    loading.value = false
  }
  // 目标列要把 ID 翻译成名字。取不到就退回显示原始 ID,不影响日志本身。
  api
    .nodes()
    .then((r) => (nodes.value = r.items))
    .catch(() => (nodes.value = []))
  api
    .users()
    .then((r) => (users.value = r.items))
    .catch(() => (users.value = []))
}

onMounted(load)

/**
 * 导出当前筛选结果为 CSV。纯前端生成 —— 数据已经在内存里,
 * 为此加一个后端接口只会多一条要维护的路由。
 */
function exportCSV() {
  const rows = [
    ['时间(UTC)', '分类', '操作', '目标', '结果', '来源 IP', '详情'],
    ...visible.value.map((l) => [
      l.created_at,
      categoryNames[categoryOf(l.action)],
      actionNames[l.action] ?? l.action,
      targetOf(l),
      l.succeeded ? '成功' : '失败',
      l.client_ip || '系统任务',
      l.detail,
    ]),
  ]
  const csv = rows
    .map((r) => r.map((c) => `"${String(c).replace(/"/g, '""')}"`).join(','))
    .join('\r\n')
  // BOM:Excel 不加它会把中文读成乱码。
  const blob = new Blob(['﻿' + csv], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `litebox-audit-${new Date().toISOString().slice(0, 10)}.csv`
  a.click()
  URL.revokeObjectURL(url)
  message.success(`已导出 ${visible.value.length} 条`)
}

function toggleExpand(id: number) {
  const next = new Set(expanded.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  expanded.value = next
}

const columns = [
  { title: '时间', key: 'time', width: 150 },
  { title: '操作', key: 'action', width: 200 },
  { title: '目标', key: 'target', width: 180 },
  { title: '结果', key: 'result', width: 90 },
  { title: '来源', key: 'ip', width: 140 },
  { title: '详情', key: 'detail' },
]
</script>

<template>
  <div class="al">
    <div class="al__head">
      <div>
        <h2 class="al__title">审计日志</h2>
        <div class="al__sub">
          最近 200 条 · 记录所有写操作与登录尝试 · 详情里不含 token、密码与 UUID 全值
        </div>
      </div>
      <a-space>
        <a-button :disabled="visible.length === 0" @click="exportCSV">导出 CSV</a-button>
        <a-button :loading="loading" @click="load">刷新</a-button>
      </a-space>
    </div>

    <a-card :body-style="{ padding: 0 }">
      <!-- 分类常驻,不占额外高度。27 个动作名靠打字筛太慢。 -->
      <div class="al__cats">
        <button
          class="al__cat"
          :class="{ 'al__cat--on': filters.category === undefined }"
          @click="filters.category = undefined"
        >
          全部 <span class="lb-mono">{{ counts.all }}</span>
        </button>
        <button
          v-for="(name, key) in categoryNames"
          :key="key"
          class="al__cat"
          :class="{ 'al__cat--on': filters.category === key }"
          @click="filters.category = key"
        >
          {{ name }} <span class="lb-mono">{{ counts[key] }}</span>
        </button>
      </div>

      <LbFilterBar
        :active-count="activeFilterCount"
        :filtered="visible.length"
        :total="logs.length"
        @clear="clearFilters"
      >
        <a-input-search
          v-model:value="filters.keyword"
          placeholder="搜索操作、目标、详情或 IP"
          allow-clear
          style="width: 240px"
        />
        <a-select v-model:value="filters.days" placeholder="时间范围" allow-clear style="width: 130px">
          <a-select-option :value="1">近 24 小时</a-select-option>
          <a-select-option :value="7">近 7 天</a-select-option>
          <a-select-option :value="30">近 30 天</a-select-option>
        </a-select>
        <a-checkbox v-model:checked="filters.onlyFailed">只看失败</a-checkbox>
      </LbFilterBar>

      <LbEmptyState
        v-if="loadError"
        variant="error"
        :title="loadError.message"
        description="分页器与筛选条保持可见,重试后回到原来那一页。"
        :http-status="loadError.status"
        :occurred-at="loadError.at"
        @retry="load"
      />
      <LbEmptyState
        v-else-if="!loading && logs.length === 0"
        variant="empty"
        title="还没有审计记录"
        description="日志从面板首次启动时开始记录。"
      />
      <!-- 「没有失败记录」是好消息,文案不用红色也不放插画。 -->
      <LbEmptyState
        v-else-if="!loading && visible.length === 0"
        variant="filtered"
        title="没有符合条件的记录"
        :description="`当前有 ${activeFilterCount} 项筛选生效,${logs.length} 条记录被筛掉。`"
        @clear="clearFilters"
      />

      <!--
        <768 换卡片,并按天分组:时间列取消,改成按天的小标题 + 卡内只留时分秒。
        六列压进三行 —— 分类 + 动作 + 结果 / 目标 + 详情 / 时间 + 来源。
      -->
      <template v-else-if="narrow">
        <div v-for="g in grouped" :key="g.day" class="al__group">
          <div class="al__day">{{ g.label }}</div>
          <div class="al__cards">
            <LbRowCard v-for="l in g.items" :key="l.id" :danger="!l.succeeded">
              <template #head>
                <span class="al__cat-tag">{{ categoryNames[categoryOf(l.action)] }}</span>
                <span class="al__card-name" :title="l.action">
                  {{ actionNames[l.action] ?? l.action }}
                </span>
                <LbStatusTag
                  :meta="
                    l.succeeded
                      ? { text: '成功', shape: 'check', fg: '#1B7A4B', bg: '#E9F5EE', bd: '#C3E3D0' }
                      : { text: '失败', shape: 'cross', fg: '#B4291D', bg: '#FDECEA', bd: '#F3CFC9' }
                  "
                />
              </template>

              <div class="al__detail lb-clamp-2">
                <template v-if="l.target_id">{{ targetOf(l) }} · </template>{{ l.detail || '—' }}
              </div>
              <div class="al__card-meta lb-mono">
                {{ l.created_at.slice(11, 19) }} · {{ l.client_ip || '系统任务' }}
              </div>
            </LbRowCard>
          </div>
        </div>

        <a-pagination
          v-if="visible.length > pager.pageSize.value"
          v-model:current="pager.current.value"
          :page-size="pager.pageSize.value"
          :total="visible.length"
          :show-size-changer="false"
          simple
          class="al__pager"
        />
      </template>

      <a-table
        v-else
        :columns="columns"
        :data-source="visible"
        :loading="loading"
        row-key="id"
        size="small"
        :pagination="pager.options.value"
        :scroll="{ x: 1050 }"
      >
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'time'">
            <LbTimeText :value="record.created_at" mode="both" />
          </template>

          <template v-else-if="column.key === 'action'">
            <div class="al__action">
              <span class="al__cat-tag">{{ categoryNames[categoryOf(record.action)] }}</span>
              <!-- 英文常量放进 title,需要时悬停可见。 -->
              <span :title="record.action">{{ actionNames[record.action] ?? record.action }}</span>
            </div>
          </template>

          <template v-else-if="column.key === 'target'">
            <span class="lb-ellipsis" :title="targetOf(record)">{{ targetOf(record) }}</span>
          </template>

          <template v-else-if="column.key === 'result'">
            <LbStatusTag
              :meta="
                record.succeeded
                  ? { text: '成功', shape: 'check', fg: '#1B7A4B', bg: '#E9F5EE', bd: '#C3E3D0' }
                  : { text: '失败', shape: 'cross', fg: '#B4291D', bg: '#FDECEA', bd: '#F3CFC9' }
              "
            />
          </template>

          <template v-else-if="column.key === 'ip'">
            <span class="lb-mono">{{ record.client_ip || '系统任务' }}</span>
            <div v-if="loginFailureBurst(record) > 2" class="al__burst">
              该 IP 近 1 小时第 {{ loginFailureBurst(record) }} 次
              <a @click="filters.keyword = record.client_ip">按此 IP 筛选</a>
            </div>
          </template>

          <template v-else-if="column.key === 'detail'">
            <div v-if="!record.detail" class="al__muted">—</div>
            <template v-else>
              <div :class="expanded.has(record.id) ? 'al__detail' : 'al__detail lb-clamp-2'">
                {{ record.detail }}
              </div>
              <a v-if="record.detail.length > 60" class="al__more" @click="toggleExpand(record.id)">
                {{ expanded.has(record.id) ? '收起' : '展开' }}
              </a>
            </template>
          </template>
        </template>
      </a-table>
    </a-card>
  </div>
</template>

<style scoped>
.al {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.al__head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
}

.al__title {
  margin: 0;
  font-size: 19px;
  font-weight: 600;
}

.al__sub {
  margin-top: 3px;
  font-size: 12.5px;
  color: #6b7480;
}

.al__cats {
  display: flex;
  gap: 4px;
  flex-wrap: wrap;
  padding: 10px 12px 0;
}

.al__cat {
  padding: 4px 10px;
  border: 1px solid #e3e6ea;
  border-radius: 4px;
  background: #fff;
  color: #576070;
  font-size: 12.5px;
  font-family: inherit;
  cursor: pointer;
}

.al__cat span {
  margin-left: 4px;
  font-size: 11px;
  color: #6b7480;
}

.al__cat--on {
  background: #eef4fc;
  border-color: #c9dcf3;
  color: #1d4f96;
  font-weight: 500;
}

.al__cat--on span {
  color: #4a7bbe;
}

.al__action {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 12.5px;
}

.al__cat-tag {
  flex: none;
  padding: 1px 5px;
  background: #f1f3f5;
  border-radius: 3px;
  font-size: 10.5px;
  color: #6b7480;
}

.al__burst {
  margin-top: 2px;
  font-size: 10.5px;
  color: #b4291d;
}

.al__burst a {
  margin-left: 4px;
}

.al__detail {
  font-size: 12px;
  line-height: 1.65;
  color: #576070;
}

.al__more {
  font-size: 11.5px;
}

.al__muted {
  color: #6b7480;
}

.al__group {
  padding: 0 12px;
}

.al__day {
  padding: 12px 2px 6px;
  font-size: 11.5px;
  font-weight: 600;
  color: #6b7480;
}

.al__cards {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding-bottom: 4px;
}

.al__card-name {
  flex: 1;
  min-width: 0;
  font-size: 13.5px;
  font-weight: 500;
}

.al__card-meta {
  font-size: 11px;
  color: #6b7480;
}

.al__pager {
  display: flex;
  justify-content: center;
  padding: 12px;
}
</style>
