<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AccessTier,
  type AdjustAction,
  type Node,
  type ProxyUser,
} from '@/api/client'
import { formatBytes } from '@/utils/format'
import UserDetailDrawer from '@/components/UserDetailDrawer.vue'
import UserFormModal from '@/components/user/UserFormModal.vue'
import UserAdjustModal from '@/components/user/UserAdjustModal.vue'
import {
  LbBatchBar,
  LbEmptyState,
  LbFilterBar,
  LbMetricCard,
  LbNameConfirm,
  LbQuotaBar,
  LbRowCard,
  LbStatusTag,
  LbTimeText,
  lbDangerConfirm,
} from '@/components/lb'
import { useNarrow } from '@/composables/useNarrow'
import {
  hasNoUsableNode,
  isExpiringSoon,
  isNearQuota,
  mustChangePassword,
  portalLoginOff,
  primaryUserAction,
  userActionLabel,
  daysUntil,
} from '@/components/lb/derive'
import { color, threshold } from '@/theme/tokens'

const narrow = useNarrow()
const users = ref<ProxyUser[]>([])
const nodes = ref<Node[]>([])
const tiers = ref<AccessTier[]>([])
const loading = ref(true)
/** 整表失败:表格保持空白,不显示「暂无数据」—— 那会被读成「一个用户都没有」。 */
const loadError = ref<{ message: string; status?: number; at: string } | null>(null)
const detailId = ref<number | null>(null)
/** 抽屉立即打开、标题先用列表里已有的这一份填上 —— 点了没反应比慢更难受。 */
const previewUser = computed(() => users.value.find((u) => u.id === detailId.value) ?? null)

// ---------- 筛选 ----------
// 筛选在前端做。用户是 10 人量级,推到 SQL 只会多出一层查询拼装代码。

const blankFilters = {
  keyword: '',
  tierID: undefined as number | undefined,
  status: undefined as string | undefined,
  login: undefined as 'yes' | 'off' | 'none' | undefined,
  expiringSoon: false,
  nearQuota: false,
}
const filters = reactive({ ...blankFilters })

const activeFilterCount = computed(
  () =>
    (filters.keyword.trim() ? 1 : 0) +
    (filters.tierID !== undefined ? 1 : 0) +
    (filters.status !== undefined ? 1 : 0) +
    (filters.login !== undefined ? 1 : 0) +
    (filters.expiringSoon ? 1 : 0) +
    (filters.nearQuota ? 1 : 0),
)

function clearFilters() {
  Object.assign(filters, blankFilters)
}

const visible = computed(() =>
  users.value.filter((u) => {
    const kw = filters.keyword.trim().toLowerCase()
    if (kw) {
      const hay = [u.display_name, u.user_code, u.remark, u.portal_account?.username ?? '']
        .join(' ')
        .toLowerCase()
      if (!hay.includes(kw)) return false
    }
    if (filters.tierID !== undefined && u.access_tier_id !== filters.tierID) return false
    if (filters.status !== undefined && u.status !== filters.status) return false
    if (filters.login === 'none' && u.portal_account) return false
    if (filters.login === 'yes' && !u.portal_account?.login_enabled) return false
    if (filters.login === 'off' && !portalLoginOff(u)) return false
    if (filters.expiringSoon && !isExpiringSoon(u)) return false
    if (filters.nearQuota && !isNearQuota(u)) return false
    return true
  }),
)

// ---------- 选择 ----------

const selected = ref<number[]>([])
// 切换筛选时清空选择:选中的对象可能已经不在视野里,
// 批量停用漏掉或多带上几个人,代价完全不对等。
watch(visible, () => {
  selected.value = selected.value.filter((id) => visible.value.some((u) => u.id === id))
})

const selectedUsers = computed(() =>
  users.value.filter((u) => selected.value.includes(u.id)),
)

const rowSelection = computed(() => ({
  selectedRowKeys: selected.value,
  onChange: (keys: (string | number)[]) => (selected.value = keys.map(Number)),
}))

// ---------- 指标 ----------

const stats = computed(() => ({
  active: users.value.filter((u) => u.status === 'ACTIVE').length,
  total: users.value.length,
  expiring: users.value.filter(isExpiringSoon).length,
  near: users.value.filter((u) => isNearQuota(u) || u.status === 'QUOTA_EXCEEDED').length,
  monthBytes: users.value.reduce((s, u) => s + u.used_total, 0),
}))

