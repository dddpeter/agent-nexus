// 与后端 DTO 契约对齐（internal/api/agents.go、sessions.go、internal/adapter）。

export interface Capabilities {
  can_group_chat: boolean
  can_stream: boolean
  max_concurrency: number
  typical_latency: string // Duration 序列化为 "1.5s"
  interrupt_level: number // 0=禁止插话 1=可申请 2=可直接插
}

export interface Agent {
  id: string
  name: string
  adapter: string
  status: string // online | offline | busy | removed
  config: Record<string, unknown> // 密钥已脱敏为 "***"
  capabilities: Capabilities
  last_heartbeat_at: string | null
  in_flight: number
  disabled: boolean
}

export type FieldType = 'string' | 'int' | 'bool' | 'duration' | 'list'

export interface SchemaField {
  name: string
  label: string
  type: FieldType
  required?: boolean
  default?: unknown
  secret?: boolean
  help?: string
  options?: string[] // type=string 时的枚举
}

export interface AdapterInfo {
  name: string
  config_schema: { fields: SchemaField[] }
}

export interface TestResult {
  ok: boolean
  duration_ms: number
  error?: string
}

export interface Session {
  id: string
  agent_id: string
  kind: string
  created_at: string
  closed_at?: string | null
}

export interface Message {
  id: number
  sender_type: 'human' | 'agent' | 'system' | string
  sender_id: string
  content: string
  reply_to?: string
  created_at: string
}

export interface SendResult {
  message: Message
  reply?: Message
  timed_out: boolean
  async?: { type: string; task_id?: string; eta_hint?: string }
}

export interface AuthStatus {
  auth_required: boolean
  authenticated: boolean
}

// ---------- SSE ----------

// 全局流 agent.status 负载（registry.publishStatus）
export interface AgentStatusEvent {
  agent_id: string
  name: string
  from: string
  to: string
  reason: string
}

// 全局流 msg.* 负载（session/private.publishMessage）
export interface MessageEventPayload {
  session_id: string
  agent_id: string
  sender_type: 'human' | 'agent' | 'system' | string
  sender_id: string
  content: string
  message_id: number
  reply_to?: string
}

// 会话流式发送帧（streamSessionSend 转译）
export interface StreamFrame {
  type: 'delta' | 'final' | 'error'
  data: string
}
