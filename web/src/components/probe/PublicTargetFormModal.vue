<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type ProbeFamily,
  type ProbeISP,
  type ProbeProtocol,
  type ProbeProvince,
  type ProbeSource,
  type ProbeTCPMode,
  type PublicProbeInput,
  type PublicProbeTarget,
} from '@/api/client'
import { LbInfoTip } from '@/components/lb'
import { originText } from '@/components/asset/probeView'

/**
 * 新增 / 编辑一个公共目标。协议、TCP 方式与地址族创建后不能改(换了就是另一条测量)。
 * HTTP(S)(V22.6)填完整 URL,主机与端口由它推出来;URL 可以改(统计重新起算),http 与 https 不能互换。
 * 主机属于已知第三方来源的(*.ip.zstaticcdn.com 等),服务端会把它归到那个来源 ——
 * 间隔不能低于那个来源的下限,对节点启用之前要先确认授权。
 */
const props = defineProps<{
  open: boolean
  target: PublicProbeTarget | null
  provinces: ProbeProvince[]
  sources: ProbeSource[]
  nextSort: number
}>()
const emit = defineEmits<{
  (e: 'close'): void
  (e: 'saved', p: PublicProbeTarget, created: boolean): void
}>()

const SYN_MAX = 900
/** HTTP 一次请求(DNS + 建连 + TLS + 等响应头)的总超时上限与默认值,与辅助程序一致。 */
const HTTP_MAX = 10000
const HTTP_DEFAULT = 5000
const saving = ref(false)
const formError = ref('')
const form = reactive({
  name: '',
  host: '',
  url: '',
  protocol: 'ICMP' as ProbeProtocol,
  tcp_mode: 'CONNECT' as ProbeTCPMode,
  port: null as number | null,
  family: 'V4' as ProbeFamily,
  interval_sec: 60 as number | null,
  count: 4 as number | null,
  timeout_ms: 1000 as number | null,
  sort_order: 0 as number | null,
  note: '',
  region: '',
  province: '',
  city: '',
  isp: '' as ProbeISP,
  allow_restricted: false,
  enabled: true,
})

watch(
  () => props.open,
  (open) => {
    if (!open) return
    formError.value = ''
    const p = props.target
    if (p) {
      Object.assign(form, {
        name: p.name, host: p.host, url: p.url ?? '', protocol: p.protocol, tcp_mode: p.tcp_mode,
        port: p.protocol === 'TCP' ? p.port : null,
        family: p.family, interval_sec: p.interval_sec, count: p.count, timeout_ms: p.timeout_ms, sort_order: p.sort_order,
        note: p.note, region: p.region, province: p.province, city: p.city, isp: p.isp,
        allow_restricted: p.allow_restricted, enabled: p.enabled,
      })
    } else {
      Object.assign(form, {
        name: '', host: '', url: '', protocol: 'ICMP', tcp_mode: 'CONNECT', port: null, family: 'V4', interval_sec: 60, count: 4,
        timeout_ms: 1000, sort_order: props.nextSort, note: '', region: '', province: '', city: '', isp: '',
        allow_restricted: false, enabled: true,
      })
    }
  },
)

watch(
  () => [form.protocol, form.tcp_mode] as const,
  ([proto, mode], old) => {
    if (props.target) return
    // 握手近似每次最多等 900 毫秒(要早于内核 1 秒的首次 SYN 重发)。
    if (proto === 'TCP' && mode === 'SYN' && (form.timeout_ms ?? 0) > SYN_MAX) form.timeout_ms = SYN_MAX
    // HTTP 的超时管整个请求,默认 5 秒;换回 ICMP / TCP 时夹回它们的上限。
    if (proto === 'HTTP' && old?.[0] !== 'HTTP') form.timeout_ms = HTTP_DEFAULT
    if (proto !== 'HTTP' && (form.timeout_ms ?? 0) > 3000) form.timeout_ms = 1000
  },
)

const isHTTP = computed(() => form.protocol === 'HTTP')
/** URL 推出来的主机与端口(只做预览;以服务端校验为准)。 */
const parsedURL = computed<{ host: string; port: number; https: boolean } | null>(() => {
  if (!isHTTP.value) return null
  try {
    const u = new URL(form.url.trim())
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return null
    const https = u.protocol === 'https:'
    return { host: u.hostname.replace(/^\[|\]$/g, ''), port: u.port ? Number(u.port) : https ? 443 : 80, https }
  } catch {
    return null
  }
})

