<script setup lang="ts">
// Agent 详情/配置编辑页（FR1.1b）：密钥脱敏回显、保存前连通性门禁（后端自动测试，
// 失败拒绝可 force）、停用/启用、软删（confirm）、运行信息、最近会话。
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import DynamicConfigForm from '../components/DynamicConfigForm.vue'
import {
  buildConfigPayload,
  disableAgent,
  enableAgent,
  deleteAgent,
  getAgent,
  initialFormValues,
  listAdapters,
  testAgent,
  updateAgent,
} from '../api/agents'
import { agentSessions } from '../api/sessions'
import type { Agent, SchemaField, Session, TestResult } from '../api/types'
import { ApiError } from '../api/client'
import { fullTime, initial, relTime } from '../utils/format'

const route = useRoute()
const router = useRouter()

const agentId = computed(() => String(route.params.id))
const agent = ref<Agent | null>(null)
const schema = ref<SchemaField[]>([])
const sessions = ref<Session[]>([])
const loadError = ref('')

const formValues = ref<Record<string, string | boolean>>({})
const capMaxConcurrency = ref('')
const capInterruptLevel = ref('')
const capCanStream = ref(true)
const force = ref(false)

const saving = ref(false)
const testing = ref(false)
const opBusy = ref(false)
const testResult = ref<TestResult | null>(null)
const saveOk = ref(false)
const saveProbeFailed = ref(false)
const saveError = ref('')

async function reload(): Promise<void> {
  const [a, adapters] = await Promise.all([getAgent(agentId.value), listAdapters()])
  agent.value = a
  schema.value = adapters.find((x) => x.name === a.adapter)?.config_schema.fields ?? []
  formValues.value = initialFormValues(schema.value, a.config)
  capMaxConcurrency.value = String(a.capabilities.max_concurrency)
  capInterruptLevel.value = String(a.capabilities.interrupt_level)
  capCanStream.value = a.capabilities.can_stream
  try {
    sessions.value = await agentSessions(a.id)
  } catch {
    sessions.value = []
  }
}

onMounted(async () => {
  try {
    await reload()
  } catch (e) {
    loadError.value = e instanceof ApiError ? e.message : e instanceof Error ? e.message : String(e)
  }
})

function capabilitiesPayload(): Record<string, unknown> {
  const mc = Number.parseInt(capMaxConcurrency.value, 10)
  const il = Number.parseInt(capInterruptLevel.value, 10)
  return {
    can_group_chat: agent.value?.capabilities.can_group_chat ?? false,
    can_stream: capCanStream.value,
    max_concurrency: Number.isFinite(mc) && mc > 0 ? mc : 1,
    typical_latency: agent.value?.capabilities.typical_latency ?? '0s',
    interrupt_level: Number.isFinite(il) ? Math.max(0, Math.min(2, il)) : 0,
  }
}

async function save(): Promise<void> {
  if (!agent.value || saving.value) return
  saving.value = true
  saveOk.value = false
  saveError.value = ''
  saveProbeFailed.value = false
  try {
    const updated = await updateAgent(agent.value.id, {
      config: buildConfigPayload(schema.value, formValues.value),
      capabilities: capabilitiesPayload() as never,
      force: force.value,
    })
    agent.value = updated
    formValues.value = initialFormValues(schema.value, updated.config)
    saveOk.value = true
    saveProbeFailed.value = updated.status === 'offline'
  } catch (e) {
    saveError.value = e instanceof ApiError ? e.message : e instanceof Error ? e.message : String(e)
  } finally {
    saving.value = false
  }
}

async function runTest(): Promise<void> {
  if (!agent.value || testing.value) return
  testing.value = true
  testResult.value = null
  try {
    testResult.value = await testAgent(agent.value.id)
  } catch (e) {
    testResult.value = { ok: false, duration_ms: 0, error: e instanceof Error ? e.message : String(e) }
  } finally {
    testing.value = false
  }
}

async function toggleDisabled(): Promise<void> {
  if (!agent.value || opBusy.value) return
  opBusy.value = true
  try {
    if (agent.value.disabled) await enableAgent(agent.value.id)
    else await disableAgent(agent.value.id)
    agent.value = await getAgent(agent.value.id)
  } finally {
    opBusy.value = false
  }
}

async function remove(): Promise<void> {
  if (!agent.value) return
  if (!confirm(`软删除 ${agent.value.name}？历史会话与消息将保留。`)) return
  opBusy.value = true
  try {
    await deleteAgent(agent.value.id)
    void router.push('/')
  } finally {
    opBusy.value = false
  }
}
</script>

