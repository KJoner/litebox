<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  portalApi,
  ApiError,
  type PortalDashboard,
  type PublicAdjustment,
} from '@/api/client'
import { formatBytes, formatQuota } from '@/utils/format'
import { LbEmptyState, LbQuotaBar, LbStatusTag, LbTimeText } from '@/components/lb'

/**
 * 概览。这一页只回答一句话:我还能不能用。
 *
 * 后端已经给了 serviceable / reason / alerts[],原实现却把 reason 渲染成
 * 账号卡底部一行红字,夹在状态标签与流量卡之间 —— 正是用户视线扫过去的空档。
 * 这里把它升为顶部横幅的第一句,字段一个没动。
 */
const router = useRouter()

const data = ref<PortalDashboard | null>(null)
const adjustments = ref<PublicAdjustment[]>([])
const loading = ref(true)
const loadError = ref('')

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    data.value = await portalApi.dashboard()
  } catch (err) {
    // 门户的错误文案不给请求 ID 和状态码 —— 用户拿它做不了任何事。
    loadError.value = err instanceof ApiError ? err.message : '暂时读不到你的账号信息'
    data.value = null
  } finally {
    loading.value = false
  }
  // 调整记录是附加信息,取不到不该让整页空着。
  portalApi
    .adjustments()
    .then((r) => (adjustments.value = r.items))
    .catch(() => (adjustments.value = []))
}

onMounted(load)

const warningLevel = computed(() => {
  const d = data.value
  if (!d) return undefined
  if (d.quota_bytes <= 0) return 'UNLIMITED' as const
  const p = d.used_percent ?? 0
  if (p >= 100) return 'EXCEEDED' as const
  if (p >= 95) return 'DANGER' as const
  if (p >= 80) return 'WARNING' as const
  return 'NORMAL' as const
})

/** 顶部一句话结论。可用与不可用是完全不同的两种语气。 */
const verdict = computed(() => {
  const d = data.value
  if (!d) return null
  if (!d.serviceable) {
    return { ok: false, title: d.reason || d.status_text, extra: '' }
  }
  return {
    ok: true,
    title: `账号正常 · 订阅可用 · 可访问 ${d.node_count} 个节点`,
    extra: d.tier_name,
  }
})
</script>

<template>
  <div class="pd">
    <LbEmptyState v-if="loadError" variant="error" :title="loadError" @retry="load" />

    <div v-else-if="loading || !data" class="pd__skel">
      <a-skeleton active :paragraph="{ rows: 3 }" />
      <a-skeleton active :paragraph="{ rows: 3 }" />
    </div>

    <template v-else>
      <!-- reason 升为第一句,不再是夹在中间的一行小红字。 -->
      <div
        v-if="verdict"
        class="pd__verdict"
        :class="verdict.ok ? 'pd__verdict--ok' : 'pd__verdict--bad'"
      >
        <span class="pd__verdict-dot" />
        <span class="pd__verdict-text">{{ verdict.title }}</span>
        <span v-if="verdict.extra" class="pd__verdict-tier">{{ verdict.extra }}</span>
      </div>

      <div
        v-for="(a, i) in data.alerts"
        :key="i"
        class="pd__alert"
        :class="a.level === 'error' ? 'pd__alert--error' : 'pd__alert--warn'"
      >
        {{ a.message }}
      </div>

      <div class="pd__cards">
        <section class="pd__card">
          <div class="pd__card-head">
            <span>本月流量</span>
            <span v-if="data.next_reset_at" class="pd__card-note">
              <LbTimeText :value="data.next_reset_at" mode="cycle" /> 重置
            </span>
            <span v-else class="pd__card-note">不自动重置</span>
          </div>
          <div class="pd__card-body">
            <div class="pd__big">
              <span class="pd__big-num lb-mono">{{ formatBytes(data.used_total).split(' ')[0] }}</span>
              <span class="pd__big-unit">{{ formatBytes(data.used_total).split(' ')[1] }}</span>
              <span class="pd__big-total">/ {{ formatQuota(data.quota_bytes) }}</span>
              <span v-if="data.used_percent !== null" class="pd__big-pct lb-mono">
                {{ Math.round(data.used_percent) }}%
              </span>
            </div>
            <LbQuotaBar
              :used-bytes="data.used_total"
              :quota-bytes="data.quota_bytes"
              :warning-level="warningLevel"
              size="md"
              :show-value="false"
            />
            <div class="pd__facts">
              <div>
                <span>剩余</span>
                <!-- 不限量时不算剩余,也不显示一个假的百分比。 -->
                <b class="lb-mono">
                  {{ data.used_percent === null ? '不限量' : formatBytes(data.remaining) }}
                </b>
              </div>
              <div><span>上行</span><b class="lb-mono">{{ formatBytes(data.used_uplink) }}</b></div>
              <div><span>下行</span><b class="lb-mono">{{ formatBytes(data.used_downlink) }}</b></div>
            </div>
          </div>
        </section>

        <section class="pd__card">
          <div class="pd__card-head"><span>有效期</span></div>
          <div class="pd__card-body">
            <div class="pd__big">
              <span class="pd__big-num pd__big-num--sm lb-mono">
                {{ data.expires_at ? data.expires_at.slice(0, 10) : '不过期' }}
              </span>
            </div>
            <div class="pd__facts">
              <div>
                <span>剩余天数</span>
                <b class="lb-mono">
                  {{ data.remaining_days === null ? '不限' : `${data.remaining_days} 天` }}
                </b>
              </div>
              <div>
                <span>可用节点</span>
                <b class="lb-mono">{{ data.node_count }} 个</b>
              </div>
              <div>
                <span>最近重置</span>
                <b><LbTimeText :value="data.last_reset_at" empty="从未" /></b>
              </div>
            </div>
            <div class="pd__links">
              <a @click="router.push('/user/subscription')">我的订阅</a>
              <a @click="router.push('/user/nodes')">我的节点</a>
              <a @click="router.push('/user/traffic')">我的流量</a>
            </div>
          </div>
        </section>
      </div>

      <section v-if="adjustments.length" class="pd__card">
        <div class="pd__card-head"><span>最近调整</span></div>
        <div class="pd__adjs">
          <div v-for="(a, i) in adjustments" :key="i" class="pd__adj">
            <LbStatusTag
              :meta="{
                text: a.action_text,
                shape: 'dot',
                fg: '#2563B8',
                bg: '#EEF4FC',
                bd: '#C9DCF3',
              }"
            />
            <div class="pd__adj-body">
              <div v-if="a.quota_delta_bytes || a.expiry_delta_days" class="pd__adj-delta lb-mono">
                <template v-if="a.quota_delta_bytes">
                  {{ a.quota_delta_bytes > 0 ? '+' : '−'
                  }}{{ formatBytes(Math.abs(a.quota_delta_bytes)) }}
                </template>
                <template v-else>
                  {{ a.expiry_delta_days > 0 ? '+' : '' }}{{ a.expiry_delta_days }} 天
                </template>
              </div>
              <div v-if="a.remark" class="pd__adj-remark">{{ a.remark }}</div>
            </div>
            <LbTimeText :value="a.created_at" />
          </div>
        </div>
      </section>
    </template>
  </div>
