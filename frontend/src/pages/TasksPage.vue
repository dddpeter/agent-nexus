<script setup lang="ts">
// 任务编排页（M4 / FR5.3）：任务列表 + 节点时间线（DAG 进度）+ 链提交表单 + cron 管理。
// SSE task.lifecycle 实时刷新；failed 任务可 resume（从失败节点续跑）。
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { cancelTask, createCronJob, deleteCronJob, getTask, listCronJobs, listTasks, resumeTask, submitChain } from '../api/tasks'
import type { Task, TaskNodeState } from '../api/types'
import { useAgents } from '../stores/agents'
import { useEventStream } from '../stores/events'
import { relTime } from '../utils/format'

const agentsStore = useAgents()
const events = useEventStream()

const tasks = ref<Task[]>([])
const crons = ref<Array<{ id: string; name: string; cron_expr: string; last_status?: string; last_task_id?: string }>>([])
const flash = ref('')
const expanded = ref('') // 展开详情的 task id
const nodeDetail = ref<TaskNodeState[]>([])

// 链提交表单：动态行
const formInput = ref('')
const formRows = ref<Array<{ agent: string; input: string; retries: string }>>([])
const submitting = ref(false)

// cron 表单
const cronName = ref('')
const cronExpr = ref('')
const cronAgent = ref('')

const canSubmit = computed(() => formRows.value.some((r) => r.agent) && formInput.value.trim())

function notify(msg: string): void {
  flash.value = msg
  setTimeout(() => {
    if (flash.value === msg) flash.value = ''
  }, 5000)
}

async function load(): Promise<void> {
  try {
    tasks.value = await listTasks(50)
  } catch {
    /* 忽略 */
  }
  try {
    crons.value = await listCronJobs()
  } catch {
    crons.value = []
  }
}

async function toggleDetail(t: Task): Promise<void> {
  if (expanded.value === t.id) {
    expanded.value = ''
    return
  }
  try {
    const fresh = await getTask(t.id)
    nodeDetail.value = fresh.nodes ?? []
    expanded.value = t.id
  } catch (e) {
    notify(`详情加载失败：${e instanceof Error ? e.message : String(e)}`)
  }
}

function agentName(id: string): string {
  return agentsStore.byId(id)?.name ?? id
}

async function addRow(): Promise<void> {
  formRows.value.push({ agent: '', input: '{{prev}}', retries: '0' })
}

async function submit(): Promise<void> {
  if (!canSubmit.value || submitting.value) return
  submitting.value = true
  try {
    const nodes = formRows.value
      .filter((r) => r.agent)
      .map((r) => ({
        agent_id: r.agent,
        input: r.input || '{{prev}}',
        retries: Number.parseInt(r.retries, 10) || 0,
      }))
    const res = await submitChain(nodes, formInput.value.trim())
    notify(`任务已提交：${res.id}`)
    formRows.value = []
    formInput.value = ''
    await load()
  } catch (e) {
    notify(`提交失败：${e instanceof Error ? e.message : String(e)}`)
  } finally {
    submitting.value = false
  }
}

async function doCancel(t: Task): Promise<void> {
  try {
    await cancelTask(t.id)
    await load()
  } catch (e) {
    notify(`取消失败：${e instanceof Error ? e.message : String(e)}`)
  }
}

async function doResume(t: Task): Promise<void> {
  try {
    await resumeTask(t.id)
    notify(`已从失败节点恢复：${t.id}`)
    await load()
  } catch (e) {
    notify(`恢复失败：${e instanceof Error ? e.message : String(e)}`)
  }
}

async function doDeleteCron(id: string): Promise<void> {
  try {
    await deleteCronJob(id)
    await load()
  } catch (e) {
    notify(`删除失败：${e instanceof Error ? e.message : String(e)}`)
  }
}

