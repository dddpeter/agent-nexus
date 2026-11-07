<script setup lang="ts">
// 私聊页（FR2）：跨 agent 最近会话侧栏 + 气泡历史 + 全局 SSE 实时刷新 + 同步/流式发送。
// 流式 = fetch 读 SSE 分帧渐进渲染；final 后重拉历史对齐（服务端已落库）。
// 兜底：SSE 断线由 EventSource 自动重连（Last-Event-ID 服务端补偿重放），另加 30s 轮询。
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
// MateChat 按需子路径引入（全入口会连带 MarkdownCard → mermaid 可选依赖）
import { McBubble } from '@matechat/core/Bubble'
import { McInput } from '@matechat/core/Input'
import '@matechat/core/Bubble/index.css'
import '@matechat/core/Input/index.css'
import { createSession, getSession, recentSessions, sendSync as sendSyncApi, sessionMessages, streamSend } from '../api/sessions'
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
const streamingId = ref<number | null>(null) // 流式中乐观气泡 id（首帧 delta 前走 McBubble loading 态）

const historyEl = ref<HTMLElement | null>(null)
/** 打开/切换会话后的首次拉取强制吸底；此后仅在用户本就在底部时跟随。 */
let jumpToBottom = true

const offs: Array<() => void> = []
let pollTimer: ReturnType<typeof setInterval> | null = null

const canStream = computed(() => agent.value?.capabilities.can_stream ?? false)
/** agent 是否支持同步发送（disable_sync 声明 = 仅流式）；缺省视为支持。 */
const canSync = computed(() => !(agent.value?.capabilities.disable_sync ?? false))

function agentName(id: string): string {
  return agentsStore.byId(id)?.name ?? id
}

/** McBubble 头像与 top 插槽文案：你 / agent 名 / 系统。 */
function senderLabel(m: Message): string {
  if (m.sender_type === 'human') return '你'
  if (m.sender_type === 'agent') return agentName(m.sender_id)
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
  scrollToBottom(jumpToBottom)
  jumpToBottom = false
}

/** 滚动跟随：仅在用户本就停留在底部附近时吸底，上翻阅读不被拽回。 */
function scrollToBottom(force = false): void {
  const el = historyEl.value
  if (!el) return
  const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80
  if (force || nearBottom) el.scrollTop = el.scrollHeight
}

async function openSession(id: string): Promise<void> {
  jumpToBottom = true
  try {
    session.value = await getSession(id)
  } catch (e) {
    // 会话打开失败（限流 429/网络抖动）不得静默成空态：给出可感知的提示
    session.value = null
    notifyError(`会话加载失败：${e instanceof Error ? e.message : String(e)}`)
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

  // SSE：本会话消息 → 重拉；任意会话消息 → 侧栏更新。
  // 流式进行中挂起历史重拉：后端在 delta 前就落库发布 msg.human，此时整表替换
  // 会把乐观气泡（含流式占位）移出数组，delta 渐进渲染随之失效——对齐留到流结束。
  for (const t of ['msg.human', 'msg.agent', 'msg.system'] as const) {
    offs.push(
      events.on(t, (payload: { session_id?: string } | null) => {
        if (payload?.session_id === session.value?.id && !streaming.value) void loadMessages()
        void loadSessions()
      }),
    )
  }
  pollTimer = setInterval(() => {
    void loadSessions()
    if (session.value && !streaming.value) void loadMessages()
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

async function sendSync(content: string): Promise<void> {
  const text = content.trim()
  if (!text || !session.value || sending.value) return
  sending.value = true
  try {
    const res = await sendSyncApi(session.value.id, text)
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

/** 发送入口：按 agent 能力自动路由——可流式优先流式（渐进渲染），否则退回同步。
 *  发送模式是 agent 的属性，不作为用户的选择。 */
function onSubmit(content: string): void {
  const text = content.trim()
  if (!text || !session.value) return
  input.value = ''
  if (canStream.value) void sendStream(text)
  else if (canSync.value) void sendSync(text)
  else notifyError('该 agent 暂无可用发送方式')
}

/** 流式发送：乐观插入 human 气泡 + agent 气泡（首帧前 loading 态），delta 渐进追加。 */
async function sendStream(content: string): Promise<void> {
  if (!content || !session.value || streaming.value) return
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
  streamingId.value = streamMsg.id
  messages.value = [...messages.value, optimistic, streamMsg]
  void nextTick(() => scrollToBottom(true))

  try {
    await streamSend(session.value.id, content, (frame) => {
      if (frame.type === 'delta' || frame.type === 'final') {
        streamMsg.content += frame.data
        messages.value = [...messages.value] // 触发响应式
        void nextTick(() => scrollToBottom())
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
    streamingId.value = null
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

    <div v-if="flash" class="callout bad" style="margin-bottom: 12px">{{ flash }}</div>

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
              <div style="display: flex; flex-direction: column; gap: 10px; padding: 8px 0">
                <template v-for="m in messages" :key="m.id">
                  <div v-if="m.sender_type === 'system'" class="bubble system">{{ m.content }}</div>
                  <McBubble
                    v-else
                    :content="m.content"
                    :align="m.sender_type === 'human' ? 'right' : 'left'"
                    :loading="streaming && m.id === streamingId && !m.content"
                  >
                    <template #avatar>
                      <span class="chat-avatar" :class="m.sender_type === 'human' ? 'is-human' : 'is-agent'">
                        {{ senderLabel(m).charAt(0) }}
                      </span>
                    </template>
                    <template #top>
                      <span class="msg-meta">{{ senderLabel(m) }} · {{ clockTime(m.created_at) }}</span>
                    </template>
                  </McBubble>
                </template>
                <div v-if="!messages.length" class="chat-empty">还没有消息，说点什么吧</div>
              </div>
            </div>

            <div class="chat-footer">
              <McInput
                :value="input"
                placeholder="输入消息，回车发送…"
                :max-length="2000"
                show-count
                @change="(e: string) => (input = e)"
                @submit="onSubmit"
              />
            </div>
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

<style scoped>
/* McBubble content 默认不保留换行；agent 多行回复（含 [error]/[网络] 追加行）需 pre-wrap。 */
.chat-history :deep(.mc-bubble-content) {
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  word-break: normal;
}
.msg-meta {
  font-size: 11px;
  color: var(--ink-4);
  font-variant-numeric: tabular-nums;
}
/* 与详情页 .avatar 同族：mono 首字方块，agent 电青 / human 灰 */
.chat-avatar {
  width: 38px;
  height: 38px;
  border-radius: var(--r-m);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font: 700 16px var(--mono);
  flex: none;
}
.chat-avatar.is-agent {
  color: var(--signal);
  background: var(--signal-wash);
  border: 1.5px solid rgba(56, 225, 255, 0.45);
  box-shadow: 0 0 10px rgba(56, 225, 255, 0.18);
}
.chat-avatar.is-human {
  color: var(--ink);
  background: var(--paper-3);
  border: 1.5px solid var(--line-strong);
}
.chat-footer {
  padding: 10px 12px;
  background: var(--paper);
  border: 1px solid var(--line-strong);
  border-radius: 0 0 var(--r-m) var(--r-m);
}
</style>
