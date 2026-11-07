<script setup lang="ts">
// 注册向导（FR1.1 三步）：选类型 → 配置（默认即实测可用配置）→ 测试并提交。
// 降低注册难度：adapter 卡片化 + 默认配置预填（schema Default 来自真实 agent 实测）+
// 名字自动生成 + 能力折叠为高级项 + 探活失败给排查建议。
import { computed, onMounted, ref } from 'vue'
import DynamicConfigForm from '../components/DynamicConfigForm.vue'
import { buildConfigPayload, discoverHermes, initialFormValues, listAdapters, registerAgent, testSpec } from '../api/agents'
import type { AdapterInfo, Agent, Capabilities, TestResult } from '../api/types'
import { ApiError } from '../api/client'

const step = ref(1)
const adapters = ref<AdapterInfo[]>([])
const loadError = ref('')

const name = ref('')
const adapterName = ref('')
const formValues = ref<Record<string, string | boolean>>({})

// 能力可选覆盖（高级项，默认折叠）
const showAdvanced = ref(false)
const capMaxConcurrency = ref('')
const capInterruptLevel = ref('')
const capTypicalLatencyS = ref('')

// hermes 自动探测（端口段 8640-8649）
const probingHermes = ref(false)
const hermesCandidates = ref<Array<{ port: number; base_url: string; platform?: string; version?: string; auth_required: boolean }>>([])
const hermesNote = ref('')

async function probeHermes(): Promise<void> {
  probingHermes.value = true
  hermesNote.value = ''
  try {
    const res = await discoverHermes()
    hermesCandidates.value = res.candidates
    if (res.candidates.length === 1) {
      const c = res.candidates[0]
      formValues.value['base_url'] = c.base_url
      hermesNote.value = `已检测到本机网关 :${c.port}（${c.platform}${c.version ? ' ' + c.version : ''}）${c.auth_required ? ' · 需要密钥（填 ${API_SERVER_KEY} 引用环境变量）' : ' · 免鉴权'}`
    } else if (res.candidates.length > 1) {
      formValues.value['base_url'] = res.candidates[0].base_url
      hermesNote.value = `检测到 ${res.candidates.length} 个网关（端口 ${res.candidates.map((c) => c.port).join('、')}），已选第一个，可在下方地址栏切换`
    } else {
      hermesNote.value = '本机 8640-8649 未检测到网关——请手动填地址，或启动网关后重进本步自动重探'
    }
  } catch {
    hermesNote.value = '探测失败（网络异常），请手动填地址'
  } finally {
    probingHermes.value = false
  }
}

const sendMessage = ref(false)
const testing = ref(false)
const submitting = ref(false)
const testResult = ref<TestResult | null>(null)
const submitOk = ref<Agent | null>(null)
const submitError = ref('')

// adapter 快速开始卡片元数据（与后端实测默认配置呼应）
const ADAPTER_CARDS: Record<string, { tagline: string; ready: string }> = {
  echod: { tagline: '内置测试假 agent（回显/流式/故障注入）', ready: '零配置，开箱即用' },
  hermes: { tagline: 'HTTP 型范式 · 接入本机 hermes 网关', ready: '默认指向本机 :8642，通常只需确认地址' },
  picoclaw: { tagline: 'CLI 型范式 · 调用本机 picoclaw 命令', ready: '命令模板/输出解析已预填实测值' },
}

const schema = computed(() => adapters.value.find((a) => a.name === adapterName.value)?.config_schema.fields ?? [])

onMounted(async () => {
  try {
    adapters.value = await listAdapters()
    if (adapters.value.length) {
      adapterName.value = adapters.value[0].name
      formValues.value = initialFormValues(schema.value)
    }
  } catch (e) {
    loadError.value = e instanceof Error ? e.message : String(e)
  }
})

function pickAdapter(name: string): void {
  adapterName.value = name
  formValues.value = initialFormValues(schema.value)
  testResult.value = null
  submitOk.value = null
  if (name === 'hermes') void probeHermes()
}

function go(n: number): void {
  step.value = n
}

function stepClass(n: number): Record<string, boolean> {
  return { on: step.value === n, done: step.value > n }
}

/** 名字留空时自动生成（adapter-日期-随机后缀）。 */
function autoName(): string {
  const d = new Date()
  const ymd = `${d.getFullYear()}${String(d.getMonth() + 1).padStart(2, '0')}${String(d.getDate()).padStart(2, '0')}`
  const suffix = Math.random().toString(36).slice(2, 6)
  return `${adapterName.value || 'agent'}-${ymd}-${suffix}`
}

/** 能力覆盖 → Capabilities 负载（未设置的项传 0，后端 MergeDefaults 沿用 adapter 默认）。 */
function capabilitiesPayload(): Record<string, unknown> | undefined {
  const mc = Number.parseInt(capMaxConcurrency.value, 10)
  const il = Number.parseInt(capInterruptLevel.value, 10)
  const lat = Number.parseFloat(capTypicalLatencyS.value)
  if (!Number.isFinite(mc) && !Number.isFinite(il) && !Number.isFinite(lat)) return undefined
  return {
    can_group_chat: false,
    can_stream: false,
    max_concurrency: Number.isFinite(mc) ? mc : 0,
    typical_latency: Number.isFinite(lat) ? `${lat}s` : '0s',
    interrupt_level: Number.isFinite(il) ? il : 0,
  }
}