const metricState = computed(() =>
  loadError.value ? 'error' : loading.value ? 'loading' : users.value.length ? 'ready' : 'empty',
)

// ---------- 列 ----------

// 用户列左固定、操作列右固定:768–1279 中间那一档要横向滚动,
// 不固定的话滚动之后既看不出这是谁,也够不着操作按钮。
const columns = [
  { title: '用户', key: 'name', width: 220, fixed: 'left' as const },
  { title: '等级', key: 'tier', width: 100 },
  { title: '状态', key: 'status', width: 150 },
  { title: '节点', key: 'nodes', width: 110 },
  {
    title: '已用流量',
    key: 'used',
    width: 180,
    sorter: (a: ProxyUser, b: ProxyUser) => a.used_total - b.used_total,
  },
  {
    title: '到期时间',
    key: 'expires',
    width: 150,
    // 默认按到期时间升序 —— 管理员最常做的事是找快过期的人。
    defaultSortOrder: 'ascend' as const,
    sorter: (a: ProxyUser, b: ProxyUser) =>
      (daysUntil(a.expires_at) ?? Infinity) - (daysUntil(b.expires_at) ?? Infinity),
  },
  { title: '操作', key: 'actions', width: 200, fixed: 'right' as const },
]

// 10 人量级不分页。分页条常驻只会在一页三行的表下面挂一个空壳。
const pagination = computed(() =>
  users.value.length > threshold.paginateOver
    ? { pageSize: 25, showSizeChanger: false }
    : (false as const),
)

async function load() {
  loading.value = true
  loadError.value = null
  try {
    const [u, n, t] = await Promise.all([api.users(), api.nodes(), api.accessTiers()])
    users.value = u.items
    nodes.value = n.items
    tiers.value = t.items
  } catch (err) {
    // 不再只弹一条三秒消失的吐司,也不把数字回退成 0。
    // ApiError 上只有 status 与 message,没有请求 ID,所以这里也不编一个出来。
    loadError.value = {
      message: err instanceof ApiError ? err.message : '加载用户列表失败',
      status: err instanceof ApiError ? err.status : undefined,
      at: new Date().toLocaleTimeString(),
    }
    users.value = []
  } finally {
    loading.value = false
  }
}

// ---------- 行操作 ----------

const formOpen = ref(false)
const editing = ref<ProxyUser | null>(null)

const adjustOpen = ref(false)
const adjustUser = ref<ProxyUser | null>(null)
const adjustAction = ref<AdjustAction>('EXTEND_EXPIRY')
const adjustBatch = ref(false)

const deleting = ref<ProxyUser | null>(null)
const deleteOpen = ref(false)
const deleteLoading = ref(false)

function openCreate() {
  editing.value = null
  formOpen.value = true
}
function openEdit(u: ProxyUser) {
  editing.value = u
  formOpen.value = true
}
function openAdjust(u: ProxyUser | null, action: AdjustAction) {
  adjustUser.value = u
  adjustBatch.value = u === null
  adjustAction.value = action
  adjustOpen.value = true
}

function nodeCountOf(u: ProxyUser) {
  return u.effective_node_ids.length
}

/** 行主操作随状态变,只留一个。「⋯」里装其余的。 */
function runPrimary(u: ProxyUser) {
  switch (primaryUserAction(u)) {
    case 'renew':
      return openAdjust(u, 'EXTEND_EXPIRY')
    case 'addQuota':
      return openAdjust(u, 'ADD_QUOTA')
    case 'enable':
      return confirmToggle(u)
    case 'assignNode':
      return openEdit(u)
    default:
      detailId.value = u.id
  }
}

