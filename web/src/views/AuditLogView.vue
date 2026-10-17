<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type AuditLog, type Node, type ProxyUser } from '@/api/client'
import { LbEmptyState, LbFilterBar, LbRowCard, LbStatusTag, LbTimeText,
  LbInfoTip,
  LbSectionTitle,
  LbIcon,
} from '@/components/lb'
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
  // 以下是 V7 之后陆续加的动作。缺了中文名的话,这一列直接显示 external_proxy.update
  // 这样的内部标识 —— 能看懂,但与上面那些对不齐。
  'user.regenerate_ss_password': '重新生成 SS 密钥',
  'user.regenerate_snell_key': '重新生成 Snell 密钥',
  'node.addresses': '修改订阅地址池',
  'node.recheck': '全面重检',
  'node.config_in_ram': '切换配置存放位置',
  'node.chain_apply': '启用链式出口',
  'node.chain_clear': '解除链式出口',
  'node.tcp_tune': 'TCP 调优',
  'node.tcp_restore': '还原 TCP 参数',
  'node.vnstat_install': '安装 vnStat',
  'node.singbox_start': '启动 sing-box',
  'node.singbox_stop': '停止 sing-box',
  'node.singbox_uninstall': '卸载 sing-box',
  'node.nginx_install': '安装 nginx',
  'node.nginx_start': '启动 nginx',
  'node.nginx_stop': '停止 nginx',
  'node.nginx_restart': '重启 nginx',
  'node.nginx_uninstall': '卸载 nginx',
  'node.realm_install': '安装 realm',
  'node.realm_stop': '停止 realm',
  'node.realm_restart': '重启 realm',
  'node.realm_uninstall': '卸载 realm',
  'node.mieru_start': '启动 Mieru',
  'node.mieru_stop': '停止 Mieru',
  'node.mieru_restart': '重启 Mieru',
  'node.mieru_uninstall': '卸载 Mieru',
  'node.cloud_bind': '绑定云实例',
  'node.cloud_unbind': '解绑云实例',
  'node.cloud_start': '云实例开机',
  'node.cloud_stop': '云实例停机',
  'inbound.create': '新增入口',
  'inbound.update': '编辑入口',
  'inbound.delete': '删除入口',
  'inbound.dest_check': '实测握手目标',
  'inbound.endpoints': '修改入口地址条目',
  'mieru_inbound.create': '新增 Mieru 入口',
  'mieru_inbound.update': '编辑 Mieru 入口',
  'mieru_inbound.delete': '删除 Mieru 入口',
  'mieru_inbound.deploy': '下发 Mieru 入口',
  'mieru_inbound.install': '安装 mita',
  'mieru_inbound.chain': '设置 Mieru 出口',
  'relay.create': '新增转发',
  'relay.update': '编辑转发',
  'relay.delete': '删除转发',
  'relay.deploy': '下发 nginx 转发',
  'realm.deploy': '下发 realm 转发',
  'external_proxy.create': '新增外部代理',
  'external_proxy.update': '编辑外部代理',
  'external_proxy.delete': '删除外部代理',
  'external_proxy.detach': '转为手工条目',
  'external_proxy.replace_endpoint': '替换外部代理地址',
  'external_proxy.set_locked_fields': '锁定字段',
  'external_proxy.set_status': '修改外部代理状态',
  'external_proxy.set_subscription': '外部代理订阅开关',
  'external_proxy.view_credentials': '查看外部代理凭据',
  'proxy_source.create': '新增代理源',
  'proxy_source.update': '编辑代理源',
  'proxy_source.delete': '删除代理源',
  'proxy_source.import': '导入代理源',
  'proxy_source.sync': '同步代理源',
  'proxy_source.view_url': '查看代理源地址',
  'subscription.order_migrate': '迁移订阅排序',
  'subscription_profile.create': '新增订阅配置',
  'subscription_profile.update': '编辑订阅配置',
  'subscription_profile.delete': '删除订阅配置',
  'subscription_profile.set_enabled': '订阅配置开关',
  'expiry.update': '修改到期档案',
  'expiry.renew': '登记续费',
  'expiry.clear': '清除到期档案',
  'cloud_account.create': '新增云账号',
  'cloud_account.update': '编辑云账号',
  'cloud_account.delete': '删除云账号',
}

type Category =
  | 'user' | 'node' | 'deploy' | 'external' | 'subscription' | 'expiry'
  | 'login' | 'traffic' | 'settings' | 'other'

const categoryNames: Record<Category, string> = {
  user: '用户',
  node: '节点',
  deploy: '部署',
  external: '外部代理',
  subscription: '订阅',
  expiry: '到期续费',
  login: '登录',
  traffic: '流量',
  settings: '设置',
  other: '其他',
}

/**
 * 分类来自动作名前缀,三种「下发」单独提出来归「部署」。
 *
 * portal.* 归到「用户」而不是「登录」:它们是管理员对某个用户的门户账号做的操作,
 * 与 admin.login 那种「谁在登录面板」不是一回事。
 *
 * **认不出的前缀归「其他」,不归「登录」。** 原来的兜底是「登录」,于是入口、转发、
 * 外部代理、订阅、云账号、到期续费这十几类动作全被算成登录 —— 管理员点「登录」
 * 想看谁进过面板,看到的是一屏 external_proxy.update。
 */
