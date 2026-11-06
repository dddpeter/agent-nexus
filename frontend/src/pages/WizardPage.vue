<script setup lang="ts">
// 注册向导（FR1.1 三步）：选类型 → 配置与能力 → 测试并提交。
// 测试走 POST /api/agents/test（不落库）；提交走 POST /api/agents（探活通过自动 online）。
import { computed, onMounted, ref } from 'vue'
import DynamicConfigForm from '../components/DynamicConfigForm.vue'
import { buildConfigPayload, initialFormValues, listAdapters, registerAgent, testSpec } from '../api/agents'
import type { AdapterInfo, Agent, Capabilities, TestResult } from '../api/types'
import { ApiError } from '../api/client'

const step = ref(1)
const adapters = ref<AdapterInfo[]>([])
const loadError = ref('')

const name = ref('')
const adapterName = ref('')
const formValues = ref<Record<string, string | boolean>>({})

// 能力可选覆盖
const capMaxConcurrency = ref('')
const capInterruptLevel = ref('')
const capTypicalLatencyS = ref('')

const sendMessage = ref(false)
const testing = ref(false)
const submitting = ref(false)
const testResult = ref<TestResult | null>(null)
const submitOk = ref<Agent | null>(null)
const submitError = ref('')

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
}

function go(n: number): void {
  step.value = n
}

function stepClass(n: number): Record<string, boolean> {
  return { on: step.value === n, done: step.value > n }
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
  if (!name.value.trim()) {
    submitError.value = 'Agent 名字必填'
    go(1)
    return
  }
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
        <div class="sub">三步接入：选类型 → 配置 → 测试并提交</div>
      </div>
    </div>

    <div class="steps">
      <div v-for="n in 3" :key="n" class="step" :class="stepClass(n)">
        <span class="dot">{{ step > n ? '✓' : n }}</span>
        {{ ['选择类型', '配置与能力', '测试并提交'][n - 1] }}
      </div>
    </div>

    <div class="card">
      <div v-if="loadError" class="callout bad">{{ loadError }}</div>

      <!-- 第 1 步：名字 + adapter -->
      <fieldset v-show="step === 1" style="border: none; padding: 0; margin: 0">
        <label class="f-label">Agent 名字 <span class="f-req">*</span></label>
        <input v-model="name" class="f-input" type="text" placeholder="如 echod-main、hermes-coder" required />

        <label class="f-label">Adapter 类型 <span class="f-req">*</span></label>
        <select class="f-input" :value="adapterName" @change="pickAdapter(($event.target as HTMLSelectElement).value)">
          <option v-for="a in adapters" :key="a.name" :value="a.name">{{ a.name }}</option>
        </select>
        <div class="f-hint">hermes = HTTP 型范式 · picoclaw = CLI 型范式 · echod = 内置测试假 agent</div>
        <div style="margin-top: 18px">
          <button class="btn btn-primary" type="button" @click="go(2)">下一步</button>
        </div>
      </fieldset>

      <!-- 第 2 步：schema 动态配置 + 能力覆盖 -->
      <fieldset v-show="step === 2" style="border: none; padding: 0; margin: 0">
        <label class="f-label">Adapter 配置</label>
        <DynamicConfigForm v-model="formValues" :fields="schema" />

        <label class="f-label">能力（可选覆盖）</label>
        <div class="f-row">
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
        <div class="f-hint">未填项沿用 adapter 默认能力。</div>
        <div style="display: flex; gap: 10px; margin-top: 18px">
          <button class="btn btn-secondary" type="button" @click="go(1)">上一步</button>
          <button class="btn btn-primary" type="button" @click="go(3)">下一步</button>
        </div>
      </fieldset>

      <!-- 第 3 步：测试 + 提交 -->
      <fieldset v-show="step === 3" style="border: none; padding: 0; margin: 0">
        <div class="f-hint" style="margin-bottom: 10px">
          提交前先测试连通性（探活 + 可选试发）；测试失败仍可提交，agent 将以 offline 保存。
        </div>
        <label class="small" style="display: flex; align-items: center; gap: 6px; font-weight: 600; color: var(--ink-2)">
          <input v-model="sendMessage" type="checkbox" /> 测试时试发一条消息
        </label>

        <div v-if="testResult" class="callout" :class="testResult.ok ? 'ok' : 'bad'" style="margin-top: 12px">
          <template v-if="testResult.ok">✔ 连通性测试通过</template>
          <template v-else>✘ 测试失败</template>
          （{{ testResult.duration_ms }} ms<template v-if="sendMessage"> · 含试发消息</template>）
          <span v-if="testResult.error" class="detail">{{ testResult.error }}</span>
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