function confirmToggle(u: ProxyUser) {
  const enable = u.status === 'DISABLED'
  const n = nodeCountOf(u)

  if (enable) {
    // 恢复不是停用的镜像:只需确认「他现在这个状态恢复了能用吗」——
    // 一个到期日已过的用户被恢复后仍然连不上,把到期与额度摆出来能拦住白做的操作。
    lbDangerConfirm({
      title: `恢复用户 ${u.display_name}`,
      okType: 'primary',
      okText: '恢复',
      impacts: [
        `凭据会重新下发到 ${n} 个节点`,
        `到期时间 ${u.expires_at ? u.expires_at.slice(0, 10) : '不过期'}`,
        `流量已用 ${formatBytes(u.used_total)}`,
      ],
      onOk: () => toggle(u, true),
    })
    return
  }

  lbDangerConfirm({
    title: `停用用户 ${u.display_name}?`,
    okType: 'primary',
    okText: '停用',
    impacts: [
      `凭据在 ${n} 个节点重新部署后失效`,
      '门户仍可登录,但看不到订阅地址',
      '历史流量与调整记录保留',
      '随时可恢复,恢复后需再次部署',
    ],
    onOk: () => toggle(u, false),
  })
}

async function toggle(u: ProxyUser, enable: boolean) {
  try {
    await api.setUserEnabled(u.id, enable)
    message.success(enable ? '已恢复' : '已停用,受影响节点将重新部署')
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '操作失败')
  }
}

function confirmRegenerateToken(u: ProxyUser) {
  lbDangerConfirm({
    title: `重新生成 ${u.display_name} 的订阅地址?`,
    impacts: [
      '旧地址立即失效',
      '该用户全部客户端需重新导入',
      '节点侧凭据不变,无需部署',
    ],
    okText: '重新生成',
    onOk: async () => {
      try {
        await api.regenerateSubToken(u.id)
        message.success('订阅地址已重新生成,请通知用户重新导入')
        await load()
      } catch (err) {
        message.error(err instanceof ApiError ? err.message : '操作失败')
      }
    },
  })
}

function openDelete(u: ProxyUser) {
  deleting.value = u
  deleteOpen.value = true
}

async function doDelete() {
  if (!deleting.value) return
  deleteLoading.value = true
  try {
    await api.deleteUser(deleting.value.id)
    message.success('已删除')
    deleteOpen.value = false
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '删除失败')
  } finally {
    deleteLoading.value = false
  }
}

function confirmBatchDisable() {
  lbDangerConfirm({
    title: `停用 ${selected.value.length} 个用户?`,
    impacts: [
      '各自的凭据在受影响节点重新部署后失效',
      '逐个执行,失败不影响其余',
      '已成功的不会回滚 —— 批量操作不是事务',
    ],
    okText: '批量停用',
    onOk: () => openAdjust(null, 'DISABLE_USER'),
  })
}

onMounted(load)
</script>

