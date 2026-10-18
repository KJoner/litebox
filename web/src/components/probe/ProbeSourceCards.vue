<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { message, Modal } from 'ant-design-vue'
import { api, ApiError, type ProbeOrigin, type ProbeSource } from '@/api/client'
import { LbIcon, LbInfoTip, LbStatusTag, LbTimeText, metaOf } from '@/components/lb'

/**
 * 三个来源:自定义 / Zstatic / TcpQuality。第三方来源要管理员先阅读使用说明并确认授权,
 * 它的目标才能对节点生效;确认只是面板上的声明,记录谁在什么时候确认过,不能替代来源方的实际授权。
 * 暂停一个来源对所有节点生效;最小间隔与单目标限流是来源级的。
 */
const props = defineProps<{ sources: ProbeSource[] }>()
const emit = defineEmits<{
  (e: 'changed'): void
  (e: 'import', kind: ProbeOrigin): void
}>()

const busy = ref<string | null>(null)

function stateMeta(s: ProbeSource) {
  if (s.paused) return metaOf.paused('已暂停')
  if (!s.authorized) return metaOf.warn('未确认授权', 'triangle')
  if (!s.third_party) return metaOf.ok('自有 / 已授权')
  return metaOf.ok('已确认授权')
}

async function togglePause(s: ProbeSource) {
  const run = async () => {
    busy.value = s.kind
    try {
      await api.updateProbeSource(s.kind, { paused: !s.paused })
      message.success(s.paused ? `已恢复「${s.name}」` : `已暂停「${s.name}」:它的目标对所有节点停测,历史保留`)
      emit('changed')
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '操作失败')
    } finally {
      busy.value = null
    }
  }
  if (s.paused) return run()
  Modal.confirm({
    title: `暂停来源「${s.name}」?`,
    content: `它的 ${s.target_count} 个目标对所有节点停测(${s.active_count} 个机器 × 目标组合会停下),历史与节点开关都保留,恢复之后接着测。`,
    okText: '暂停',
    cancelText: '取消',
    onOk: () => {
      void run()
    },
  })
}

// ---------- 说明与授权 ----------

const authOpen = ref(false)
const authSource = ref<ProbeSource | null>(null)
const authRead = ref(false)
const authNote = ref('')
const authSaving = ref(false)

function openAuth(s: ProbeSource) {
  authSource.value = s
  authRead.value = false
  authNote.value = s.authorized_note
  authOpen.value = true
}

async function confirmAuth(authorize: boolean) {
  const s = authSource.value
  if (!s) return
  if (authorize && !authRead.value) {
    message.warning('先勾选「已阅读并确认具备使用授权」')
    return
  }
  authSaving.value = true
  try {
    await api.updateProbeSource(s.kind, authorize ? { authorize: true, authorized_note: authNote.value.trim() } : { authorize: false })
    message.success(authorize ? `已记录对「${s.name}」的授权确认` : `已撤销「${s.name}」的授权确认,它的目标随即停测`)
    authOpen.value = false
    emit('changed')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    authSaving.value = false
  }
}

// ---------- 来源限额 ----------

const limitOpen = ref(false)
const limitSource = ref<ProbeSource | null>(null)
const limitForm = reactive({ min_interval_sec: 300, per_target_per_min: 12, high_freq_allowed: false })
const limitSaving = ref(false)
const limitError = ref('')
const limitMin = computed(() => (limitForm.high_freq_allowed ? 60 : 300))

function openLimit(s: ProbeSource) {
  limitSource.value = s
  Object.assign(limitForm, { min_interval_sec: s.min_interval_sec, per_target_per_min: s.per_target_per_min, high_freq_allowed: s.high_freq_allowed })
  limitError.value = ''
  limitOpen.value = true
}

async function saveLimit() {
  const s = limitSource.value
  if (!s) return
  limitSaving.value = true
  limitError.value = ''
  try {
    await api.updateProbeSource(s.kind, {
      high_freq_allowed: limitForm.high_freq_allowed,
      min_interval_sec: limitForm.min_interval_sec,
      per_target_per_min: limitForm.per_target_per_min,
    })
    message.success('已保存')
    limitOpen.value = false
    emit('changed')
  } catch (err) {
    limitError.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    limitSaving.value = false
  }
}
</script>