/** 这个主机会归到哪个第三方来源(与服务端 OriginForHost 同一张后缀表)。 */
const thirdParty = computed<ProbeSource | null>(() => {
  const h = (isHTTP.value ? (parsedURL.value?.host ?? '') : form.host).trim().toLowerCase().replace(/\.$/, '')
  const kind = props.target?.source && props.target.source !== 'MANUAL'
    ? props.target.source
    : h.endsWith('.ip.zstaticcdn.com')
      ? 'ZSTATIC'
      : h.endsWith('.ibsgss.uk')
        ? 'TCPQUALITY'
        : null
  return kind ? (props.sources.find((s) => s.kind === kind) ?? null) : null
})
const minInterval = computed(() => (thirdParty.value ? thirdParty.value.min_interval_sec : 30))
const isSyn = computed(() => form.protocol === 'TCP' && form.tcp_mode === 'SYN')
const provinceOptions = computed(() => props.provinces.map((p) => ({ value: p.name })))
function provinceFilter(input: string, o?: { value?: unknown }): boolean {
  return String(o?.value ?? '').includes(input)
}

async function submit() {
  formError.value = ''
  const body: PublicProbeInput = {
    name: form.name.trim(),
    host: isHTTP.value ? (parsedURL.value?.host ?? '') : form.host.trim(),
    url: isHTTP.value ? form.url.trim() : '',
    protocol: form.protocol,
    tcp_mode: form.protocol === 'TCP' ? form.tcp_mode : 'CONNECT',
    port: form.protocol === 'TCP' ? form.port ?? 0 : 0,
    family: form.family,
    interval_sec: form.interval_sec ?? 0,
    count: form.protocol === 'ICMP' || isSyn.value ? form.count ?? 0 : 0,
    timeout_ms: form.timeout_ms ?? 0,
    allow_restricted: form.allow_restricted,
    sort_order: form.sort_order ?? 0,
    note: form.note.trim(),
    region: form.region.trim(),
    province: form.province.trim(),
    city: form.city.trim(),
    isp: form.isp,
  }
  if (props.target) body.enabled = form.enabled
  saving.value = true
  try {
    if (props.target) {
      const p = await api.updatePublicProbeTarget(props.target.id, body)
      message.success('已保存')
      emit('saved', p, false)
    } else {
      const p = await api.createPublicProbeTarget(body)
      message.success(
        p.third_party
          ? `已登记到「${originText(p.source)}」来源,还没有对任何节点启用`
          : '已登记,还没有对任何节点启用 —— 接着选要测它的节点',
      )
      emit('saved', p, true)
    }
  } catch (err) {
    formError.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <a-modal
    :open="open"
    :title="target ? `编辑目标 · ${target.name}` : '新增探测目标'"
    :width="600"
    :confirm-loading="saving"
    :ok-text="target ? '保存' : '登记'"
    cancel-text="取消"
    :mask-closable="false"
    @cancel="emit('close')"
    @ok="submit"
  >
    <div class="ptm" data-testid="public-target-form">
      <div v-if="formError" class="lb-error-strip">{{ formError }}</div>
      <div v-if="thirdParty" class="lb-notice ptm__notice">
        <div class="lb-notice__body">
          <div class="lb-notice__title">这个主机属于第三方来源「{{ thirdParty.name }}」</div>
          <div class="lb-notice__text">
            它会归到那个来源:探测间隔不低于 {{ thirdParty.min_interval_sec }} 秒,对节点启用前要先在来源卡片里确认授权,
            并计入第三方预算与单目标限流。确有授权的自建或第三方业务站点请用它们自己的地址登记。
          </div>
        </div>
      </div>
      <div class="lb-group">
        <div class="lb-group__row ptm__row">
          <span class="lb-group__label"><span class="ptm__req">*</span> 名称</span>
          <a-input v-model:value="form.name" placeholder="例如 自建测速点" :maxlength="64" />
        </div>
        <div class="lb-group__row ptm__row">
          <span class="lb-group__label">
            <span class="ptm__req">*</span> 协议
            <LbInfoTip
              :width="340"
              text="ICMP:往返时间与真实丢包率(每轮连发几个包)。TCP 建连:一次完整建连,看连接失败率(内核在超时内会照常重发 SYN,丢了第一个 SYN 只表现为延迟变大)。TCP 握手近似:每次只等一个 SYN 的应答、每轮做几次,看握手超时无应答率 —— 它基于系统的 TCP 建连,不是原始套接字的 SYN 探测,也不是真实的 TCP 丢包率。HTTP(S):每轮一次 GET,计时从发起请求(含 DNS、建连、TLS)到收到响应头,看 HTTP 请求失败率;不跟随跳转、不下载正文、校验证书,200 / 404 / 5xx 都算取得了响应。创建后不能改。"
            />
          </span>
          <div class="ptm__inline">
            <a-radio-group v-model:value="form.protocol" :disabled="!!target" button-style="solid" size="small">
              <a-radio-button value="ICMP">ICMP</a-radio-button>
              <a-radio-button value="TCP">TCP</a-radio-button>
              <a-radio-button value="HTTP" data-testid="ptm-proto-http">HTTP(S)</a-radio-button>
            </a-radio-group>
            <a-radio-group v-if="form.protocol === 'TCP'" v-model:value="form.tcp_mode" :disabled="!!target" button-style="solid" size="small">
              <a-radio-button value="CONNECT">建连</a-radio-button>
              <a-radio-button value="SYN">握手近似</a-radio-button>
            </a-radio-group>
          </div>
        </div>
        <div class="lb-group__row ptm__row">
          <span class="lb-group__label"><span class="ptm__req">*</span> 地址族</span>
          <a-radio-group v-model:value="form.family" :disabled="!!target" button-style="solid" size="small">
            <a-radio-button value="V4">IPv4</a-radio-button>
            <a-radio-button value="V6">IPv6</a-radio-button>
          </a-radio-group>
        </div>
        <div v-if="isHTTP" class="lb-group__row ptm__row">
          <span class="lb-group__label">
            <span class="ptm__req">*</span> URL
            <LbInfoTip
              :width="340"
              text="完整的 http:// 或 https:// 地址,可以带端口、路径与查询参数,例如 https://www.example.com/generate_204、http://example.com:8080/health。不收账号密码;不跟随 301 / 302,3xx 本身就是结果;不读节点上的 HTTP_PROXY 之类的代理设置;解析到私网、回环等受限地址时拒绝探测(只连查过的那个地址)。查询参数不会写进日志、推送与审计。"
            />
          </span>
          <div class="ptm__url">
            <a-input
              v-model:value="form.url"
              class="lb-mono"
              placeholder="https://www.example.com/generate_204"
              :maxlength="2048"
              data-testid="ptm-url"
            />
            <span class="ptm__muted">
              <template v-if="parsedURL">将由各节点连接 {{ parsedURL.host }}:{{ parsedURL.port }}({{ parsedURL.https ? 'HTTPS,校验证书' : 'HTTP' }})</template>
              <template v-else-if="form.url.trim()">只支持 http:// 与 https:// 开头的完整地址</template>
              <template v-else>示例:https://www.example.com/generate_204 · http://example.com:8080/health</template>
            </span>
          </div>
        </div>
        <div v-else class="lb-group__row ptm__row">
          <span class="lb-group__label">
            <span class="ptm__req">*</span> 主机
            <LbInfoTip
              :width="320"
              text="IP 或域名。域名由每台机器在本机解析(只查 A 或 AAAA),换了指向就跟着测新地址,最近一次实际连的 IP 会记下来;解析到私网、回环等受限地址时拒绝探测。"
            />
          </span>
          <a-input v-model:value="form.host" class="lb-mono" placeholder="1.1.1.1 或 one.one.one.one" />
        </div>
        <div v-if="form.protocol === 'TCP'" class="lb-group__row ptm__row">
          <span class="lb-group__label"><span class="ptm__req">*</span> 端口</span>
          <a-input-number v-model:value="form.port" :min="1" :max="65535" :precision="0" class="ptm__num" placeholder="443" />
        </div>
      </div>
      <div class="lb-group">
        <div class="lb-group__row ptm__row">
          <span class="lb-group__label">
            探测间隔
            <LbInfoTip :width="280" :text="`最低 ${minInterval} 秒。单台节点可以在节点详情里覆盖,但同样不能低于来源下限。`" />
          </span>
          <a-input-number v-model:value="form.interval_sec" :min="minInterval" :max="3600" :precision="0" addon-after="秒" class="ptm__num" />
        </div>
        <div v-if="form.protocol === 'ICMP' || isSyn" class="lb-group__row ptm__row">
          <span class="lb-group__label">{{ isSyn ? '每轮握手次数' : '每轮包数' }}</span>
          <a-input-number v-model:value="form.count" :min="1" :max="10" :precision="0" :addon-after="isSyn ? '次' : '个'" class="ptm__num" />
        </div>
        <div class="lb-group__row ptm__row">
          <span class="lb-group__label">
            超时
            <LbInfoTip
              :width="300"
              :text="isSyn ? '每次握手最多等多久,200~900 毫秒:要早于内核 1 秒的首次 SYN 重发,每次才只有一个 SYN 在路上。应答晚于它记为无应答。' : isHTTP ? '整个请求(DNS + 建连 + TLS + 等响应头)的总超时,200~10000 毫秒;超时不会记一个假的 0 毫秒耗时。' : '单个包 / 单次建连的超时,200~3000 毫秒。'"
            />
          </span>
          <a-input-number
            v-model:value="form.timeout_ms"
            :min="200"
            :max="isSyn ? SYN_MAX : isHTTP ? HTTP_MAX : 3000"
            :precision="0"
            addon-after="毫秒"
            class="ptm__num"
          />
        </div>
      </div>
      <div class="lb-group">
        <div class="lb-group__row ptm__row">
          <span class="lb-group__label">地区</span>
          <a-input v-model:value="form.region" :maxlength="32" placeholder="华东 / 海外…(选填,用于筛选)" />
        </div>
        <div class="lb-group__row ptm__row">
          <span class="lb-group__label">省份 / 城市</span>
          <div class="ptm__inline">
            <a-auto-complete
              v-model:value="form.province"
              class="ptm__half"
              :options="provinceOptions"
              placeholder="省份"
              :filter-option="provinceFilter"
            />
            <a-input v-model:value="form.city" class="ptm__half" :maxlength="32" placeholder="城市" />
          </div>
        </div>
        <div class="lb-group__row ptm__row">
          <span class="lb-group__label">运营商</span>
          <a-select v-model:value="form.isp" class="ptm__num">
            <a-select-option value="">不区分</a-select-option>
            <a-select-option value="CT">电信</a-select-option>
            <a-select-option value="CU">联通</a-select-option>
            <a-select-option value="CM">移动</a-select-option>
            <a-select-option value="CERNET">教育网</a-select-option>
            <a-select-option value="OTHER">其他</a-select-option>
          </a-select>
        </div>
        <div class="lb-group__row ptm__row">
          <span class="lb-group__label">排序</span>
          <a-input-number v-model:value="form.sort_order" :precision="0" class="ptm__num" />
        </div>
        <div class="lb-group__row ptm__row">
          <span class="lb-group__label">备注</span>
          <a-input v-model:value="form.note" :maxlength="200" placeholder="选填,只给管理员看" />
        </div>
        <div class="lb-group__row ptm__row ptm__row--switch">
          <span class="lb-group__label">
            允许受限地址与端口
            <LbInfoTip
              warn
              :width="320"
              text="默认拒绝:私网、回环、链路本地(含云元数据 169.254.169.254)、CGNAT、保留与文档地址段,以及邮件(25 / 465 / 587)与 SMB / NetBIOS(135~139 / 445)端口 —— 也包括域名解析到这些地址的情况。勾选之后各台机器会去测它,确认这是你想要的。"
            />
          </span>
          <a-switch v-model:checked="form.allow_restricted" />
        </div>
        <div v-if="target" class="lb-group__row ptm__row ptm__row--switch">
          <span class="lb-group__label">
            启用
            <LbInfoTip :width="260" text="关掉 = 暂停:哪台机器都不再测,历史与节点开关都保留。" />
          </span>
          <a-switch v-model:checked="form.enabled" />
        </div>
      </div>
      <div v-if="target" class="ptm__muted">
        改主机、端口或 URL 之后,各台机器上的统计从改动那一刻重新起算<template v-if="target.protocol === 'HTTP'">;http:// 与 https:// 不能互换</template>。
        <template v-if="target.source !== 'MANUAL'">从来源导入的目标,手工改过的名称、地址、地区等字段在之后同步时不会被上游覆盖。</template>
      </div>
    </div>
  </a-modal>
</template>

<style scoped>
.ptm {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.ptm__row {
  grid-template-columns: 130px minmax(0, 1fr);
}
.ptm__row--switch {
  grid-template-columns: minmax(0, 1fr) auto;
}
.ptm__inline {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-2);
}
.ptm__half {
  flex: 1 1 140px;
  min-width: 0;
}
.ptm__num {
  width: 180px;
}
.ptm__url {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}
.ptm__req {
  color: var(--bad);
}
.ptm__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.ptm__notice {
  margin: 0;
}
@media (max-width: 767px) {
  .ptm__row {
    grid-template-columns: 1fr;
  }
}
</style>