<template>
  <div>
    <div v-if="loadError" class="callout bad">{{ loadError }}</div>

    <template v-if="agent">
      <div class="page-head">
        <div class="detail-head">
          <div class="avatar">{{ initial(agent.name) }}</div>
          <div>
            <h1 style="display: flex; align-items: center; gap: 10px">
              {{ agent.name }}
              <span class="st" :class="`st-${agent.status}`">{{ agent.status }}</span>
              <span v-if="agent.disabled" class="st st-disabled">停用</span>
            </h1>
            <div class="sub mono">{{ agent.id }} · {{ agent.adapter }}</div>
          </div>
        </div>
        <div class="row-actions">
          <button class="btn btn-secondary" type="button" :disabled="opBusy" @click="toggleDisabled">
            {{ agent.disabled ? '启用' : '停用' }}
          </button>
          <button class="btn btn-secondary" type="button" style="color: var(--bad); border-color: #e8c2c2" :disabled="opBusy" @click="remove">
            软删除
          </button>
          <router-link class="btn btn-primary" :to="`/chat?agent=${agent.id}`">发起私聊</router-link>
        </div>
      </div>

      <div class="grid" style="grid-template-columns: 1fr 340px; align-items: start">
        <div>
          <div class="card">
            <h2 class="card-title">
              配置
              <span class="small muted" style="font-weight: 400">密钥字段回显脱敏 · 留空表示不修改</span>
            </h2>

            <div v-if="saveOk" class="callout ok">
              ✔ 已保存<template v-if="saveProbeFailed">（探活失败，agent 置为 offline 待恢复）</template>
            </div>
            <div v-else-if="saveError" class="callout bad">✘ 保存被拒<span class="detail">{{ saveError }}</span></div>

            <form @submit.prevent="save">
              <DynamicConfigForm v-model="formValues" :fields="schema" :current="agent.config" />

              <h2 class="card-title" style="margin-top: 22px">能力</h2>
              <div class="f-row">
                <div>
                  <label class="f-label">最大并发</label>
                  <input v-model="capMaxConcurrency" class="f-input" type="number" min="1" />
                </div>
                <div>
                  <label class="f-label">插话级别（0-2）</label>
                  <input v-model="capInterruptLevel" class="f-input" type="number" min="0" max="2" />
                </div>
                <div>
                  <label class="f-label">可流式</label>
                  <!-- 能力只开不关（后端 MergeDefaults 只合并 non-zero）：已开启的流式锁死，防静默无效修改 -->
                  <select v-model="capCanStream" class="f-input" :disabled="agent.capabilities.can_stream">
                    <option :value="true">是</option>
                    <option :value="false">否</option>
                  </select>
                </div>
              </div>
              <div class="f-hint">
                变更保存前自动做连通性测试；探活失败默认拒绝（坏配置不生效）。
                能力只能开启或提升、不能关闭：已开启的「可流式」与降回 0 的插话级别会被后端忽略，关闭需软删后重建。
              </div>

              <div style="display: flex; align-items: center; gap: 14px; margin-top: 18px">
                <button class="btn btn-primary" type="submit" :disabled="saving">
                  保存变更<span v-if="saving" class="spin"></span>
                </button>
                <label class="small muted" style="display: flex; align-items: center; gap: 6px; margin: 0">
                  <input v-model="force" type="checkbox" /> 强制保存（跳过探活拦截）
                </label>
              </div>
            </form>
          </div>
        </div>

        <div>
          <div class="card tight">
            <h2 class="card-title">
              运行信息
              <button class="btn btn-secondary btn-sm" type="button" :disabled="testing" @click="runTest">
                测试连通性<span v-if="testing" class="spin"></span>
              </button>
            </h2>
            <dl class="kv" style="padding: 4px 2px 8px">
              <dt>心跳</dt>
              <dd class="muted" :title="fullTime(agent.last_heartbeat_at)">{{ relTime(agent.last_heartbeat_at) }}</dd>
              <dt>并发占用</dt>
              <dd class="mono">{{ agent.in_flight }} / {{ agent.capabilities.max_concurrency }}</dd>
              <dt>典型延迟</dt>
              <dd>{{ agent.capabilities.typical_latency }}</dd>
              <dt>可群聊</dt>
              <dd>{{ agent.capabilities.can_group_chat ? '是' : '否' }}</dd>
            </dl>
            <div v-if="testResult" class="callout" :class="testResult.ok ? 'ok' : 'bad'" style="margin-top: 8px">
              {{ testResult.ok ? '✔ 连通性测试通过' : '✘ 测试失败' }}（{{ testResult.duration_ms }} ms）
              <span v-if="testResult.error" class="detail">{{ testResult.error }}</span>
            </div>
          </div>

          <div class="card tight" style="margin-top: 16px">
            <h2 class="card-title">最近会话</h2>
            <div v-if="sessions.length" class="sess-list" style="padding: 2px">
              <router-link v-for="s in sessions" :key="s.id" :to="`/chat?session=${s.id}`">
                <span class="mono">{{ s.id }}</span>
                <span class="small muted">{{ relTime(s.created_at) }}</span>
              </router-link>
            </div>
            <div v-else class="small muted" style="padding: 8px 2px">还没有会话</div>
          </div>
        </div>
      </div>
    </template>
  </div>
</template>
