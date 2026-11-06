<script setup lang="ts">
// 私聊页（FR2）：跨 agent 最近会话侧栏 + 气泡历史 + 全局 SSE 实时刷新 + 同步/流式发送。
// 流式 = fetch 读 SSE 分帧渐进渲染；final 后重拉历史对齐（服务端已落库）。
// 兜底：SSE 断线由 EventSource 自动重连（Last-Event-ID 服务端补偿重放），另加 30s 轮询。
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { createSession, getSession, recentSessions, sendSync, sessionMessages, streamSend } from '../api/sessions'
import { getAgent } from '../api/agents'
import type { Agent, Message, Session } from '../api/types'
import { useAgents } from '../stores/agents'
import { useEventStream } from '../stores/events'
import { clockTime } from '../utils/format'

const route = useRoute()
const router = useRouter()
const agentsStore = useAgents()
const events = useEventStream()

const sessions = ref<Session[]>([])
const session = ref<Session | null>(null)
const agent = ref<Agent | null>(null)
const messages = ref<Message[]>([])
const input = ref('')
const flash = ref('')

const sending = ref(false)
const streaming = ref(false)

const historyEl = ref<HTMLElement | null>(null)

const offs: Array<() => void> = []
let pollTimer: ReturnType<typeof setInterval> | null = null

const canStream = computed(() => agent.value?.capabilities.can_stream ?? false)

function agentName(id: string): string {
  return agentsStore.byId(id)?.name ?? id
}

function bubbleClass(senderType: string): string {
  if (senderType === 'human') return 'human'
  if (senderType === 'agent') return 'agent'
  return 'system'
}

function bubbleLabel(senderType: string): string {
  if (senderType === 'human') return '你'
  if (senderType === 'agent') return 'Agent'
  return '系统'
}

async function loadSessions(): Promise<void> {
  try {
    sessions.value = await recentSessions(20)
  } catch {
    sessions.value = []
  }
}

async function loadMessages(): Promise<void> {
  if (!session.value) return
  try {
    messages.value = await sessionMessages(session.value.id)
  } catch {
    /* 会话可能刚被清理 */
  }
  await nextTick()
  const el = historyEl.value
  if (el) el.scrollTop = el.scrollHeight
}

async function openSession(id: string): Promise<void> {
  try {
    session.value = await getSession(id)
  } catch {
    session.value = null
    return
  }
  const aid = session.value?.agent_id ?? ''
  if (!aid) {
    agent.value = null
  } else {
    // 优先本地目录；软删后 Registry.Get 失败 → 占位展示（历史会话仍可读，等价原 SSR 行为）
    agent.value =
      agentsStore.byId(aid) ??
      (await getAgent(aid).catch(() => null)) ?? {
        id: aid,
        name: aid,
        adapter: '',
        status: 'removed',
        config: {},
        capabilities: {
          can_group_chat: false,
          can_stream: false,
          max_concurrency: 0,
          typical_latency: '0s',
          interrupt_level: 0,
        },
        last_heartbeat_at: null,
        in_flight: 0,
        disabled: false,
      }
  }
  await loadMessages()
}

/** 为 agent 建会话并进入。 */
async function startWith(a: Agent): Promise<void> {
  try {
    const sess = await createSession(a.id)
    sessions.value = [sess, ...sessions.value]
    await router.replace({ query: { session: sess.id } })
  } catch (e) {
    flash.value = `建会话失败：${e instanceof Error ? e.message : String(e)}`
    setTimeout(() => (flash.value = ''), 4000)
  }
}

// 路由查询参数驱动（?session= / ?agent=，等价原 303 重定向语义）；immediate 覆盖首屏
watch(
  () => route.query,
  async (q) => {
    const sid = typeof q.session === 'string' ? q.session : ''
    const aid = typeof q.agent === 'string' ? q.agent : ''
    if (sid) {
      await openSession(sid)
    } else if (aid) {
      const a = agentsStore.byId(aid) ?? (await getAgent(aid).catch(() => null))
      if (a) await startWith(a)
    } else {
      session.value = null
      agent.value = null
      messages.value = []
    }
  },
  { immediate: true },
)

onMounted(async () => {
  if (!agentsStore.loaded) void agentsStore.load()
  void loadSessions()

  // SSE：本会话消息 → 重拉；任意会话消息 → 侧栏更新
  for (const t of ['msg.human', 'msg.agent', 'msg.system'] as const) {
    offs.push(
      events.on(t, (payload: { session_id?: string } | null) => {
        if (payload?.session_id === session.value?.id) void loadMessages()
        void loadSessions()
      }),
    )
  }
  pollTimer = setInterval(() => {
    void loadSessions()
    if (session.value) void loadMessages()
  }, 30000)
})

onUnmounted(() => {
  offs.forEach((fn) => fn())
  if (pollTimer) clearInterval(pollTimer)
})

function notifyError(msg: string): void {
  flash.value = msg
  setTimeout(() => (flash.value = ''), 6000)
}

async function sendSyncMsg(): Promise<void> {
  const content = input.value.trim()
  if (!content || !session.value || sending.value) return
  input.value = ''
  sending.value = true
  try {
    const res = await sendSync(session.value.id, content)
    if (res.timed_out) notifyError('回复超时：已留占位消息，会话保留可重发')
    await loadMessages()
    void loadSessions()
  } catch (e) {
    notifyError(`发送失败：${e instanceof Error ? e.message : String(e)}`)
    await loadMessages()
  } finally {
    sending.value = false
  }
}