/** 探活失败的排查建议（按 adapter 与错误内容映射常见原因）。 */
const HINTS: Record<string, Array<[string, string]>> = {
  hermes: [
    ['unauthorized|401', 'API Key 不正确——确认网关 API_SERVER_KEY'],
    ['unreachable|connect|refused', '网关未启动或地址/端口不对——检查 hermes gateway 是否在运行'],
    ['503', '网关在线但该 profile 不可用'],
  ],
  picoclaw: [
    ['executable|not found|no such', 'picoclaw 不在 PATH——用绝对路径或先安装'],
    ['probe|exit', '探活命令失败——检查二进制与版本'],
    ['parse|malformed|empty', '输出解析失败——确认 output 模式与回复行前缀'],
  ],
  echod: [['down|health', 'echod mode=down 为预期探活失败（故障注入模式）']],
}

function troubleshooting(err: string): string {
  for (const [pattern, tip] of HINTS[adapterName.value] ?? []) {
    if (new RegExp(pattern, 'i').test(err)) return tip
  }
  return ''
}

async function runTest(): Promise<void> {
  if (testing.value) return
  testing.value = true
  testResult.value = null
  try {
    testResult.value = await testSpec(adapterName.value, buildConfigPayload(schema.value, formValues.value), sendMessage.value)
  } catch (e) {
    testResult.value = { ok: false, duration_ms: 0, error: e instanceof Error ? e.message : String(e) }
  } finally {
    testing.value = false
  }
}