<template>
  <div id="lb-main" class="uv">
    <div class="uv__head">
      <div>
        <h2 class="uv__title">用户管理</h2>
        <div class="uv__sub">
          {{ stats.total }} 个用户 · 流量周期边界为 UTC 00:00 · 等级不高于用户等级的节点自动继承
        </div>
      </div>
      <a-space>
        <a-button :loading="loading" @click="load">刷新</a-button>
        <a-button type="primary" @click="openCreate">新增用户</a-button>
      </a-space>
    </div>

    <a-alert
      v-if="nodes.length === 0 && !loading && !loadError"
      type="info"
      show-icon
      class="uv__hint"
      message="还没有节点"
      description="用户需要分配到节点才能使用。请先在节点管理中添加并部署节点。"
    />

    <div class="uv__metrics">
      <LbMetricCard label="有效用户" :state="metricState" :value="stats.active" :total="stats.total" />
      <LbMetricCard label="7 天内到期" :state="metricState" :value="stats.expiring" tone="warning">
        <template #action>
          <a v-if="stats.expiring" @click="filters.expiringSoon = true">筛选</a>
        </template>
      </LbMetricCard>
      <LbMetricCard label="流量超 80%" :state="metricState" :value="stats.near" tone="warning">
        <template #action>
          <a v-if="stats.near" @click="filters.nearQuota = true">筛选</a>
        </template>
      </LbMetricCard>
      <LbMetricCard label="累计已用流量" :state="metricState" :value="formatBytes(stats.monthBytes)" />
    </div>

    <a-card :body-style="{ padding: 0 }">
      <LbFilterBar
        :active-count="activeFilterCount"
        :filtered="visible.length"
        :total="users.length"
        @clear="clearFilters"
      >
        <a-input-search
          v-model:value="filters.keyword"
          placeholder="名称 / 编号 / 备注 / 登录账号"
          allow-clear
          style="width: 230px"
        />
        <a-select v-model:value="filters.status" placeholder="状态" allow-clear style="width: 130px">
          <a-select-option value="ACTIVE">正常</a-select-option>
          <a-select-option value="DISABLED">已停用</a-select-option>
          <a-select-option value="EXPIRED">已到期</a-select-option>
          <a-select-option value="QUOTA_EXCEEDED">流量用尽</a-select-option>
          <a-select-option value="DEPLOY_PENDING">待部署</a-select-option>
          <a-select-option value="DEPLOY_FAILED">部署失败</a-select-option>
        </a-select>
        <a-select v-model:value="filters.tierID" placeholder="访问等级" allow-clear style="width: 120px">
          <a-select-option v-for="t in tiers" :key="t.id" :value="t.id">{{ t.name }}</a-select-option>
        </a-select>
        <a-select v-model:value="filters.login" placeholder="门户登录" allow-clear style="width: 130px">
          <a-select-option value="yes">已开启</a-select-option>
          <!-- login_enabled=false 全站统称「门户登录已关闭」,不叫「已停用」 -->
          <a-select-option value="off">已关闭</a-select-option>
          <a-select-option value="none">未开通</a-select-option>
        </a-select>
        <a-checkbox v-model:checked="filters.expiringSoon">7 天内到期</a-checkbox>
        <a-checkbox v-model:checked="filters.nearQuota">流量超 80%</a-checkbox>
      </LbFilterBar>

      <LbBatchBar
        :selected-count="selected.length"
        :filtered-total="visible.length"
        :total="users.length"
        @clear="selected = []"
      >
        <a-button size="small" type="primary" @click="openAdjust(null, 'EXTEND_EXPIRY')">批量续期</a-button>
        <a-button size="small" @click="openAdjust(null, 'ADD_QUOTA')">加流量</a-button>
        <a-button size="small" danger @click="confirmBatchDisable">批量停用</a-button>
      </LbBatchBar>

      <LbEmptyState
        v-if="loadError"
        variant="error"
        :title="loadError.message"
        description="表格保持空白,不显示「暂无数据」—— 那会被读成一个用户都没有。"
        :http-status="loadError.status"
        :occurred-at="loadError.at"
        @retry="load"
      />
      <LbEmptyState
        v-else-if="!loading && users.length === 0"
        variant="empty"
        title="还没有用户"
        description="创建第一个用户后,他会自动继承访问等级覆盖的节点。"
      >
        <template #action>
          <a-button type="primary" size="small" @click="openCreate">新增用户</a-button>
        </template>
      </LbEmptyState>
      <LbEmptyState
        v-else-if="!loading && visible.length === 0"
        variant="filtered"
        title="没有符合条件的用户"
        :description="`当前有 ${activeFilterCount} 项筛选生效,${users.length} 个用户被筛掉。`"
        @clear="clearFilters"
      />

      <!--
        <768 整表换卡片:AntD Table 的横向滚动会把最右边的「操作」列推到屏幕外,
        手机上根本找不到它。按 P1→P2 顺序堆叠,P3(登录账号、最近续期)不出现在卡片上。
      -->
      <div v-else-if="narrow" class="uv__cards">
        <LbRowCard v-for="u in visible" :key="u.id">
          <template #head>
            <a class="uv__card-name" @click="detailId = u.id">{{ u.display_name }}</a>
            <LbStatusTag kind="user" :status="u.status" />
          </template>

          <div class="uv__card-sub lb-mono">
            {{ u.user_code }} · {{ u.access_tier_name }} ·
            <span v-if="hasNoUsableNode(u)" class="uv__nonode-inline">无可用节点</span>
            <span v-else>{{ nodeCountOf(u) }} 个节点</span>
          </div>
          <LbQuotaBar
            :used-bytes="u.used_total"
            :quota-bytes="u.quota_bytes"
            :warning-level="
              u.status === 'QUOTA_EXCEEDED'
                ? 'EXCEEDED'
                : isNearQuota(u)
                  ? 'WARNING'
                  : u.quota_bytes <= 0
                    ? 'UNLIMITED'
                    : 'NORMAL'
            "
          />
          <div class="uv__card-sub">
            <span v-if="!u.expires_at">不过期</span>
            <span v-else>
              {{
                (daysUntil(u.expires_at) ?? 0) < 0
                  ? `已过期 ${-(daysUntil(u.expires_at) ?? 0)} 天`
                  : `${daysUntil(u.expires_at)} 天后到期`
              }}
              · {{ u.expires_at.slice(0, 10) }}
            </span>
            <span v-if="mustChangePassword(u)" class="uv__flag--warn"> · 待改初始密码</span>
          </div>

          <template #foot>
            <a-button
              :type="primaryUserAction(u) === 'detail' ? 'default' : 'primary'"
              @click="runPrimary(u)"
            >
              {{ userActionLabel[primaryUserAction(u)] }}
            </a-button>
            <a-dropdown v-if="primaryUserAction(u) !== 'detail'" placement="topRight">
              <a-button class="lb-touch-target" :aria-label="`${u.display_name} 的更多操作`">⋯</a-button>
              <template #overlay>
                <a-menu>
                  <a-menu-item @click="detailId = u.id">详情</a-menu-item>
                  <a-menu-item @click="openEdit(u)">编辑</a-menu-item>
                  <a-menu-item @click="openAdjust(u, 'EXTEND_EXPIRY')">续期 / 调整</a-menu-item>
                  <a-menu-divider />
                  <a-menu-item danger @click="openDelete(u)">删除用户</a-menu-item>
                </a-menu>
              </template>
            </a-dropdown>
          </template>
        </LbRowCard>
      </div>

      <a-table
        v-else
        :columns="columns"
        :data-source="visible"
        :loading="loading"
        :row-selection="rowSelection"
        :pagination="pagination"
        row-key="id"
        size="small"
        :scroll="{ x: 1120 }"
      >
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'name'">
            <a @click="detailId = record.id">{{ record.display_name }}</a>
            <div class="uv__code lb-mono">
              {{ record.user_code }}
              <template v-if="record.portal_account">· {{ record.portal_account.username }}</template>
              <template v-else>· 未开通登录</template>
            </div>
          </template>

          <template v-else-if="column.key === 'tier'">
            <a-tag>{{ record.access_tier_name }}</a-tag>
          </template>

          <!-- 状态列两行:status 一行,与它正交的派生标记一行。不挤进同一个标签。 -->
          <template v-else-if="column.key === 'status'">
            <div class="uv__status">
              <LbStatusTag kind="user" :status="record.status" />
              <span v-if="mustChangePassword(record)" class="uv__flag uv__flag--warn">
                待改初始密码
              </span>
              <span v-else-if="portalLoginOff(record)" class="uv__flag">门户登录已关闭</span>
            </div>
          </template>

          <template v-else-if="column.key === 'nodes'">
            <span v-if="hasNoUsableNode(record)" class="uv__nonode">
              <span class="lb-mono">0</span>
              <span>无可用节点</span>
            </span>
            <span v-else class="lb-mono">
              {{ nodeCountOf(record) }}
              <span v-if="record.node_ids.length" class="uv__extra">
                (含追加 {{ record.node_ids.length }})
              </span>
            </span>
          </template>

          <template v-else-if="column.key === 'used'">
            <LbQuotaBar
              :used-bytes="record.used_total"
              :quota-bytes="record.quota_bytes"
              :warning-level="
                record.status === 'QUOTA_EXCEEDED'
                  ? 'EXCEEDED'
                  : isNearQuota(record)
                    ? 'WARNING'
                    : record.quota_bytes <= 0
                      ? 'UNLIMITED'
                      : 'NORMAL'
              "
            />
          </template>

          <template v-else-if="column.key === 'expires'">
            <span v-if="!record.expires_at" class="uv__muted">不过期</span>
            <span v-else class="uv__expiry">
              <span
                class="lb-mono"
                :style="{
                  color:
                    (daysUntil(record.expires_at) ?? 99) < 0
                      ? color.danger
                      : isExpiringSoon(record)
                        ? color.warning
                        : color.text2,
                }"
              >
                {{
                  (daysUntil(record.expires_at) ?? 0) < 0
                    ? `已过期 ${-(daysUntil(record.expires_at) ?? 0)} 天`
                    : `${daysUntil(record.expires_at)} 天后`
                }}
              </span>
              <LbTimeText :value="record.expires_at" mode="cycle" />
            </span>
          </template>

          <template v-else-if="column.key === 'actions'">
            <div class="uv__actions">
              <a-button
                size="small"
                :type="primaryUserAction(record) === 'detail' ? 'default' : 'primary'"
                @click="runPrimary(record)"
              >
                {{ userActionLabel[primaryUserAction(record)] }}
              </a-button>
              <a-button v-if="primaryUserAction(record) !== 'detail'" size="small" @click="detailId = record.id">
                详情
              </a-button>
              <a-dropdown placement="bottomRight">
                <a-button size="small" :aria-label="`${record.display_name} 的更多操作`" :title="`${record.display_name} 的更多操作`">⋯</a-button>
                <template #overlay>
                  <a-menu>
                    <a-menu-item @click="openEdit(record)">编辑</a-menu-item>
                    <a-menu-item @click="openAdjust(record, 'EXTEND_EXPIRY')">续期 / 调整</a-menu-item>
                    <a-menu-item @click="openAdjust(record, 'RESET_TRAFFIC')">重置流量</a-menu-item>
                    <a-menu-item @click="confirmRegenerateToken(record)">重新生成订阅地址</a-menu-item>
                    <a-menu-item @click="confirmToggle(record)">
                      {{ record.status === 'DISABLED' ? '恢复' : '停用' }}
                    </a-menu-item>
                    <!-- 危险项在菜单底部,分隔线之下 -->
                    <a-menu-divider />
                    <a-menu-item danger @click="openDelete(record)">删除用户</a-menu-item>
                  </a-menu>
                </template>
              </a-dropdown>
            </div>
          </template>
        </template>
      </a-table>
    </a-card>

    <UserFormModal
      v-model:open="formOpen"
      :user="editing"
      :tiers="tiers"
      :nodes="nodes"
      @saved="load"
    />

    <UserAdjustModal
      v-model:open="adjustOpen"
      :user="adjustBatch ? null : adjustUser"
      :targets="selectedUsers"
      :tiers="tiers"
      :initial-action="adjustAction"
      @done="
        () => {
          selected = []
          load()
        }
      "
    />

    <LbNameConfirm
      v-model:open="deleteOpen"
      :title="`删除用户 ${deleting?.display_name ?? ''}`"
      :name="deleting?.display_name ?? ''"
      :loading="deleteLoading"
      prompt="输入用户名称以确认"
      :impacts="[
        `UUID 在 ${deleting ? nodeCountOf(deleting) : 0} 个节点重新部署后失效`,
        '门户登录账号一并删除',
        '历史流量记录保留,用户本身无法恢复',
      ]"
      @confirm="doDelete"
    />

    <UserDetailDrawer
      :user-id="detailId"
      :nodes="nodes"
      :tiers="tiers"
      :preview="previewUser"
      @close="detailId = null"
      @changed="load"
      @edit="
        (u) => {
          detailId = null
          openEdit(u)
        }
      "
    />
  </div>
