import { del, get, post } from './client'
import type { ChainNode, CronJob, CronTarget, Task } from './types'

export const listTasks = (limit = 50): Promise<Task[]> => get<Task[]>(`/api/tasks?limit=${limit}`)

export const getTask = (id: string): Promise<Task> => get<Task>(`/api/tasks/${id}`)

export const submitChain = (nodes: ChainNode[], input: string): Promise<{ id: string }> =>
  post<{ id: string }>('/api/tasks', { chain: { nodes, input } })

export const cancelTask = (id: string): Promise<{ ok: boolean }> =>
  post<{ ok: boolean }>(`/api/tasks/${id}/cancel`)

export const resumeTask = (id: string): Promise<{ id: string }> =>
  post<{ id: string }>(`/api/tasks/${id}/resume`)

export const listCronJobs = (): Promise<CronJob[]> => get<CronJob[]>('/api/cron')

export const createCronJob = (body: {
  name: string
  expr: string
  target: CronTarget
}): Promise<CronJob> => post<CronJob>('/api/tasks', { cron: body })

export const deleteCronJob = (id: string): Promise<void> =>
  del(`/api/cron/${id}`)
