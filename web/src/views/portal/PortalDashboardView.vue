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
import { LbEmptyState, LbIcon, LbInfoTip, LbQuotaBar, LbStatusTag, LbTimeText } from '@/components/lb'

/**
 * 概览。这一页只回答一句话:我还能不能用。
 *
 * 后端已经给了 serviceable / reason / alerts[],原实现却把 reason 渲染成
 * 账号卡底部一行红字,夹在状态标签与流量卡之间 —— 正是用户视线扫过去的空档。
 * V18 把它做成 h1 一句话结论(「一切正常。」/「流量快用完了。」/ 不可用的原因),
 * 字段一个没动;下面是指标条(额度 / 到期 / 可用节点)、告警条与额度大卡。
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

/** 一句话结论。可用与不可用是完全不同的两种语气。 */
const headline = computed(() => {
  const d = data.value
  if (!d) return ''
  if (!d.serviceable) return d.reason || d.status_text || '账号当前不可用。'
  const p = d.used_percent ?? 0
  if (d.quota_bytes > 0 && p >= 95) return '流量快用完了。'
  if (d.remaining_days !== null && d.remaining_days <= 7) return `${d.remaining_days} 天后到期。`
  if (d.quota_bytes > 0 && p >= 80) return '流量用了八成。'
  return '一切正常。'
})

const summaryLine = computed(() => {
  const d = data.value
  if (!d) return ''
  const parts: string[] = []
  parts.push(d.serviceable ? '订阅可用' : '订阅暂不可用')
  parts.push(`可访问 ${d.node_count} 个节点`)
  if (d.tier_name) parts.push(d.tier_name)
  return parts.join(' · ') + '。'
})

const usedParts = computed(() => {
  const s = formatBytes(data.value?.used_total ?? 0).split(' ')
  return { num: s[0], unit: s[1] ?? '' }
})
</script>

<template>
  <div class="lb-page pd">
    <div v-if="loadError" class="lb-card lb-card--flush">
      <LbEmptyState variant="error" :title="loadError" @retry="load" />
    </div>

    <div v-else-if="loading || !data" class="pd__skel">
      <a-skeleton active :paragraph="{ rows: 3 }" />
      <a-skeleton active :paragraph="{ rows: 3 }" />
    </div>

    <template v-else>
      <!-- reason 升为第一句,不再是夹在中间的一行小红字。 -->
      <div class="lb-page__head">
        <div class="lb-page__title-wrap">
          <h1 class="lb-page__title" :class="{ 'pd__title--bad': !data.serviceable }">
            <span>{{ headline }}</span>
            <LbInfoTip
              :width="280"
              text="流量与到期按 UTC 计。额度用满或到期后,客户端会连不上;续期或加量由管理员操作。"
            />
          </h1>
          <div class="lb-page__summary">{{ summaryLine }}</div>
        </div>
      </div>

      <div v-if="data.alerts.length" class="pd__alerts">
        <div v-for="(a, i) in data.alerts" :key="i" class="lb-notice pd__alert">
          <span class="lb-notice__icon" :class="a.level === 'error' ? 'lb-notice__icon--bad' : 'lb-notice__icon--warn'">
            <LbIcon name="alert-triangle" :size="18" />
          </span>
          <div class="lb-notice__body">
            <div class="lb-notice__text pd__alert-text">{{ a.message }}</div>
          </div>
        </div>
      </div>

      <!-- 指标条:额度 / 到期 / 可用节点。 -->
      <section class="lb-metrics">
        <div class="lb-metric pd__metric">
          <div class="pd__metric-label">
            本月流量
            <span v-if="data.next_reset_at" class="pd__metric-note">
              <LbTimeText :value="data.next_reset_at" mode="cycle" /> 重置
            </span>
            <span v-else class="pd__metric-note">不自动重置</span>
          </div>
          <div class="pd__metric-row">
            <span class="pd__metric-value lb-tabular" :class="{ 'pd__metric-value--warn': warningLevel === 'WARNING', 'pd__metric-value--bad': warningLevel === 'DANGER' || warningLevel === 'EXCEEDED' }">
              {{ usedParts.num }}
            </span>
            <span class="pd__metric-unit">{{ usedParts.unit }}</span>
            <span class="pd__metric-unit">/ {{ formatQuota(data.quota_bytes) }}</span>
          </div>
          <div class="pd__metric-foot">
            <template v-if="data.used_percent === null">不限量</template>
            <template v-else>剩余 {{ formatBytes(data.remaining) }} · 已用 {{ Math.round(data.used_percent) }}%</template>
          </div>
        </div>
        <div class="lb-metric pd__metric">
          <div class="pd__metric-label">有效期</div>
          <div class="pd__metric-row">
            <span class="pd__metric-value pd__metric-value--sm lb-tabular">
              {{ data.remaining_days === null ? '不限' : data.remaining_days }}
            </span>
            <span v-if="data.remaining_days !== null" class="pd__metric-unit">天</span>
          </div>
          <div class="pd__metric-foot">
            {{ data.expires_at ? `到 ${data.expires_at.slice(0, 10)}` : '不过期' }}
          </div>
        </div>
        <div class="lb-metric pd__metric">
          <div class="pd__metric-label">可用节点</div>
          <div class="pd__metric-row">
            <span class="pd__metric-value lb-tabular">{{ data.node_count }}</span>
            <span class="pd__metric-unit">个</span>
          </div>
          <div class="pd__metric-foot">
            <a class="pd__link" @click="router.push('/user/nodes')">查看节点 ›</a>
          </div>
        </div>
      </section>

      <!-- 额度大卡:8px 进度条 + 三项事实 + 三条链接。 -->
      <section class="lb-card pd__quota">
        <div class="pd__quota-head">
          <span class="lb-card__title pd__quota-title">流量与有效期</span>
          <LbStatusTag :meta="{ text: data.status_text, shape: data.serviceable ? 'dot' : 'cross', fg: data.serviceable ? 'var(--ok)' : 'var(--bad)', bg: data.serviceable ? 'var(--ok-bg)' : 'var(--bad-bg)' }" />
        </div>
        <LbQuotaBar
          :used-bytes="data.used_total"
          :quota-bytes="data.quota_bytes"
          :warning-level="warningLevel"
          size="md"
        />
        <div class="lb-kv lb-kv--4 pd__facts">
          <div class="lb-kv__item">
            <span class="lb-kv__k">剩余</span>
            <!-- 不限量时不算剩余,也不显示一个假的百分比。 -->
            <b class="lb-kv__v">{{ data.used_percent === null ? '不限量' : formatBytes(data.remaining) }}</b>
          </div>
          <div class="lb-kv__item"><span class="lb-kv__k">上行</span><b class="lb-kv__v">{{ formatBytes(data.used_uplink) }}</b></div>
          <div class="lb-kv__item"><span class="lb-kv__k">下行</span><b class="lb-kv__v">{{ formatBytes(data.used_downlink) }}</b></div>
          <div class="lb-kv__item">
            <span class="lb-kv__k">最近重置</span>
            <b class="lb-kv__v"><LbTimeText :value="data.last_reset_at" empty="从未" /></b>
          </div>
        </div>
        <div class="pd__links">
          <a-button size="small" class="lb-btn-ghost" @click="router.push('/user/subscription')">我的订阅</a-button>
          <a-button size="small" class="lb-btn-ghost" @click="router.push('/user/nodes')">我的节点</a-button>
          <a-button size="small" class="lb-btn-ghost" @click="router.push('/user/traffic')">我的流量</a-button>
        </div>
      </section>

      <section v-if="adjustments.length" class="lb-card lb-card--flush">
        <div class="pd__adjs-head">最近调整</div>
        <div class="pd__adjs">
          <div v-for="(a, i) in adjustments" :key="i" class="pd__adj">
            <LbStatusTag
              :meta="{
                text: a.action_text,
                shape: 'dot',
                fg: 'var(--brand)',
                bg: 'var(--brand-bg)',
              }"
            />
            <div class="pd__adj-body">
              <div v-if="a.quota_delta_bytes || a.expiry_delta_days" class="pd__adj-delta lb-tabular">
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
  gap: 20px;
}