</template>

<style scoped>
.uv {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.uv__head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
}

.uv__title {
  margin: 0;
  font-size: 19px;
  font-weight: 600;
}

.uv__sub {
  margin-top: 3px;
  font-size: 12.5px;
  color: #6b7480;
}

.uv__metrics {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 16px;
}

.uv__cards {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 12px;
}

.uv__card-name {
  flex: 1;
  min-width: 0;
  font-size: 14px;
  font-weight: 600;
}

.uv__card-sub {
  font-size: 11.5px;
  color: #6b7480;
}

.uv__nonode-inline {
  color: #b4291d;
}

.uv__code {
  font-size: 11px;
  color: #6b7480;
}

.uv__status {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 3px;
}

.uv__flag {
  font-size: 11px;
  color: #6b7480;
}

.uv__flag--warn {
  color: #92610a;
}

.uv__nonode {
  display: flex;
  flex-direction: column;
  gap: 1px;
  color: #b4291d;
  font-size: 10.5px;
}

.uv__nonode .lb-mono {
  font-size: 12px;
}

.uv__extra,
.uv__muted {
  font-size: 11px;
  color: #6b7480;
}

.uv__expiry {
  display: flex;
  flex-direction: column;
  gap: 1px;
}

.uv__actions {
  display: flex;
  align-items: center;
  gap: 6px;
}

/* 768 以下整表换卡片。这里先保证操作列可达:
   AntD 的横向滚动会把最右边的操作列推到看不见的地方。 */
@media (max-width: 1279px) {
  .uv__metrics {
    grid-template-columns: repeat(2, 1fr);
  }
}
</style>
