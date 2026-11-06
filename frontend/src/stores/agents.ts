import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { listAgents } from '../api/agents'
import type { Agent } from '../api/types'
import { useEventStream } from './events'

// agent 目录 store：列表 + 遥测统计（口径与后端 computeStats 一致）+ SSE 联动。
// 统计需要含 removed（总数扣除），表格展示不含 removed —— 一次拉取 include_removed=true 共用。

export const useAgents = defineStore('agents', () => {
  const agents = ref<Agent[]>([])
  const loaded = ref(false)
  const loading = ref(false)
  const error = ref('')
  const query = ref('')

  async function load(): Promise<void> {
    loading.value = true
    error.value = ''
    try {
      agents.value = await listAgents({ includeRemoved: true })
      loaded.value = true
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  /** 表格行：不含 removed（与后端列表页口径一致）。 */
  const visible = computed<Agent[]>(() => agents.value.filter((a) => a.status !== 'removed'))

  /** 搜索过滤：名称 / adapter / 状态包含即命中（与原 SSR dtoMatches 一致）。 */
  const filtered = computed<Agent[]>(() => {
    const q = query.value.trim().toLowerCase()
    if (!q) return visible.value
    return visible.value.filter(
      (a) => a.name.toLowerCase().includes(q) || a.adapter.toLowerCase().includes(q) || a.status.includes(q),
    )
  })

  /** 遥测统计：online 计入 busy；total 扣除 removed。 */
  const stats = computed(() => {
    let online = 0
    let offline = 0
    let removed = 0
    for (const a of agents.value) {
      if (a.status === 'online' || a.status === 'busy') online++
      else if (a.status === 'offline') offline++
      else if (a.status === 'removed') removed++
    }
    return { total: agents.value.length - removed, online, offline, removed }
  })

  function byId(id: string): Agent | undefined {
    return agents.value.find((a) => a.id === id)
  }

  function bindEvents(): void {
    const events = useEventStream()
    // 状态迁移是低频事件，整表重拉最简单且口径不出错
    events.on('agent.status', () => {
      void load()
    })
  }

  return { agents, loaded, loading, error, query, load, visible, filtered, stats, byId, bindEvents }
})
