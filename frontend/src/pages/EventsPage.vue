<script setup lang="ts">
// 事件流页（可观测性）：事件总线落库存档的检索视图 + SSE 实时置顶追加。
// 事件只读 —— 与 /api/stream 同源（bus.Event），这里是给运维看的「总线回放」。
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { listEvents, type BusEvent } from '../api/events'
import { useEventStream } from '../stores/events'
import { clockTime, relTime } from '../utils/format'

const events = useEventStream()

const items = ref<BusEvent[]>([])
const loading = ref(false)
const loadError = ref('')
const typeFilter = ref('')

const EVENT_TYPES = [
  'agent.status',
  'msg.human',
  'msg.agent',
  'msg.system',
  'task.lifecycle',
  'arbiter.decision',
] as const

const filtered = computed(() =>
  typeFilter.value ? items.value.filter((e) => e.type === typeFilter.value) : items.value,
)

async function load(): Promise<void> {
  loading.value = true
  loadError.value = ''
  try {
    items.value = await listEvents(150)
  } catch (e) {
    loadError.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

// SSE 实时置顶：meta.seq 即 bus seq（writeSSE 的 id 字段）；刷新后与服务端 seq 自然对齐
const offs: Array<() => void> = []
onMounted(async () => {
  void load()
  for (const t of EVENT_TYPES) {
    offs.push(
      events.on(t, (payload, meta) => {
        items.value = [
          { seq: meta?.seq ?? 0, type: t, payload, created_at: new Date().toISOString() },
          ...items.value,
        ].slice(0, 300)
      }),
    )
  }
})
onUnmounted(() => offs.forEach((fn) => fn()))

function payloadPreview(payload: unknown): string {
  try {
    const s = JSON.stringify(payload)
    return s.length > 160 ? s.slice(0, 160) + '…' : s
  } catch {
    return String(payload)
  }
}

function typeClass(t: string): string {
  if (t === 'agent.status') return 'ev-status'
  if (t.startsWith('msg.')) return t === 'msg.system' ? 'ev-system' : 'ev-msg'
  if (t === 'task.lifecycle') return 'ev-task'
  return 'ev-arbiter'
}
</script>

<template>
  <div>
    <div class="page-head">
      <div>
        <h1>事件流</h1>
        <div class="sub">事件总线落库存档 · SSE 实时置顶 · 保留最近 500 条补偿重放水位</div>
      </div>
      <button class="btn btn-secondary" type="button" :disabled="loading" @click="load">
        刷新<span v-if="loading" class="spin"></span>
      </button>
    </div>

    <div class="card" style="padding: 6px 20px 10px">
      <div class="tbl-tools">
        <div class="ev-filters">
          <button
            class="chip"
            :class="{ on: !typeFilter }"
            type="button"
            style="cursor: pointer; border: none"
            @click="typeFilter = ''"
          >全部</button>
          <button
            v-for="t in EVENT_TYPES"
            :key="t"
            class="chip"
            :class="{ on: typeFilter === t }"
            type="button"
            style="cursor: pointer; border: none"
            @click="typeFilter = typeFilter === t ? '' : t"
          >{{ t }}</button>
        </div>
        <span class="small muted">{{ filtered.length }} 条</span>
      </div>

      <div v-if="loadError" class="callout bad" style="margin: 8px 0">{{ loadError }}</div>

      <div class="ev-feed">
        <div v-for="(e, i) in filtered" :key="`${e.seq}-${i}`" class="ev-row">
          <span class="mono muted ev-seq">#{{ e.seq }}</span>
          <span class="chip" :class="typeClass(e.type)">{{ e.type }}</span>
          <code class="ev-payload mono" :title="payloadPreview(e.payload)">{{ payloadPreview(e.payload) }}</code>
          <span class="small muted ev-time" :title="e.created_at">{{ clockTime(e.created_at) }} · {{ relTime(e.created_at) }}</span>
        </div>
        <div v-if="!filtered.length && !loading" class="empty">
          <div class="big">暂无事件</div>
          <div>agent 状态迁移、会话消息、任务生命周期都会流入这里</div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.ev-filters { display: flex; gap: 6px; flex-wrap: wrap; }
.ev-feed { display: flex; flex-direction: column; }
.ev-row {
  display: flex; align-items: baseline; gap: 10px;
  padding: 7px 4px; border-bottom: 1px solid var(--line);
  font-size: 12.5px; min-width: 0;
}
.ev-row:last-child { border-bottom: none; }
.ev-seq { flex: none; min-width: 52px; }
.ev-payload {
  flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  color: var(--ink-2); background: none; border: none; padding: 0;
}
.ev-time { flex: none; font-variant-numeric: tabular-nums; }
.chip.ev-msg { color: var(--signal); background: var(--signal-wash); }
.chip.ev-status { color: var(--ok); background: var(--ok-bg); }
.chip.ev-system { color: var(--mut); background: var(--mut-bg); }
.chip.ev-task { color: var(--warn); background: var(--warn-bg); }
.chip.ev-arbiter { color: #c084fc; background: rgba(192, 132, 252, .12); }
</style>