async function createCron(): Promise<void> {
  if (!cronName.value.trim() || !cronAgent.value || !cronExpr.value.trim()) return
  try {
    await createCronJob({
      name: cronName.value.trim(),
      expr: cronExpr.value.trim(),
      target: { type: 'agent', agent_id: cronAgent.value, input: formInput.value.trim() || '定时问候' },
    })
    cronName.value = ''
    cronExpr.value = ''
    await load()
  } catch (e) {
    notify(`cron 创建失败：${e instanceof Error ? e.message : String(e)}`)
  }
}

const offs: Array<() => void> = []
onMounted(async () => {
  if (!agentsStore.loaded) void agentsStore.load()
  void load()
  offs.push(events.on('task.lifecycle', () => void load()))
})
onUnmounted(() => offs.forEach((fn) => fn()))

function stClass(s: string): string {
  return `st st-${s === 'done' ? 'online' : s === 'failed' ? 'offline' : s === 'running' ? 'busy' : 'removed'}`
}

function nodeProgress(t: Task): string {
  const n = t.nodes?.length ?? 0
  if (!n) return ''
  const done = t.nodes!.filter((x) => x.status === 'done').length
  return `${done}/${n}`
}
</script>

<template>
  <div>
    <div class="page-head">
      <div>
        <h1>任务</h1>
        <div class="sub">协作链 DAG · 节点重试/超时/取消/恢复 · cron 定时触发</div>
      </div>
    </div>

    <div v-if="flash" class="callout" :class="flash.includes('失败') ? 'bad' : 'ok'" style="margin-bottom: 12px">{{ flash }}</div>

    <!-- 链提交 -->
    <div class="card tight" style="margin-bottom: 16px">
      <h2 class="card-title">提交协作链</h2>
      <div class="f-row" style="grid-template-columns: 1fr">
        <div>
          <label class="f-label">任务初始输入（首节点 prev/task.input 占位同源）</label>
          <input v-model="formInput" class="f-input" type="text" placeholder="如：梳理本周群聊要点" />
        </div>
      </div>
      <label class="f-label">节点序列（prev 占位注入上一节点输出）</label>
      <div v-for="(row, i) in formRows" :key="i" class="f-row" style="grid-template-columns: 220px 1fr 80px 40px; align-items: center; margin-bottom: 6px">
        <select v-model="row.agent" class="f-input">
          <option value="" disabled>选择 agent</option>
          <option v-for="a in agentsStore.visible" :key="a.id" :value="a.id">{{ a.name }}</option>
        </select>
        <input v-model="row.input" class="f-input" type="text" placeholder="输入模板（支持 {{prev}}）" />
        <input v-model="row.retries" class="f-input" type="number" min="0" placeholder="重试" />
        <button class="btn btn-quiet btn-sm" type="button" @click="formRows.splice(i, 1)">✕</button>
      </div>
      <div style="display: flex; gap: 8px; margin-top: 8px">
        <button class="btn btn-secondary btn-sm" type="button" @click="addRow">＋ 节点</button>
        <button class="btn btn-primary" type="button" :disabled="!canSubmit || submitting" @click="submit">
          提交任务<span v-if="submitting" class="spin"></span>
        </button>
      </div>
    </div>

    <!-- 任务列表 -->
    <div class="card" style="padding: 6px 20px 10px; margin-bottom: 16px">
      <h2 class="card-title" style="padding-top: 8px">任务</h2>
      <table class="tbl">
        <thead>
          <tr>
            <th style="width: 90px">状态</th>
            <th>ID</th>
            <th style="width: 90px">节点</th>
            <th>结果 / 错误</th>
            <th style="width: 150px">时间</th>
            <th style="width: 170px"></th>
          </tr>
        </thead>
        <tbody>
          <template v-for="t in tasks" :key="t.id">
            <tr>
              <td><span class="st" :class="stClass(t.status)">{{ t.status }}</span></td>
              <td class="mono">{{ t.id.slice(0, 16) }}</td>
              <td class="mono">{{ nodeProgress(t) }}</td>
              <td class="small" style="max-width: 320px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap">
                <span v-if="t.error" style="color: var(--bad)">{{ t.error }}</span>
                <span v-else-if="t.result" class="muted">{{ t.result.slice(0, 80) }}</span>
                <span v-else class="muted">—</span>
              </td>
              <td class="small muted">{{ relTime(t.created_at) }}</td>
              <td>
                <div class="row-actions">
                  <button class="btn btn-quiet btn-sm" type="button" @click="toggleDetail(t)">详情</button>
                  <button v-if="t.status === 'running' || t.status === 'queued'" class="btn btn-quiet btn-sm" type="button" @click="doCancel(t)">取消</button>
                  <button v-if="t.status === 'failed'" class="btn btn-quiet btn-sm" type="button" @click="doResume(t)">恢复</button>
                </div>
              </td>
            </tr>
            <tr v-if="expanded === t.id">
              <td colspan="6" style="background: rgba(7, 12, 24, 0.5)">
                <div style="display: flex; flex-direction: column; gap: 6px; padding: 8px 4px">
                  <div v-for="n in nodeDetail" :key="n.id" style="display: flex; gap: 10px; align-items: baseline; font-size: 12.5px">
                    <span class="mono muted" style="min-width: 36px">#{{ n.id }}</span>
                    <span class="st" :class="stClass(n.status)" style="min-width: 70px">{{ n.status }}</span>
                    <span class="chip">{{ agentName(n.agent_id) }}</span>
                    <span v-if="n.retried" class="small muted">重试 {{ n.retried }}</span>
                    <code class="mono muted" style="flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap" :title="n.output || n.error">
                      {{ n.error ? '✘ ' + n.error : (n.output || '—').slice(0, 110) }}
                    </code>
                  </div>
                </div>
              </td>
            </tr>
          </template>
          <tr v-if="!tasks.length">
            <td colspan="6">
              <div class="empty"><div class="big">还没有任务</div><div>提交一条协作链试试：a1 → a2 → a3</div></div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- cron 管理 -->
    <div class="card tight">
      <h2 class="card-title">定时任务（cron）</h2>
      <div class="f-row" style="grid-template-columns: 200px 160px 1fr auto; align-items: end">
        <div>
          <label class="f-label">名称</label>
          <input v-model="cronName" class="f-input" type="text" placeholder="daily-report" />
        </div>
        <div>
          <label class="f-label">cron 表达式（分 时 日 月 周）</label>
          <input v-model="cronExpr" class="f-input" type="text" placeholder="0 9 * * 1-5" />
        </div>
        <div>
          <label class="f-label">目标 agent</label>
          <select v-model="cronAgent" class="f-input">
            <option value="" disabled>选择 agent</option>
            <option v-for="a in agentsStore.visible" :key="a.id" :value="a.id">{{ a.name }}</option>
          </select>
        </div>
        <button class="btn btn-secondary" type="button" @click="createCron">登记</button>
      </div>
      <div v-if="crons.length" style="margin-top: 10px">
        <div v-for="j in crons" :key="j.id" style="display: flex; gap: 10px; align-items: center; padding: 6px 0; border-bottom: 1px solid var(--line); font-size: 13px">
          <b>{{ j.name }}</b>
          <code class="mono muted">{{ j.cron_expr }}</code>
          <span v-if="j.last_status" class="chip" :class="{ on: j.last_status === 'ok' }">{{ j.last_status }}</span>
          <span v-if="j.last_task_id" class="mono muted">{{ j.last_task_id.slice(0, 14) }}</span>
          <span style="flex: 1"></span>
          <button class="btn btn-danger btn-sm" type="button" @click="doDeleteCron(j.id)">删除</button>
        </div>
      </div>
      <div v-else class="small muted" style="padding: 6px 0">暂无定时任务</div>
    </div>
  </div>
</template>
