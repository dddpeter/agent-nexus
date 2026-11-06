<script setup lang="ts">
// Agent 目录页：遥测统计条 + 搜索 + 行操作（测试/停用/启用/软删）+ SSE 实时刷新。
// 口径与原 SSR 页一致：统计含 removed（总数扣除），表格不含 removed。
import { onMounted, onUnmounted, ref } from 'vue'
import { disableAgent, deleteAgent, enableAgent, testAgent } from '../api/agents'
import type { Agent } from '../api/types'
import { useAgents } from '../stores/agents'
import { fullTime, relTime } from '../utils/format'

const agentsStore = useAgents()

const flash = ref('')
const flashOk = ref(false)
const rowBusy = ref('') // 正在执行行操作的 agent id
const testing = ref('') // 正在测试的 agent id

let pollTimer: ReturnType<typeof setInterval> | null = null

onMounted(() => {
  void agentsStore.load()
  // SSE 联动（App 装配时 store 已订阅 agent.status → 重拉）；此处补 15s 轮询兜底
  pollTimer = setInterval(() => void agentsStore.load(), 15000)
})

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
  pollTimer = null
})

function notify(msg: string, ok: boolean): void {
  flash.value = msg
  flashOk.value = ok
  setTimeout(() => {
    if (flash.value === msg) flash.value = ''
  }, 4000)
}

async function withRow(agent: Agent, fn: () => Promise<void>): Promise<void> {
  rowBusy.value = agent.id
  try {
    await fn()
    await agentsStore.load()
  } catch (e) {
    notify(`操作失败：${e instanceof Error ? e.message : String(e)}`, false)
  } finally {
    rowBusy.value = ''
  }
}

const doTest = (a: Agent) =>
  withRow(a, async () => {
    testing.value = a.id
    try {
      const res = await testAgent(a.id)
      notify(
        res.ok
          ? `✔ ${a.name} 连通性测试通过（${res.duration_ms} ms）`
          : `✘ ${a.name} 测试失败：${res.error ?? '未知错误'}`,
        res.ok,
      )
    } finally {
      testing.value = ''
    }
  })

const doDisable = (a: Agent) => withRow(a, () => disableAgent(a.id))
const doEnable = (a: Agent) => withRow(a, () => enableAgent(a.id))

function doDelete(a: Agent): void {
  if (!confirm(`软删除 ${a.name}？历史会话与消息保留。`)) return
  void withRow(a, () => deleteAgent(a.id))
}
</script>

<template>
  <div>
    <div class="page-head">
      <div>
        <h1>Agent</h1>
        <div class="sub">被纳管的 agent 目录 · 状态由心跳探活自动维护</div>
      </div>
      <router-link class="btn btn-primary" to="/agents/new">注册 Agent</router-link>
    </div>

    <!-- 遥测读数条 -->
    <div class="telemetry">
      <div class="t">
        <div class="n">{{ agentsStore.stats.total }}</div>
        <div class="l">已注册</div>
      </div>
      <div class="t">
        <div class="n" style="color: var(--ok)">{{ agentsStore.stats.online }}</div>
        <div class="l">在线</div>
      </div>
      <div class="t bad">
        <div class="n">{{ agentsStore.stats.offline }}</div>
        <div class="l">离线</div>
      </div>
      <div class="t dim">
        <div class="n">{{ agentsStore.stats.removed }}</div>
        <div class="l">停用 / 已删</div>
      </div>
    </div>

    <div v-if="flash" class="callout" :class="flashOk ? 'ok' : 'bad'" style="margin-bottom: 12px">
      {{ flash }}
    </div>

    <div class="card" style="padding: 6px 20px 10px">
      <div class="tbl-tools">
        <input
          v-model="agentsStore.query"
          class="f-input"
          type="search"
          placeholder="按名称、adapter 或状态过滤"
        />
        <span class="small muted">{{ agentsStore.filtered.length }} / {{ agentsStore.visible.length }}</span>
      </div>

      <table class="tbl">
        <thead>
          <tr>
            <th style="width: 110px">状态</th>
            <th>名称</th>
            <th>Adapter</th>
            <th>能力</th>
            <th style="width: 70px">并发</th>
            <th style="width: 110px">心跳</th>
            <th style="width: 250px"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="a in agentsStore.filtered" :key="a.id" class="agent-row">
            <td>
              <span class="st" :class="`st-${a.status}`">{{ a.status }}</span>
              <span v-if="a.disabled" class="st st-disabled">停用</span>
            </td>
            <td>
              <router-link :to="`/agents/${a.id}`" style="font-weight: 600">{{ a.name }}</router-link>
              <div class="mono muted">{{ a.id }}</div>
            </td>
            <td><span class="chip">{{ a.adapter }}</span></td>
            <td class="small">
              <span v-if="a.capabilities.can_stream" class="chip on">流式</span>
              <span v-if="a.capabilities.can_group_chat" class="chip">群聊</span>
              <span class="chip">插话 L{{ a.capabilities.interrupt_level }}</span>
            </td>
            <td class="mono">{{ a.in_flight }}/{{ a.capabilities.max_concurrency }}</td>
            <td class="small muted" :title="fullTime(a.last_heartbeat_at)">{{ relTime(a.last_heartbeat_at) }}</td>
            <td>
              <div class="row-actions">
                <button class="btn btn-quiet btn-sm" type="button" :disabled="rowBusy === a.id" @click="doTest(a)">
                  测试<span v-if="testing === a.id" class="spin"></span>
                </button>
                <router-link class="btn btn-quiet btn-sm" :to="`/agents/${a.id}`">配置</router-link>
                <button
                  v-if="a.disabled"
                  class="btn btn-quiet btn-sm"
                  type="button"
                  :disabled="rowBusy === a.id"
                  @click="doEnable(a)"
                >
                  启用
                </button>
                <button
                  v-else
                  class="btn btn-quiet btn-sm"
                  type="button"
                  :disabled="rowBusy === a.id"
                  @click="doDisable(a)"
                >
                  停用
                </button>
                <button class="btn btn-danger btn-sm" type="button" :disabled="rowBusy === a.id" @click="doDelete(a)">
                  软删
                </button>
              </div>
            </td>
          </tr>
          <tr v-if="!agentsStore.filtered.length">
            <td colspan="7">
              <div v-if="agentsStore.query" class="empty">
                <div class="big">没有匹配「{{ agentsStore.query }}」的 agent</div>
                <div>换个关键词，或清空过滤条件</div>
              </div>
              <div v-else-if="agentsStore.loading" class="empty">
                <div class="big">加载中…</div>
              </div>
              <div v-else class="empty">
                <div class="big">还没有注册任何 agent</div>
                <div>通过注册向导接入第一个 agent（echod / hermes / picoclaw）</div>
                <router-link class="btn btn-primary" to="/agents/new">注册 Agent</router-link>
              </div>
            </td>
          </tr>
        </tbody>
      </table>

      <div v-if="agentsStore.error" class="callout bad" style="margin: 8px 0">{{ agentsStore.error }}</div>
    </div>
  </div>
</template>