</template>

<style scoped>
.pd {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.pd__skel {
  display: flex;
  flex-direction: column;
  gap: 20px;
}

.pd__verdict {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 12px 16px;
  border: 1px solid;
  border-radius: 8px;
  font-size: 13px;
}

.pd__verdict--ok {
  background: #e9f5ee;
  border-color: #c3e3d0;
  color: #14603b;
}

.pd__verdict--bad {
  background: #fdecea;
  border-color: #f3cfc9;
  color: #8e2117;
}

.pd__verdict-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: currentColor;
  flex: none;
}

.pd__verdict-text {
  flex: 1;
  min-width: 0;
  line-height: 1.7;
}

.pd__verdict-tier {
  flex: none;
  padding: 1px 8px;
  background: rgb(255 255 255 / 70%);
  border-radius: 3px;
  font-size: 11.5px;
  font-weight: 500;
}

.pd__alert {
  padding: 11px 14px;
  border: 1px solid;
  border-radius: 8px;
  font-size: 12.5px;
  line-height: 1.75;
}

.pd__alert--error {
  background: #fdecea;
  border-color: #f3cfc9;
  color: #8e2117;
}

.pd__alert--warn {
  background: #fcf3e3;
  border-color: #efdcb4;
  color: #5c4405;
}

.pd__cards {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 14px;
}

.pd__card {
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

.pd__card-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 16px;
  border-bottom: 1px solid #edeff2;
  font-size: 13px;
  font-weight: 600;
}

.pd__card-note {
  font-size: 11px;
  font-weight: 400;
  color: #6b7480;
}

.pd__card-body {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 16px;
}

.pd__big {
  display: flex;
  align-items: baseline;
  gap: 8px;
  flex-wrap: wrap;
}

.pd__big-num {
  font-size: 30px;
  font-weight: 600;
  letter-spacing: -0.02em;
  line-height: 1;
}

.pd__big-num--sm {
  font-size: 22px;
}

.pd__big-unit {
  font-size: 14px;
  color: #576070;
}

.pd__big-total,
.pd__big-pct {
  font-size: 12.5px;
  color: #6b7480;
}

.pd__facts {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 10px;
}

.pd__facts > div {
  display: flex;
  flex-direction: column;
  gap: 3px;
  min-width: 0;
}

.pd__facts span {
  font-size: 11.5px;
  color: #6b7480;
}

.pd__facts b {
  font-size: 12.5px;
  font-weight: 500;
}

.pd__links {
  display: flex;
  gap: 16px;
  padding-top: 10px;
  border-top: 1px solid #edeff2;
  font-size: 12.5px;
}

.pd__adjs {
  display: flex;
  flex-direction: column;
}

.pd__adj {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr) auto;
  align-items: center;
  gap: 12px;
  padding: 11px 16px;
}

.pd__adj + .pd__adj {
  border-top: 1px solid #edeff2;
}

.pd__adj-body {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.pd__adj-delta {
  font-size: 12.5px;
  color: #1b7a4b;
}

.pd__adj-remark {
  font-size: 11.5px;
  line-height: 1.6;
  color: #576070;
}

@media (max-width: 767px) {
  .pd__cards {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
