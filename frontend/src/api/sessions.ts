import { get, post } from './client'
import type { Message, SendResult, Session, StreamFrame } from './types'

export const createSession = (agentId: string): Promise<Session> =>
  post<Session>('/api/sessions', { agent_id: agentId })

export const getSession = (id: string): Promise<Session> => get<Session>(`/api/sessions/${id}`)

/** 最近会话（跨 agent，聊天侧栏）。 */
export const recentSessions = (limit = 20): Promise<Session[]> => get<Session[]>(`/api/sessions?limit=${limit}`)

export const agentSessions = (agentId: string): Promise<Session[]> =>
  get<Session[]>(`/api/agents/${agentId}/sessions`)

export const sessionMessages = (sessionId: string): Promise<Message[]> =>
  get<Message[]>(`/api/sessions/${sessionId}/messages`)

export function sendSync(sessionId: string, content: string): Promise<SendResult> {
  return post<SendResult>(`/api/sessions/${sessionId}/messages`, { content, mode: 'sync' })
}

/**
 * 流式发送：fetch 读 SSE 分帧（delta/final/error），逐帧回调。
 * final/error 后服务端关流；final 内容已落库，调用方最后重拉历史对齐。
 */
export async function streamSend(
  sessionId: string,
  content: string,
  onFrame: (frame: StreamFrame) => void,
): Promise<void> {
  const res = await fetch(`/api/sessions/${sessionId}/messages`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    credentials: 'include',
    body: JSON.stringify({ content, mode: 'stream' }),
  })
  if (res.status === 401) {
    throw Object.assign(new Error('登录已失效，请重新登录'), { name: 'ApiError', status: 401 })
  }
  if (!res.ok || !res.body) {
    let msg = `HTTP ${res.status}`
    try {
      const body = await res.json()
      msg = body?.error?.message ?? msg
    } catch { /* 非 JSON 错误体 */ }
    throw new Error(msg)
  }

  const reader = res.body.getReader()
  const dec = new TextDecoder()
  let buf = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (done) return
    buf += dec.decode(value, { stream: true })
    let idx: number
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const frame = buf.slice(0, idx)
      buf = buf.slice(idx + 2)
      const line = frame.split('\n').find((l) => l.startsWith('data: '))
      if (!line) continue
      try {
        onFrame(JSON.parse(line.slice(6)) as StreamFrame)
      } catch { /* 跳过坏帧 */ }
    }
  }
}
