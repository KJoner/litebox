<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { message, Modal } from 'ant-design-vue'
import { portalApi, ApiError, type PortalSubscription } from '@/api/client'
import { formatRelative } from '@/utils/format'

const data = ref<PortalSubscription | null>(null)
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    data.value = await portalApi.subscription()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载失败')
  } finally {
    loading.value = false
  }
}

async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    message.success('已复制')
  } catch {
    // 非 HTTPS 或浏览器不支持时剪贴板 API 不可用。不能只说"复制失败"就完 ——
    // 用户还是需要拿到这段地址,把它选中给他。
    message.warning('浏览器不允许自动复制,请手动选中地址复制')
  }
}

function regenerate() {
  Modal.confirm({
    title: '重新生成订阅地址',
    content: '重新生成后,旧订阅地址将立即失效,所有设备都需要重新导入。',
    okText: '确认重新生成',
    okType: 'danger',
    cancelText: '取消',
    onOk: async () => {
      try {
        data.value = await portalApi.regenerateSubscription()
        message.success('订阅地址已更新,请在所有设备重新导入')
      } catch (err) {
        message.error(err instanceof ApiError ? err.message : '重新生成失败')
      }
    },
  })
}

onMounted(load)
</script>

<template>
  <a-spin :spinning="loading">
    <template v-if="data">
      <a-alert
        v-if="!data.available"
        type="error"
        show-icon
        class="card"
        message="订阅当前不可用"
        :description="data.reason"
      />

      <a-card v-else title="订阅地址" class="card">
        <template #extra>
          <a-button danger size="small" @click="regenerate">重新生成</a-button>
        </template>

        <div v-for="item in [
          { label: '通用订阅（Base64）', url: data.url_base64, tip: 'v2rayN、Shadowrocket 等客户端' },
          { label: 'sing-box 配置', url: data.url_singbox, tip: 'sing-box 客户端直接导入' },
          { label: '明文 VLESS 链接', url: data.url_uri, tip: '用于人工核对' },
        ]" :key="item.label" class="sub-item">
          <div class="sub-label">
            {{ item.label }}
            <span class="sub-tip">{{ item.tip }}</span>
          </div>
          <a-input-group compact class="sub-row">
            <a-input :value="item.url" readonly class="sub-url" />
            <a-button type="primary" @click="copy(item.url)">复制</a-button>
          </a-input-group>
        </div>

        <a-descriptions :column="{ xs: 1, sm: 3 }" size="small" class="meta">
          <!-- 两个数字都给出来:配了 IPv6 的节点在订阅里是两条,
               只报节点数的话用户导入后会觉得"多出来几个"。 -->
          <a-descriptions-item label="包含节点">
            {{ data.node_count }} 个
            <span v-if="data.ipv6_count > 0" class="entry-hint">
              (订阅共 {{ data.entry_count }} 条,其中 {{ data.ipv6_count }} 个节点额外提供 IPv6)
            </span>
          </a-descriptions-item>
          <a-descriptions-item label="最近拉取">
            {{ formatRelative(data.last_access_at) }}
          </a-descriptions-item>
          <a-descriptions-item label="累计拉取">{{ data.access_count }} 次</a-descriptions-item>
        </a-descriptions>
      </a-card>

      <a-card title="使用说明" size="small">
        <ol class="tips">
          <li>复制上方对应客户端的订阅地址,在客户端里新增订阅并粘贴。</li>
          <li>订阅地址等同于你的凭据,不要转发给他人。</li>
          <li>怀疑泄露时点「重新生成」,旧地址会立即失效。</li>
          <li>节点变动后客户端需要手动更新一次订阅才能看到。</li>
        </ol>
      </a-card>
    </template>
  </a-spin>
</template>

<style scoped>
.card {
  margin-bottom: 16px;
}

.sub-item {
  margin-bottom: 16px;
}

.sub-label {
  margin-bottom: 6px;
  font-weight: 500;
}

.sub-tip {
  margin-left: 8px;
  font-weight: 400;
  font-size: 12px;
  color: rgb(0 0 0 / 45%);
}

.entry-hint {
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
}

.sub-row {
  display: flex;
}

.sub-url {
  flex: 1;
  min-width: 0;
}

.meta {
  margin-top: 8px;
}

.tips {
  margin: 0;
  padding-left: 20px;
  color: rgb(0 0 0 / 65%);
  line-height: 1.9;
}
</style>
