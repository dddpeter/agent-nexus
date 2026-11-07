<script setup lang="ts">
// 群聊页（M3 / FR5.3）：群列表 + 建群 + 群聊天视图（仲裁可视化：Deferred 存档消息高亮、
// 冻结/预算运行态徽标）。人发言同步返回路由 agent 的回复。
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { McBubble } from '@matechat/core/Bubble'
import { McInput } from '@matechat/core/Input'
import '@matechat/core/Bubble/index.css'
import '@matechat/core/Input/index.css'
import { createGroup, getGroup, groupMessages, groupSend, groupUnfreeze, groupTopic, listGroups } from '../api/groups'
import type { Group, GroupState, Message } from '../api/types'
import { useAgents } from '../stores/agents'
import { useEventStream } from '../stores/events'
import { clockTime } from '../utils/format'

const route = useRoute()
const router = useRouter()
const agentsStore = useAgents()
const events = useEventStream()

const groups = ref<Group[]>([])
const group = ref<Group | null>(null)
const state = ref<GroupState | null>(null)
const messages = ref<Message[]>([])
const input = ref('')
const flash = ref('')
const sending = ref(false)
const historyEl = ref<HTMLElement | null>(null)
let jumpToBottom = true

// 建群表单
const showCreate = ref(false)
const newName = ref('')
const newMaxTurns = ref('2')
const newSilent = ref('4')
const picked = ref<Record<string, boolean>>({})
const creating = ref(false)

const agentMembers = computed(() => group.value?.members.filter((m) => m.type === 'agent') ?? [])

function memberName(id: string): string {
  const m = group.value?.members.find((x) => x.id === id)
  if (m?.name) return m.name
  return agentsStore.byId(id)?.name ?? id
}

async function loadGroups(): Promise<void> {
  try {
    groups.value = await listGroups()
  } catch {
    groups.value = []
  }
}

async function openGroup(id: string): Promise<void> {
  jumpToBottom = true
  try {
    const detail = await getGroup(id)
    group.value = detail.group
    state.value = detail.state
  } catch (e) {
    group.value = null
    notify(`群加载失败：${e instanceof Error ? e.message : String(e)}`)
    return
  }
  await loadMessages()
}

async function loadMessages(): Promise<void> {
  if (!group.value) return
  try {
    messages.value = await groupMessages(group.value.id)
  } catch {
    /* 群可能刚被清理 */
  }
  await nextTick()
  const el = historyEl.value
  if (el) {
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80
    if (jumpToBottom || nearBottom) el.scrollTop = el.scrollHeight
    jumpToBottom = false
  }
}

watch(
  () => route.query,
  async (q) => {
    const gid = typeof q.group === 'string' ? q.group : ''
    if (gid) await openGroup(gid)
    else {
      group.value = null
      state.value = null
      messages.value = []
    }
  },
  { immediate: true },
)

let pollTimer: ReturnType<typeof setInterval> | null = null
const offs: Array<() => void> = []
onMounted(async () => {
  if (!agentsStore.loaded) void agentsStore.load()
  void loadGroups()
  // 协调器发布的 msg.* 负载带 group_id（group.publishMessage），精确刷新当前群
  for (const t of ['msg.human', 'msg.agent', 'msg.system'] as const) {
    offs.push(
      events.on(t, (payload: { group_id?: string } | null) => {
        void loadGroups()
        if (group.value && payload?.group_id === group.value.id) void loadMessages()
      }),
    )
  }
  pollTimer = setInterval(() => {
    void loadGroups()
    if (group.value) void refreshState()
  }, 15000)
})
onUnmounted(() => {
  offs.forEach((fn) => fn())
  if (pollTimer) clearInterval(pollTimer)
})

async function refreshState(): Promise<void> {
  if (!group.value) return
  try {
    const detail = await getGroup(group.value.id)
    state.value = detail.state
  } catch {
    /* 忽略 */
  }
}

function notify(msg: string): void {
  flash.value = msg
  setTimeout(() => {
    if (flash.value === msg) flash.value = ''
  }, 6000)
}