<template>
  <div class="psc" data-testid="probe-sources">
    <div v-for="s in props.sources" :key="s.kind" class="lb-card psc__card" :data-testid="`probe-source-${s.kind}`">
      <div class="psc__head">
        <div class="psc__name">
          {{ s.name }}
          <span v-if="s.third_party" class="lb-chip">第三方</span>
        </div>
        <LbStatusTag :meta="stateMeta(s)" />
      </div>
      <div class="psc__meta lb-tabular">
        <span>{{ s.target_count }} 个目标 · {{ s.active_count }} 组在测</span>
        <template v-if="s.third_party">
          <span>间隔 ≥ {{ s.min_interval_sec }} 秒<template v-if="s.high_freq_allowed">(已声明更高频率许可)</template></span>
          <span>单目标 ≤ {{ s.per_target_per_min }} 次 / 分钟</span>
        </template>
        <span v-else>间隔按目标自己的设置(≥ 30 秒)</span>
      </div>
      <div v-if="s.third_party && s.authorized" class="psc__muted">
        {{ s.authorized_by }} 于 <LbTimeText :value="s.authorized_at" /> 确认<template v-if="s.authorized_note">:{{ s.authorized_note }}</template>
      </div>
      <div v-if="s.syncable && s.sync_state !== 'NEVER'" class="psc__muted" :class="{ psc__bad: s.sync_state === 'ERROR' }">
        <template v-if="s.sync_state === 'ERROR'">
          上次拉取失败(<LbTimeText :value="s.last_sync_at" />):{{ s.last_error }} —— 已导入的目标不受影响
        </template>
        <template v-else>上次拉取 <LbTimeText :value="s.last_success_at" /> · {{ s.item_count }} 条</template>
      </div>
      <div class="psc__ops">
        <a-button v-if="s.third_party" size="small" class="lb-btn-ghost" @click="openAuth(s)">
          {{ s.authorized ? '说明与授权' : '阅读说明并确认' }}
        </a-button>
        <a-button v-if="s.kind !== 'MANUAL'" size="small" class="lb-btn-ghost" @click="emit('import', s.kind)">
          {{ s.kind === 'ZSTATIC' ? '预置与目录' : '同步节点' }}
        </a-button>
        <a-button v-if="s.third_party" size="small" class="lb-btn-ghost lb-btn-ghost--text" @click="openLimit(s)">限额</a-button>
        <a-button size="small" class="lb-btn-ghost lb-btn-ghost--text" :loading="busy === s.kind" @click="togglePause(s)">
          {{ s.paused ? '恢复' : '暂停' }}
        </a-button>
      </div>
    </div>

    <a-modal
      :open="authOpen"
      :title="authSource ? `${authSource.name} · 使用说明与授权` : ''"
      :width="580"
      :mask-closable="false"
      @cancel="authOpen = false"
    >
      <div v-if="authSource" class="psc__auth" data-testid="probe-source-auth">
        <ul class="psc__notice">
          <li v-for="(n, i) in authSource.notice" :key="i">{{ n }}</li>
        </ul>
        <a v-if="authSource.homepage" :href="authSource.homepage" target="_blank" rel="noopener noreferrer" class="psc__link">
          <LbIcon name="external-link" :size="13" />查看来源原文
        </a>
        <template v-if="!authSource.authorized">
          <a-checkbox v-model:checked="authRead">已阅读上面的说明,并确认具备使用授权(含频率与目标可用性)</a-checkbox>
          <a-input v-model:value="authNote" :maxlength="200" placeholder="授权说明(选填,例如:已与来源方确认个人研究用途、每目标 5 分钟一次)" />
        </template>
        <div v-else class="psc__muted">
          {{ authSource.authorized_by }} 于 <LbTimeText :value="authSource.authorized_at" /> 确认。撤销之后这个来源的目标对所有节点停测(历史保留)。
        </div>
      </div>
      <template #footer>
        <a-button @click="authOpen = false">关闭</a-button>
        <a-button v-if="authSource?.authorized" danger :loading="authSaving" @click="confirmAuth(false)">撤销授权确认</a-button>
        <a-button v-else type="primary" :loading="authSaving" :disabled="!authRead" @click="confirmAuth(true)">确认</a-button>
      </template>
    </a-modal>

    <a-modal
      :open="limitOpen"
      :title="limitSource ? `${limitSource.name} · 限额` : ''"
      :width="500"
      :confirm-loading="limitSaving"
      ok-text="保存"
      cancel-text="取消"
      :mask-closable="false"
      @cancel="limitOpen = false"
      @ok="saveLimit"
    >
      <div class="psc__auth">
        <div v-if="limitError" class="lb-error-strip">{{ limitError }}</div>
        <div class="lb-group">
          <div class="lb-group__row psc__row psc__row--switch">
            <span class="lb-group__label">
              已获得来源方的更高频率许可
              <LbInfoTip warn :width="300" text="只有明确拿到来源方许可时才打开:打开后最小间隔可以调到 60 秒。关掉时下限自动回到 300 秒。这同样只是管理员的声明。" />
            </span>
            <a-switch v-model:checked="limitForm.high_freq_allowed" />
          </div>
          <div class="lb-group__row psc__row">
            <span class="lb-group__label">最小间隔</span>
            <a-input-number v-model:value="limitForm.min_interval_sec" :min="limitMin" :max="3600" :precision="0" addon-after="秒" />
          </div>
          <div class="lb-group__row psc__row">
            <span class="lb-group__label">
              单目标全站限流
              <LbInfoTip :width="300" text="同一个目标,全部节点合计每分钟最多多少次实际探测尝试(ICMP 按包、TCP 握手近似按次、TCP 建连按次);每个节拍(15 秒)最多四分之一。超出的延后执行,不记失败。" />
            </span>
            <a-input-number v-model:value="limitForm.per_target_per_min" :min="1" :max="600" :precision="0" addon-after="次 / 分钟" />
          </div>
        </div>
      </div>
    </a-modal>
  </div>
</template>

<style scoped>
.psc {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
  gap: var(--sp-3);
}
.psc__card {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.psc__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
}
.psc__name {
  display: inline-flex;
  align-items: center;
  gap: var(--sp-2);
  font-weight: 600;
}
.psc__meta {
  display: flex;
  flex-wrap: wrap;
  gap: 2px var(--sp-3);
  font-size: var(--fs-xs);
  color: var(--text2);
}
.psc__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
  line-height: 1.6;
}
.psc__bad {
  color: var(--bad);
}
.psc__ops {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-1);
  margin-top: auto;
}
.psc__auth {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.psc__notice {
  margin: 0;
  padding-left: 18px;
  display: flex;
  flex-direction: column;
  gap: 6px;
  font-size: var(--fs-sm);
  color: var(--text2);
  line-height: 1.6;
}
.psc__link {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-sm);
}
.psc__row {
  grid-template-columns: 150px minmax(0, 1fr);
}
.psc__row--switch {
  grid-template-columns: minmax(0, 1fr) auto;
}
</style>
