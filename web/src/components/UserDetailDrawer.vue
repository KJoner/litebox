<script setup lang="ts">
import { ref, watch } from 'vue'
import { message, Modal } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AuditLog,
  type Node,
  type ProxyUser,
  type UserTraffic,
} from '@/api/client'
import { formatBytes, formatQuota, formatRelative, formatTime } from '@/utils/format'
import StatusTag from '@/components/StatusTag.vue'
import TrafficChart from '@/components/TrafficChart.vue'

const props = defineProps<{ userId: number | null; nodes: Node[] }>()
const emit = defineEmits<{ close: []; changed: [] }>()

const user = ref<ProxyUser | null>(null)
const traffic = ref<UserTraffic | null>(null)
const logs = ref<AuditLog[]>([])
const loading = ref(false)
const revealUUID = ref(false)

async function load(id: number) {
  loading.value = true
  revealUUID.value = false
  try {
    const u = await api.user(id)
    user.value = u
    const [t, l] = await Promise.all([
      api.userTraffic(id, 30),
      api.auditLogs({ targetType: 'user', targetId: u.user_code, limit: 20 }),
    ])
    traffic.value = t
    logs.value = l.items
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载用户详情失败')
  } finally {
    loading.value = false
  }
}

watch(
  () => props.userId,
  (id) => {
    if (id !== null) load(id)
    else {
      user.value = null
      traffic.value = null
      logs.value = []
    }
  },
  { immediate: true },
)

function nodeName(id: number): string {
  return props.nodes.find((n) => n.id === id)?.name ?? `节点 ${id}`
}

async function copy(text: string, label: string) {
  try {
    await navigator.clipboard.writeText(text)
    message.success(`${label}已复制`)
  } catch {
    message.warning('浏览器拒绝了剪贴板访问,请手动复制')
  }
}

async function act(fn: () => Promise<ProxyUser>, successText: string) {
  try {
    user.value = await fn()
    message.success(successText)
    emit('changed')
    if (props.userId !== null) await load(props.userId)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '操作失败')
  }
}

function confirmResetTraffic() {
  Modal.confirm({
    title: '重置已用流量?',
    content: '用户累计流量将清零,历史流水与节点计数器基线保持不变。若此前因超额被停用,将自动恢复。',
    okText: '重置',
    cancelText: '取消',
    onOk: () => act(() => api.resetUserTraffic(props.userId!), '流量已重置'),
  })
}

function confirmRegenerateUUID() {
  Modal.confirm({
    title: '重新生成 UUID?',
    content: '用户当前的客户端将在节点重新部署后立即失效,需要重新导入订阅。',
    okText: '重新生成',
    okType: 'danger',
    cancelText: '取消',
    onOk: () => act(() => api.regenerateUserUUID(props.userId!), 'UUID 已重新生成'),
  })
}

function confirmRegenerateToken() {
  Modal.confirm({
    title: '重新生成订阅地址?',
    content: '旧订阅地址立即失效,需要把新地址重新发给用户。',
    okText: '重新生成',
    okType: 'danger',
    cancelText: '取消',
    onOk: () => act(() => api.regenerateSubToken(props.userId!), '订阅地址已重新生成'),
  })
}
</script>