async function create(): Promise<void> {
  if (!newName.value.trim() || creating.value) return
  const members = Object.entries(picked.value)
    .filter(([, on]) => on)
    .map(([id]) => ({ id, type: 'agent', trigger: 'any', priority: 1, profile: '' }))
  if (!members.length) {
    notify('至少选择一个 agent 成员')
    return
  }
  creating.value = true
  try {
    const g = await createGroup({
      name: newName.value.trim(),
      members,
      max_turns_per_topic: Number.parseInt(newMaxTurns.value, 10) || undefined,
      human_silent_threshold: Number.parseInt(newSilent.value, 10) || undefined,
    })
    showCreate.value = false
    newName.value = ''
    picked.value = {}
    await loadGroups()
    await router.replace({ query: { group: g.id } })
  } catch (e) {
    notify(`建群失败：${e instanceof Error ? e.message : String(e)}`)
  } finally {
    creating.value = false
  }
}

async function send(): Promise<void> {
  const text = input.value.trim()
  if (!text || !group.value || sending.value) return
  input.value = ''
  sending.value = true
  try {
    const res = await groupSend(group.value.id, text)
    if (res.deferred?.length) {
      notify(`仲裁存档 ${res.deferred.length} 条候选：${res.deferred.map((d) => `${memberName(d.agent_id)}（${d.reason}）`).join('、')}`)
    }
    await loadMessages()
    void refreshState()
  } catch (e) {
    notify(`发送失败：${e instanceof Error ? e.message : String(e)}`)
    await loadMessages()
  } finally {
    sending.value = false
  }
}

function onSubmit(): void {
  void send()
}

function insertAt(m: { name?: string; id: string }): void {
  input.value = `${input.value}@${m.name ?? m.id} `.trimStart()
}

async function doUnfreeze(): Promise<void> {
  if (!group.value) return
  try {
    await groupUnfreeze(group.value.id)
    await loadMessages()
    void refreshState()
  } catch (e) {
    notify(`解冻失败：${e instanceof Error ? e.message : String(e)}`)
  }
}

async function doTopic(): Promise<void> {
  if (!group.value) return
  const topic = window.prompt('新话题 ID（预算将归零）')
  if (!topic) return
  try {
    await groupTopic(group.value.id, topic)
    await loadMessages()
    void refreshState()
  } catch (e) {
    notify(`话题切换失败：${e instanceof Error ? e.message : String(e)}`)
  }
}

/** system 消息的仲裁可视化分类：Deferred 存档 / 冻结 / 其他 */
function systemKind(content: string): 'deferred' | 'freeze' | 'plain' {
  if (content.includes('[deferred:')) return 'deferred'
  if (content.includes('冻结')) return 'freeze'
  return 'plain'
}
</script>