.pd__skel {
  display: flex;
  flex-direction: column;
  gap: 20px;
}

.pd__title--bad {
  color: var(--bad);
}

.pd__alerts {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.pd__alert {
  align-items: center;
}
.pd__alert-text {
  margin-top: 0;
  color: var(--text);
}

.pd__metric {
  gap: 12px;
  padding: 22px 24px;
}
.pd__metric-label {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  font-size: 13px;
  font-weight: 500;
  color: var(--text3);
}
.pd__metric-note {
  font-size: 11.5px;
  font-weight: 400;
}
.pd__metric-note :deep(.lb-time) {
  font-size: 11.5px;
}
.pd__metric-row {
  display: flex;
  align-items: baseline;
  gap: 6px;
  flex-wrap: wrap;
}
.pd__metric-value {
  font-size: 36px;
  font-weight: 700;
  letter-spacing: -0.04em;
  line-height: 1;
}
.pd__metric-value--sm {
  font-size: 30px;
}
.pd__metric-value--warn {
  color: var(--warn);
}
.pd__metric-value--bad {
  color: var(--bad);
}
.pd__metric-unit {
  font-size: 14px;
  color: var(--text3);
}
.pd__metric-foot {
  font-size: 12.5px;
  color: var(--text2);
}
.pd__link {
  font-weight: 500;
}

.pd__quota {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.pd__quota-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}
.pd__quota-title {
  margin: 0;
}
.pd__facts {
  gap: 12px;
}
.pd__links {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}

.pd__adjs-head {
  padding: 18px 22px 0;
  font-size: 15px;
  font-weight: 600;
}
.pd__adjs {
  display: flex;
  flex-direction: column;
  padding-top: 8px;
}

.pd__adj {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr) auto;
  align-items: center;
  gap: 12px;
  padding: 12px 22px;
}

.pd__adj + .pd__adj {
  border-top: 1px solid var(--sep2);
}

.pd__adj-body {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.pd__adj-delta {
  font-size: 13px;
  font-weight: 500;
  color: var(--ok);
}

.pd__adj-remark {
  font-size: 12.5px;
  line-height: 1.6;
  color: var(--text2);
}

@media (max-width: 767px) {
  .pd__metric {
    padding: 16px 18px;
  }
  .pd__metric-value {
    font-size: 30px;
  }
}
</style>
