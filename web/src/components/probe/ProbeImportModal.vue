<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type ProbeImportResult } from '@/api/client'
import { familyText, ispText, metricLabel, originText, redactUrl, tcpModeOf } from '@/components/asset/probeView'

/**
 * 批量导入自定义目标:粘贴 CSV 或 TSV(第一行是表头),先预览再提交。导入的目标不对任何节点生效。
 * 主机属于已知第三方来源的(*.ip.zstaticcdn.com 等)归到那个来源,间隔按它的下限校验。
 */
const props = defineProps<{ open: boolean }>()
const emit = defineEmits<{ (e: 'close'): void; (e: 'imported', n: number): void }>()

const SAMPLE = `name,host,protocol,tcp_mode,port,family,interval_sec,region,province,city,isp,note
自建 · 东京,203.0.113.10,TCP,CONNECT,443,V4,60,海外,,,OTHER,自己的机器
自建 · 香港 ICMP,203.0.113.11,ICMP,,,V4,60,海外,,,OTHER,`

const text = ref('')
const preview = ref<ProbeImportResult | null>(null)
const busy = ref(false)
const error = ref('')

watch(
  () => props.open,
  (open) => {
    if (open) {
      preview.value = null
      error.value = ''
    }
  },
)
watch(text, () => {
  preview.value = null
})

async function run(commit: boolean) {
  busy.value = true
  error.value = ''
  try {
    const r = await api.importPublicProbes(text.value, commit)
    if (commit) {
      message.success(`已导入 ${r.created} 个目标,还没有对任何节点启用`)
      emit('imported', r.created)
    } else {
      preview.value = r
    }
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '导入失败'
  } finally {
    busy.value = false
  }
}

const canCommit = computed(() => !!preview.value && preview.value.ok > 0)
</script>

<template>
  <a-modal :open="open" title="批量导入目标" :width="820" :mask-closable="false" @cancel="emit('close')">
    <div class="pim" data-testid="probe-import">
      <div class="pim__muted">
        粘贴 CSV 或 TSV,第一行是表头。可用列:name、host(必填;HTTP 目标填 url 代替)、url(http:// 或 https:// 的完整地址,填了就是 HTTP)、protocol(ICMP / TCP / HTTP,有端口时默认 TCP)、tcp_mode(CONNECT / SYN)、
        port、family(V4 / V6,域名默认 V4)、interval_sec、timeout_ms、count、region、province、city、isp(电信 / 联通 / 移动 / 教育网 / 其他)、note;
        中文列名(名称、主机、端口…)也认。受限地址(私网、文档地址段等)与受限端口默认拒绝,确实要测请导入后在编辑里放开。
        <button type="button" class="lb-linkbtn" @click="text = SAMPLE">填入格式示例</button>
        (示例里是文档地址段,只为说明格式,预览会把它们判成受限地址;导入前换成自己的地址)
      </div>
      <a-textarea v-model:value="text" :rows="8" class="lb-mono" placeholder="name,host,port&#10;我的测速点,203.0.113.10,443" />
      <div v-if="error" class="lb-error-strip">{{ error }}</div>
      <template v-if="preview">
        <div class="pim__sum lb-tabular">
          可导入 <b>{{ preview.ok }}</b> · 重复 {{ preview.duplicates }} · 错误 {{ preview.errors }}
        </div>
        <div class="pim__table">
          <table>
            <thead>
              <tr><th>行</th><th>结果</th><th>名称</th><th>主机</th><th>方式</th><th>地区</th><th>来源</th></tr>
            </thead>
            <tbody>
              <tr v-for="r in preview.rows" :key="r.line" :class="`pim__row--${r.status.toLowerCase()}`">
                <td class="lb-tabular">{{ r.line }}</td>
                <td>
                  <span v-if="r.status === 'OK'" class="pim__ok">可导入</span>
                  <span v-else-if="r.status === 'DUPLICATE'" class="pim__muted">重复</span>
                  <span v-else class="pim__bad" :title="r.reason">错误:{{ r.reason }}</span>
                </td>
                <td class="lb-ellipsis">{{ r.input.name }}</td>
                <td class="lb-mono">{{ r.input.protocol === 'HTTP' ? redactUrl(r.input.url ?? '') : r.input.host }}<template v-if="r.input.port && r.input.protocol !== 'HTTP'">:{{ r.input.port }}</template></td>
                <td>{{ metricLabel(tcpModeOf(r.input.protocol, r.input.tcp_mode)) }} · {{ familyText(r.input.family) }}</td>
                <td>{{ [r.input.region, r.input.province, r.input.city, ispText(r.input.isp ?? '')].filter(Boolean).join(' ') || '—' }}</td>
                <td>{{ r.status === 'OK' ? originText(r.source) : '' }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </div>
    <template #footer>
      <a-button @click="emit('close')">取消</a-button>
      <a-button :loading="busy && !canCommit" :disabled="!text.trim()" @click="run(false)">预览</a-button>
      <a-button type="primary" :loading="busy && canCommit" :disabled="!canCommit" @click="run(true)">
        导入 {{ preview?.ok ?? 0 }} 个
      </a-button>
    </template>
  </a-modal>
</template>

<style scoped>
.pim {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.pim__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
  line-height: 1.6;
}
.pim__sum {
  font-size: var(--fs-sm);
}
.pim__table {
  max-height: 300px;
  overflow: auto;
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
}
.pim__table table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--fs-xs);
}
.pim__table th,
.pim__table td {
  padding: 6px 8px;
  border-bottom: 1px solid var(--sep2);
  text-align: left;
  white-space: nowrap;
  max-width: 220px;
}
.pim__table th {
  position: sticky;
  top: 0;
  background: var(--surface2);
  font-weight: 600;
  color: var(--text2);
}
.pim__ok {
  color: var(--ok);
}
.pim__bad {
  color: var(--bad);
  white-space: normal;
}
</style>