<template>
  <div>
    <div class="page-head">
      <div>
        <h1>群聊</h1>
        <div class="sub">人机混合群 · agent 发言经仲裁（@路由 / 相关性 / 预算 / 防连环）</div>
      </div>
      <button class="btn btn-primary" type="button" @click="showCreate = !showCreate">建群</button>
    </div>

    <!-- 建群表单 -->
    <div v-if="showCreate" class="card" style="margin-bottom: 16px">
      <h2 class="card-title">新建群聊</h2>
      <div class="f-row">
        <div>
          <label class="f-label">群名称 <span class="f-req">*</span></label>
          <input v-model="newName" class="f-input" type="text" placeholder="如 评审讨论组" />
        </div>
        <div>
          <label class="f-label">每话题发言上限</label>
          <input v-model="newMaxTurns" class="f-input" type="number" min="1" />
        </div>
        <div>
          <label class="f-label">静默冻结阈值（轮）</label>
          <input v-model="newSilent" class="f-input" type="number" min="1" />
        </div>
      </div>
      <label class="f-label">Agent 成员 <span class="f-req">*</span></label>
      <div style="display: flex; gap: 8px; flex-wrap: wrap">
        <label
          v-for="a in agentsStore.visible"
          :key="a.id"
          class="chip"
          style="cursor: pointer; padding: 5px 12px"
          :style="{ color: picked[a.id] ? 'var(--signal)' : undefined }"
        >
          <input v-model="picked[a.id]" type="checkbox" style="margin-right: 5px" />{{ a.name }}
        </label>
      </div>
      <div class="f-hint">成员触发方式一期统一 any（@ 指名优先，否则按画像相关性路由）；话题预算与冻结阈值保存后可热调。</div>
      <div style="margin-top: 14px">
        <button class="btn btn-primary" type="button" :disabled="creating" @click="create">创建群聊</button>
      </div>
    </div>

    <div v-if="flash" class="callout bad" style="margin-bottom: 12px">{{ flash }}</div>

    <div class="chat-layout">
      <!-- 群列表 -->
      <div class="card tight">
        <h2 class="card-title">群</h2>
        <div v-if="groups.length" class="sess-list" style="padding: 2px">
          <a
            v-for="g in groups"
            :key="g.id"
            href="#"
            :class="{ on: g.id === group?.id }"
            @click.prevent="router.replace({ query: { group: g.id } })"
          >
            <span style="font-weight: 600">{{ g.name }}</span>
            <span class="small muted">{{ g.members.length }} 成员</span>
          </a>
        </div>
        <div v-else class="empty" style="padding: 28px 8px">
          <div class="big">还没有群</div>
          <div>点右上角「建群」拉一个讨论组</div>
        </div>
      </div>

      <!-- 群聊天面板 -->
      <div v-if="group">
        <div class="card" style="padding: 0; overflow: hidden">
          <div style="display: flex; justify-content: space-between; align-items: center; padding: 12px 16px; border-bottom: 1px solid var(--line); gap: 10px; flex-wrap: wrap">
            <div style="display: flex; align-items: center; gap: 8px; flex-wrap: wrap">
              <b>{{ group.name }}</b>
              <span v-if="state?.frozen" class="st st-removed">已冻结</span>
              <span class="small muted" style="color: var(--ink-3)">
                预算
                <template v-for="(used, id) in state?.turns_used" :key="id">
                  <span class="mono">{{ memberName(String(id)) }} {{ used }}/{{ state?.max_turns_per_topic }}</span>
                </template>
              </span>
            </div>
            <div style="display: flex; gap: 6px">
              <button class="btn btn-quiet btn-sm" type="button" @click="doTopic">/topic</button>
              <button v-if="state?.frozen" class="btn btn-quiet btn-sm" type="button" @click="doUnfreeze">解冻</button>
            </div>
          </div>

          <!-- 成员 @ 快捷条 -->
          <div style="display: flex; gap: 6px; padding: 8px 16px 0; flex-wrap: wrap">
            <button v-for="m in agentMembers" :key="m.id" class="chip" type="button" style="cursor: pointer; border: none" @click="insertAt(m)">
              @{{ m.name ?? m.id }}
            </button>
          </div>

          <div class="chat-box">
            <div ref="historyEl" class="chat-history">
              <div style="display: flex; flex-direction: column; gap: 10px; padding: 8px 0">
                <template v-for="m in messages" :key="m.id">
                  <div
                    v-if="m.sender_type === 'system'"
                    class="bubble system"
                    :class="systemKind(m.content) === 'deferred' ? 'sys-deferred' : systemKind(m.content) === 'freeze' ? 'sys-freeze' : ''"
                  >
                    {{ m.content }}
                  </div>
                  <McBubble
                    v-else
                    :content="m.content"
                    :align="m.sender_type === 'human' ? 'right' : 'left'"
                  >
                    <template #avatar>
                      <span class="chat-avatar" :class="m.sender_type === 'human' ? 'is-human' : 'is-agent'">
                        {{ memberName(m.sender_id).charAt(0) }}
                      </span>
                    </template>
                    <template #top>
                      <span class="msg-meta">{{ m.sender_type === 'human' ? (m.sender_id === 'user' ? '你' : m.sender_id) : memberName(m.sender_id) }} · {{ clockTime(m.created_at) }}</span>
                    </template>
                  </McBubble>
                </template>
                <div v-if="!messages.length" class="chat-empty">群里还没有消息，@ 一个 agent 试试</div>
              </div>
            </div>

            <div class="chat-footer">
              <McInput
                :value="input"
                placeholder="输入消息（@名字 指定响应者）…"
                :max-length="2000"
                show-count
                :disabled="state?.frozen"
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
              从左侧选择一个群
            </div>
            多 agent 受控讨论：@路由、发言预算、防死循环
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
/* 与私聊页同族；system 消息按仲裁语义着色 */
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
.bubble.system.sys-deferred {
  color: var(--warn);
  border-color: rgba(251, 191, 36, 0.4);
  background: var(--warn-bg);
  max-width: 88%;
}
.bubble.system.sys-freeze {
  color: var(--bad);
  border-color: rgba(248, 113, 113, 0.4);
  background: var(--bad-bg);
}
</style>