<template>
  <a-drawer
    :open="userId !== null"
    :title="user ? `${user.display_name} · ${user.user_code}` : '用户详情'"
    width="640"
    @close="emit('close')"
  >
    <a-spin :spinning="loading">
      <template v-if="user">
        <a-descriptions :column="2" size="small" bordered>
          <a-descriptions-item label="状态">
            <StatusTag :status="user.status" kind="user" />
          </a-descriptions-item>
          <a-descriptions-item label="备注">{{ user.remark || '—' }}</a-descriptions-item>
          <a-descriptions-item label="已用流量">
            {{ formatBytes(user.used_total) }} / {{ formatQuota(user.quota_bytes) }}
          </a-descriptions-item>
          <a-descriptions-item label="到期时间">
            {{ user.expires_at ? formatTime(user.expires_at) : '不过期' }}
          </a-descriptions-item>
          <a-descriptions-item label="上行 / 下行">
            {{ formatBytes(user.used_uplink) }} / {{ formatBytes(user.used_downlink) }}
          </a-descriptions-item>
          <a-descriptions-item label="流量重置">
            {{ user.reset_cycle === 'MONTHLY' ? `每月 ${user.reset_day} 日` : '不重置' }}
          </a-descriptions-item>
          <a-descriptions-item label="创建时间">{{ formatTime(user.created_at) }}</a-descriptions-item>
          <a-descriptions-item label="订阅拉取">
            <span v-if="user.sub_access_count === 0">从未拉取</span>
            <span v-else>
              {{ user.sub_access_count }} 次 · {{ formatRelative(user.sub_last_access_at) }}
            </span>
          </a-descriptions-item>
        </a-descriptions>

        <div class="section-title">节点分配</div>
        <a-space v-if="user.node_ids.length > 0" wrap>
          <a-tag v-for="id in user.node_ids" :key="id">{{ nodeName(id) }}</a-tag>
        </a-space>
        <a-empty v-else description="未分配节点" :image="undefined" />

        <div class="section-title">订阅地址</div>
        <a-input-group compact class="copy-row">
          <a-input :value="user.subscription_url" readonly style="width: calc(100% - 80px)" />
          <a-button @click="copy(user.subscription_url ?? '', '订阅地址')">复制</a-button>
        </a-input-group>
        <div class="sub-formats">
          <a :href="`${user.subscription_url}?format=uri`" target="_blank">查看 VLESS 链接</a>
          <a-divider type="vertical" />
          <a :href="`${user.subscription_url}?format=sing-box`" target="_blank">sing-box 配置</a>
        </div>

        <div class="section-title">
          UUID
          <a class="reveal" @click="revealUUID = !revealUUID">
            {{ revealUUID ? '隐藏' : '显示' }}
          </a>
        </div>
        <a-input-group compact class="copy-row">
          <a-input
            :value="revealUUID ? user.uuid : '••••••••-••••-••••-••••-••••••••••••'"
            readonly
            style="width: calc(100% - 80px)"
          />
          <a-button @click="copy(user.uuid ?? '', 'UUID')">复制</a-button>
        </a-input-group>

        <div class="section-title">最近 30 天流量</div>
        <TrafficChart :data="traffic?.daily ?? []" />

        <div v-if="traffic && traffic.by_node.length > 0" class="by-node">
          <div v-for="n in traffic.by_node" :key="n.node_id" class="by-node-row">
            <span class="by-node-name">{{ n.node_name }}</span>
            <span class="by-node-value">{{ formatBytes(n.total) }}</span>
          </div>
        </div>

        <div class="section-title">最近操作记录</div>
        <a-empty v-if="logs.length === 0" description="暂无记录" :image="undefined" />
        <a-timeline v-else class="log-timeline">
          <a-timeline-item
            v-for="l in logs"
            :key="l.id"
            :color="l.succeeded ? 'green' : 'red'"
          >
            <div class="log-action">{{ l.action }}</div>
            <div class="log-detail">{{ l.detail || '—' }}</div>
            <div class="log-time">{{ formatTime(l.created_at) }}</div>
          </a-timeline-item>
        </a-timeline>
      </template>
    </a-spin>

    <template #footer>
      <a-space>
        <a-button @click="confirmResetTraffic">重置流量</a-button>
        <a-button @click="confirmRegenerateUUID">重新生成 UUID</a-button>
        <a-button @click="confirmRegenerateToken">重新生成订阅地址</a-button>
      </a-space>
    </template>
  </a-drawer>
</template>

<style scoped>
.section-title {
  margin: 20px 0 8px;
  font-weight: 500;
  display: flex;
  align-items: center;
  gap: 8px;
}

.reveal {
  font-weight: 400;
  font-size: 12px;
}

.copy-row {
  display: flex;
}

.sub-formats {
  margin-top: 6px;
  font-size: 12px;
}

.by-node {
  margin-top: 12px;
}

.by-node-row {
  display: flex;
  justify-content: space-between;
  padding: 4px 0;
  border-bottom: 1px solid #f0f0f0;
  font-size: 13px;
}

.by-node-value {
  font-variant-numeric: tabular-nums;
}

.log-timeline {
  margin-top: 8px;
}

.log-action {
  font-size: 13px;
}

.log-detail {
  color: rgb(0 0 0 / 65%);
  font-size: 12px;
  word-break: break-all;
}

.log-time {
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
}
</style>
