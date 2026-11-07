import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { AgentStatusEvent, MessageEventPayload } from '../api/types'

// 全局 SSE 订阅：原生 EventSource（断线自动重连，Last-Event-ID 由浏览器自动回传，
// 服务端补偿重放最近 500 条）。事件仅作「刷新信号」，负载用于精化局部更新。

const EVENT_TYPES = ['agent.status', 'msg.human', 'msg.agent', 'msg.system', 'task.lifecycle', 'arbiter.decision'] as const

export type EventType = (typeof EVENT_TYPES)[number]

/** SSE 帧元信息：seq 来自 writeSSE 的 id 字段（= bus.Event.Seq）。 */
export interface EventMeta {
  seq: number
}

// EventSource 不能放 pinia state（会被 reactive 代理破坏），放模块级变量。
let es: EventSource | null = null
const listeners = new Map<EventType, Set<(payload: any, meta: EventMeta) => void>>()

export const useEventStream = defineStore('eventStream', () => {
  const connected = ref(false)

  function ensure(): void {
    if (es) return
    es = new EventSource('/api/stream')
    es.onopen = () => {
      connected.value = true
    }
    es.onerror = () => {
      connected.value = false // 浏览器自动重连
    }
    for (const type of EVENT_TYPES) {
      es.addEventListener(type, (e: MessageEvent<string>) => {
        let payload: unknown = null
        try {
          payload = JSON.parse(e.data)
        } catch { /* 心跳类空数据忽略 */ }
        const meta: EventMeta = { seq: Number(e.lastEventId) || 0 }
        listeners.get(type)?.forEach((fn) => fn(payload, meta))
      })
    }
  }

  /** 订阅；返回退订函数。meta 携带事件 seq（可选参数，老订阅者不受影响）。 */
  function on<T = unknown>(type: EventType, fn: (payload: T, meta?: EventMeta) => void): () => void {
    ensure()
    if (!listeners.has(type)) listeners.set(type, new Set())
    listeners.get(type)!.add(fn as (payload: any, meta: EventMeta) => void)
    return () => {
      listeners.get(type)?.delete(fn as (payload: any, meta: EventMeta) => void)
    }
  }

  return { connected, on }
})

export type { AgentStatusEvent, MessageEventPayload }