async function submit(): Promise<void> {
  if (submitting.value) return
  if (!name.value.trim()) name.value = autoName()
  submitting.value = true
  submitError.value = ''
  try {
    const caps = capabilitiesPayload()
    const agent = await registerAgent({
      name: name.value.trim(),
      adapter: adapterName.value,
      config: buildConfigPayload(schema.value, formValues.value),
      ...(caps ? { capabilities: caps as unknown as Capabilities } : {}),
    })
    submitOk.value = agent
  } catch (e) {
    submitError.value =
      e instanceof ApiError ? `${e.message}（${e.code}）` : e instanceof Error ? e.message : String(e)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div>
    <div class="page-head">
      <div>
        <h1>注册 Agent</h1>
        <div class="sub">默认配置即真实 agent 实测值——多数情况只需确认地址，一路下一步</div>
      </div>
    </div>

    <div class="steps">
      <div v-for="n in 3" :key="n" class="step" :class="stepClass(n)">
        <span class="dot">{{ step > n ? '✓' : n }}</span>
        {{ ['选择类型', '确认配置', '测试并提交'][n - 1] }}
      </div>
    </div>

    <div class="card">
      <div v-if="loadError" class="callout bad">{{ loadError }}</div>

      <!-- 第 1 步：快速开始卡片 + 名字 -->
      <fieldset v-show="step === 1" style="border: none; padding: 0; margin: 0">
        <label class="f-label">Agent 名字（留空自动生成）</label>
        <input v-model="name" class="f-input" type="text" :placeholder="`如 ${adapterName || 'agent'}-20261107-a1b2`" />

        <label class="f-label">选择类型</label>
        <div class="adapter-cards">
          <button
            v-for="a in adapters"
            :key="a.name"
            class="adapter-card"
            :class="{ on: adapterName === a.name }"
            type="button"
            @click="pickAdapter(a.name)"
          >
            <span class="adapter-name">{{ a.name }}</span>
            <span class="adapter-tagline">{{ ADAPTER_CARDS[a.name]?.tagline ?? '' }}</span>
            <span v-if="ADAPTER_CARDS[a.name]?.ready" class="adapter-ready">✓ {{ ADAPTER_CARDS[a.name].ready }}</span>
          </button>
        </div>

        <div style="margin-top: 18px">
          <button class="btn btn-primary" type="button" @click="go(2)">下一步</button>
        </div>
      </fieldset>

      <!-- 第 2 步：schema 动态配置（默认已预填）+ 能力折叠 -->
      <fieldset v-show="step === 2" style="border: none; padding: 0; margin: 0">
        <div class="callout ok" style="margin: 0 0 14px">
          ✓ 配置已预填实测默认值——通常只需确认地址/路径与真实环境一致
        </div>
        <template v-if="adapterName === 'hermes'">
          <div v-if="probingHermes" class="callout" style="margin: 0 0 14px">
            正在探测本机 8640-8649 端口的 hermes 网关…<span class="spin" style="display:inline-block;margin-left:8px"></span>
          </div>
          <div v-else-if="hermesNote" class="callout" :class="hermesCandidates.length ? 'ok' : ''" style="margin: 0 0 14px">
            {{ hermesNote }}
            <button
              v-if="hermesCandidates.length"
              class="btn btn-quiet btn-sm"
              type="button"
              style="margin-left: 10px"
              @click="probeHermes"
            >重新探测</button>
          </div>
        </template>
        <label class="f-label">Adapter 配置</label>
        <DynamicConfigForm v-model="formValues" :fields="schema" />

        <details class="adv" :open="showAdvanced" @toggle="showAdvanced = ($event.target as HTMLDetailsElement).open">
          <summary class="f-label" style="cursor: pointer">高级：能力覆盖（可选，默认沿用 adapter 声明）</summary>
          <div class="f-row" style="margin-top: 6px">
            <div>
              <input v-model="capMaxConcurrency" class="f-input" type="number" placeholder="最大并发（默认 1）" min="1" />
            </div>
            <div>
              <input v-model="capInterruptLevel" class="f-input" type="number" placeholder="插话级别 0-2" min="0" max="2" />
            </div>
            <div>
              <input v-model="capTypicalLatencyS" class="f-input" type="text" placeholder="典型延迟（秒）" />
            </div>
          </div>
          <div class="f-hint">能力只开不关：覆盖值仅能提升（后端 MergeDefaults 忽略关闭动作）。</div>
        </details>

        <div style="display: flex; gap: 10px; margin-top: 18px">
          <button class="btn btn-secondary" type="button" @click="go(1)">上一步</button>
          <button class="btn btn-primary" type="button" @click="go(3)">下一步</button>
        </div>
      </fieldset>

      <!-- 第 3 步：测试 + 提交 -->
      <fieldset v-show="step === 3" style="border: none; padding: 0; margin: 0">
        <div class="f-hint" style="margin-bottom: 10px">
          测试失败也可以提交（agent 将以 offline 保存），但建议先按下方建议排查。
        </div>
        <label class="small" style="display: flex; align-items: center; gap: 6px; font-weight: 600; color: var(--ink-2)">
          <input v-model="sendMessage" type="checkbox" /> 测试时试发一条消息
        </label>

        <div v-if="testResult" class="callout" :class="testResult.ok ? 'ok' : 'bad'" style="margin-top: 12px">
          <template v-if="testResult.ok">✔ 连通性测试通过</template>
          <template v-else>✘ 测试失败</template>
          （{{ testResult.duration_ms }} ms<template v-if="sendMessage"> · 含试发消息</template>）
          <span v-if="testResult.error" class="detail">{{ testResult.error }}</span>
          <span v-if="!testResult.ok && testResult.error" class="detail" style="color: var(--signal-2)">
            排查建议：{{ troubleshooting(testResult.error) || '检查配置项与真实环境是否一致' }}
          </span>
        </div>

        <div v-if="submitOk" class="callout ok" style="margin-top: 12px">
          ✔ 注册成功：{{ submitOk.name }} · 状态 {{ submitOk.status }} ·
          <router-link :to="`/agents/${submitOk.id}`">查看配置页</router-link> 或
          <router-link :to="`/chat?agent=${submitOk.id}`">开始私聊</router-link>
        </div>
        <div v-else-if="submitError" class="callout bad" style="margin-top: 12px">
          ✘ 注册失败<span class="detail">{{ submitError }}</span>
        </div>

        <div style="display: flex; gap: 10px; margin-top: 18px">
          <button class="btn btn-secondary" type="button" @click="go(2)">上一步</button>
          <button class="btn btn-secondary" type="button" :disabled="testing" @click="runTest">
            测试连通性<span v-if="testing" class="spin"></span>
          </button>
          <button class="btn btn-primary" type="button" :disabled="submitting" @click="submit">
            提交注册<span v-if="submitting" class="spin"></span>
          </button>
        </div>
      </fieldset>
    </div>
  </div>
</template>

<style scoped>
.adapter-cards { display: grid; grid-template-columns: repeat(3, 1fr); gap: 10px; }
@media (max-width: 760px) { .adapter-cards { grid-template-columns: 1fr; } }
.adapter-card {
  display: flex; flex-direction: column; gap: 6px; text-align: left; cursor: pointer;
  background: rgba(11, 17, 32, .6); border: 1px solid var(--line-strong); border-radius: var(--r-m);
  padding: 14px 16px; color: var(--ink); transition: border-color .12s, box-shadow .12s, background .12s;
}
.adapter-card:hover { border-color: var(--signal); }
.adapter-card.on { border-color: var(--signal); background: var(--signal-wash); box-shadow: 0 0 0 3px rgba(56, 225, 255, .1); }
.adapter-name { font: 700 14px var(--mono); letter-spacing: .04em; color: var(--signal); }
.adapter-tagline { font-size: 12px; color: var(--ink-2); line-height: 1.5; }
.adapter-ready { font: 600 11px var(--mono); color: var(--ok); }
.adv summary { list-style: none; }
.adv summary::before { content: "▸ "; color: var(--signal); }
.adv[open] summary::before { content: "▾ "; }
.adv summary::-webkit-details-marker { display: none; }
</style>
