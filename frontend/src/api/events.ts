import { get } from './client'

// bus.Event JSON 契约（internal/api/events.go busEventDTO）
export interface BusEvent {
  seq: number
  type: string
  payload: unknown
  created_at: string
}

/** 最近事件（seq 降序，最新在前）。type 过滤可选。 */
export const listEvents = (limit = 100, type?: string): Promise<BusEvent[]> =>
  get<BusEvent[]>(`/api/events?limit=${limit}${type ? `&type=${encodeURIComponent(type)}` : ''}`)