/** 流式发送：乐观插入 human 气泡 + typing 气泡，delta 渐进追加。 */
async function sendStreamMsg(): Promise<void> {
  const content = input.value.trim()
  if (!content || !session.value || streaming.value) return
  input.value = ''
  streaming.value = true

  const optimistic: Message = {
    id: -Date.now(),
    sender_type: 'human',
    sender_id: 'browser',
    content,
    created_at: new Date().toISOString(),
  }
  const streamMsg: Message = {
    id: -Date.now() - 1,
    sender_type: 'agent',
    sender_id: agent.value?.id ?? '',
    content: '',
    created_at: new Date().toISOString(),
  }
  messages.value = [...messages.value, optimistic, streamMsg]
  void nextTick(() => {
    const el = historyEl.value
    if (el) el.scrollTop = el.scrollHeight
  })

  try {
    await streamSend(session.value.id, content, (frame) => {
      if (frame.type === 'delta' || frame.type === 'final') {
        streamMsg.content += frame.data
        messages.value = [...messages.value] // 触发响应式
      } else if (frame.type === 'error') {
        streamMsg.content += (streamMsg.content ? '\n' : '') + `[error] ${frame.data}`
        messages.value = [...messages.value]
      }
    })
    await loadMessages() // final 已落库，从服务端对齐
    void loadSessions()
  } catch (e) {
    streamMsg.content += (streamMsg.content ? '\n' : '') + `[网络] ${e instanceof Error ? e.message : String(e)}`
    messages.value = [...messages.value]
  } finally {
    streaming.value = false
  }
}
</script>

<template>
  <div>
    <div class="page-head">
      <div>
        <h1>私聊</h1>
        <div class="sub">与单个 agent 的 1v1 会话 · 回复经事件总线实时推送</div>
      </div>
    </div>

    <div class="chat-layout">
      <!-- 侧栏：会话列表 / agent 选择 -->
      <div class="card tight">
        <h2 class="card-title">
          会话
          <a class="small" href="/chat" style="font-weight: 500" @click.prevent="router.replace({ query: {} })">＋ 新会话</a>
        </h2>

        <div v-if="sessions.length" class="sess-list" style="padding: 2px">
          <a
            v-for="s in sessions"
            :key="s.id"
            href="#"
            :class="{ on: s.id === session?.id }"
            @click.prevent="router.replace({ query: { session: s.id } })"
          >
            <span style="font-weight: 600">{{ agentName(s.agent_id) }}</span>
            <span class="small muted">{{ clockTime(s.created_at) }}</span>
          </a>
        </div>

        <div v-if="!session" style="padding: 6px 4px 2px">
          <template v-if="agentsStore.visible.length">
            <div class="f-hint" style="margin: 0 0 8px">选择 agent 开始对话：</div>
            <button
              v-for="a in agentsStore.visible"
              :key="a.id"
              class="btn btn-secondary"
              type="button"
              style="width: 100%; justify-content: space-between; margin-bottom: 6px"
              @click="startWith(a)"
            >
              <span>{{ a.name }}</span>
              <span class="st" :class="`st-${a.status}`" style="padding: 0 6px">{{ a.status }}</span>
            </button>
          </template>
          <div v-else class="empty" style="padding: 28px 8px">
            <div class="big">还没有可用 agent</div>
            <router-link class="btn btn-primary" to="/agents/new">注册 Agent</router-link>
          </div>
        </div>
        <div v-else class="small muted" style="padding: 6px 8px">「＋ 新会话」可另选 agent</div>
      </div>

      <!-- 聊天面板 -->
      <div v-if="session && agent">
        <div class="card" style="padding: 0; overflow: hidden">
          <div style="display: flex; justify-content: space-between; align-items: center; padding: 12px 16px; border-bottom: 1px solid var(--line)">
            <div style="display: flex; align-items: center; gap: 8px">
              <b>{{ agentName(agent.id) }}</b>
              <span class="st" :class="`st-${agent.status}`">{{ agent.status }}</span>
            </div>
            <span class="mono small muted">{{ session.id }}</span>
          </div>

          <div class="chat-box">
            <div ref="historyEl" class="chat-history">
              <div v-if="flash" class="callout bad" style="margin: 8px 12px">{{ flash }}</div>
              <div style="display: flex; flex-direction: column; padding: 4px 0">
                <div v-for="m in messages" :key="m.id" class="bubble" :class="bubbleClass(m.sender_type)">
                  <span class="who">{{ bubbleLabel(m.sender_type) }} · {{ clockTime(m.created_at) }}</span>{{ m.content }}
                </div>
                <div v-if="!messages.length" class="chat-empty">还没有消息，说点什么吧</div>
              </div>
            </div>

            <form class="chat-input-bar" @submit.prevent="sendSyncMsg">
              <input
                v-model="input"
                class="f-input"
                type="text"
                placeholder="输入消息，回车发送…"
                autocomplete="off"
                required
              />
              <button v-if="canStream" class="btn btn-secondary" type="button" :disabled="streaming" @click="sendStreamMsg">
                {{ streaming ? '生成中…' : '流式' }}
              </button>
              <button class="btn btn-primary" type="submit" :disabled="sending">发送</button>
            </form>
          </div>
        </div>
      </div>

      <div v-else>
        <div class="card" style="height: 560px; display: flex; align-items: center; justify-content: center">
          <div class="chat-empty">
            <div class="big" style="font-size: 15px; font-weight: 600; color: var(--ink-2); margin-bottom: 4px">
              从左侧选择一个 agent
            </div>
            开始你的第一个私聊会话
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