function categoryOf(action: string): Category {
  if (
    action === 'node.deploy' ||
    action === 'relay.deploy' ||
    action === 'realm.deploy' ||
    action === 'mieru_inbound.deploy'
  ) {
    return 'deploy'
  }
  if (action.startsWith('user.') || action.startsWith('portal.')) return 'user'
  if (
    action.startsWith('node.') ||
    action.startsWith('inbound.') ||
    action.startsWith('mieru_inbound.') ||
    action.startsWith('relay.')
  ) {
    return 'node'
  }
  if (action.startsWith('external_proxy.') || action.startsWith('proxy_source.')) return 'external'
  if (action.startsWith('subscription')) return 'subscription'
  if (action.startsWith('expiry.')) return 'expiry'
  if (action.startsWith('admin.')) return 'login'
  if (action.startsWith('traffic.')) return 'traffic'
  if (
    action.startsWith('settings.') ||
    action.startsWith('tier.') ||
    action.startsWith('cloud_account.') ||
    action.startsWith('notify.')
  ) {
    return 'settings'
  }
  return 'other'
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
  <div class="lb-page al">
    <div class="lb-page__head">
      <div class="lb-page__title-wrap">
        <h1 class="lb-page__title">
          <span>审计日志</span>
          <LbInfoTip text="最近 200 条,记录所有写操作与登录尝试。详情里不含 token、密码与 UUID 全值。" :width="300" />
        </h1>
        <div class="lb-page__summary">{{ logs.length }} 条记录,{{ logs.filter((l) => !l.succeeded).length }} 条失败。</div>
      </div>
      <div class="lb-page__actions">
        <a-button :loading="loading" aria-label="刷新" title="刷新" @click="load">
          <template #icon><LbIcon name="refresh" :size="15" /></template>
        </a-button>
        <a-button :disabled="visible.length === 0" @click="exportCSV">
          <template #icon><LbIcon name="download" :size="15" /></template>
          导出 CSV
        </a-button>
      </div>
    </div>

    <section>
    <LbSectionTitle title="全部记录" :count="`${visible.length} / ${logs.length} 条`" />
    <div class="lb-card lb-card--flush">
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

      <LbFilterBar :active-count="activeFilterCount" @clear="clearFilters">
        <a-input v-model:value="filters.keyword" placeholder="搜索操作、目标、详情或 IP" allow-clear>
          <template #prefix><LbIcon name="search" :size="14" /></template>
        </a-input>
        <a-select v-model:value="filters.days" placeholder="时间范围" allow-clear style="width: 130px">
          <a-select-option :value="1">近 24 小时</a-select-option>
          <a-select-option :value="7">近 7 天</a-select-option>
          <a-select-option :value="30">近 30 天</a-select-option>
        </a-select>
        <label class="lb-filter__toggle" :class="{ 'lb-filter__toggle--on': filters.onlyFailed }">
          <a-switch v-model:checked="filters.onlyFailed" size="small" />
          只看失败
        </label>
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
                      ? { text: '成功', shape: 'check', fg: 'var(--ok)', bg: 'var(--ok-bg)' }
                      : { text: '失败', shape: 'cross', fg: 'var(--bad)', bg: 'var(--bad-bg)' }
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
                  ? { text: '成功', shape: 'check', fg: 'var(--ok)', bg: 'var(--ok-bg)' }
                  : { text: '失败', shape: 'cross', fg: 'var(--bad)', bg: 'var(--bad-bg)' }
              "
            />
          </template>

          <template v-else-if="column.key === 'ip'">
            <span class="lb-mono">{{ record.client_ip || '系统任务' }}</span>
            <div v-if="loginFailureBurst(record) > 2" class="al__burst">
              该 IP 近 1 小时第 {{ loginFailureBurst(record) }} 次
              <button type="button" class="lb-linkbtn" @click="filters.keyword = record.client_ip">按此 IP 筛选</button>
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
    </div>
    </section>
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
  font-size: var(--fs-xl);
  font-weight: 600;
}

.al__sub {
  margin-top: 3px;
  font-size: var(--fs-sm);
  color: var(--text3);
}

.al__cats {
  display: flex;
  gap: 4px;
  flex-wrap: wrap;
  padding: 10px 12px 0;
}

.al__cat {
  padding: 4px 10px;
  border: 1px solid var(--sep);
  border-radius: var(--r-input);
  background: var(--surface);
  color: var(--text2);
  font-size: var(--fs-sm);
  font-family: inherit;
  cursor: pointer;
}

.al__cat span {
  margin-left: 4px;
  font-size: var(--fs-2xs);
  color: var(--text3);
}

.al__cat--on {
  background: var(--brand-bg);
  border-color: var(--brand-bg);
  color: var(--brand-hover);
  font-weight: 500;
}

.al__cat--on span {
  color: var(--brand);
}

.al__action {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: var(--fs-sm);
}

.al__cat-tag {
  flex: none;
  padding: 1px 5px;
  background: var(--fill);
  border-radius: var(--r-sm);
  font-size: var(--fs-2xs);
  color: var(--text3);
}

.al__burst {
  margin-top: 2px;
  font-size: var(--fs-2xs);
  color: var(--bad);
}

.al__burst .lb-linkbtn {
  margin-left: 4px;
}

.al__detail {
  font-size: var(--fs-xs);
  line-height: 1.65;
  color: var(--text2);
}

.al__more {
  font-size: var(--fs-xs);
}

.al__muted {
  color: var(--text3);
}

.al__group {
  padding: 0 12px;
}

.al__day {
  padding: 12px 2px 6px;
  font-size: var(--fs-xs);
  font-weight: 600;
  color: var(--text3);
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
  font-size: var(--fs-base);
  font-weight: 500;
}

.al__card-meta {
  font-size: var(--fs-2xs);
  color: var(--text3);
}

.al__pager {
  display: flex;
  justify-content: center;
  padding: 12px;
}
</style>
