import { del, get, post, put } from './client'
import type { AdapterInfo, Agent, Capabilities, SchemaField, TestResult } from './types'

export interface AgentFilter {
  status?: string
  adapter?: string
  includeRemoved?: boolean
}

export function listAgents(filter: AgentFilter = {}): Promise<Agent[]> {
  const q = new URLSearchParams()
  if (filter.status) q.set('status', filter.status)
  if (filter.adapter) q.set('adapter', filter.adapter)
  if (filter.includeRemoved) q.set('include_removed', 'true')
  const qs = q.toString()
  return get<Agent[]>(`/api/agents${qs ? `?${qs}` : ''}`)
}

export const getAgent = (id: string): Promise<Agent> => get<Agent>(`/api/agents/${id}`)

export function listAdapters(): Promise<AdapterInfo[]> {
  return get<AdapterInfo[]>('/api/adapters')
}

export interface RegisterReq {
  name: string
  adapter: string
  config: Record<string, unknown>
  capabilities?: Capabilities
}

export const registerAgent = (req: RegisterReq): Promise<Agent> => post<Agent>('/api/agents', req)

export interface UpdateReq {
  config: Record<string, unknown>
  capabilities?: Capabilities
  force?: boolean
}

export const updateAgent = (id: string, req: UpdateReq): Promise<Agent> => put<Agent>(`/api/agents/${id}`, req)

/** 保存前按规格测试（向导用，不落库）。 */
export function testSpec(adapter: string, config: Record<string, unknown>, sendMessage: boolean): Promise<TestResult> {
  return post<TestResult>('/api/agents/test', { adapter, config, send_message: sendMessage })
}

/** 已保存 agent 连通性测试（body 可空 = 用当前配置）。 */
export const testAgent = (id: string): Promise<TestResult> =>
  post<TestResult>(`/api/agents/${id}/test`, {})

export const disableAgent = (id: string): Promise<void> => post(`/api/agents/${id}/disable`)
export const enableAgent = (id: string): Promise<void> => post(`/api/agents/${id}/enable`)
export const deleteAgent = (id: string): Promise<void> => del(`/api/agents/${id}`)

/**
 * 表单值 → 后端 config 负载。后端 CoerceConfig 宽容接受字符串，
 * 但这里按 schema 规范化为目标类型，错误在前端提前暴露。
 * secret 字段：留空或仍是脱敏占位 "***" → 不提交（后端沿用旧值）。
 */
export function buildConfigPayload(
  fields: SchemaField[],
  values: Record<string, string | boolean>,
): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const f of fields) {
    const raw = values[f.name]
    if (raw === undefined) continue
    if (f.secret) {
      const s = String(raw).trim()
      if (s === '' || s === '***') continue
      out[f.name] = s
      continue
    }
    switch (f.type) {
      case 'bool':
        out[f.name] = raw === true || raw === 'true'
        break
      case 'int': {
        const s = String(raw).trim()
        if (s !== '') out[f.name] = Number.parseInt(s, 10)
        break
      }
      case 'list': {
        const s = String(raw).trim()
        out[f.name] = s === '' ? [] : s.split(/\s+/)
        break
      }
      default: {
        const s = String(raw).trim()
        if (s !== '') out[f.name] = s
      }
    }
  }
  return out
}

/** 表单初始值：当前值（可为脱敏值）> schema 默认值；secret 永远留空。 */
export function initialFormValues(fields: SchemaField[], current?: Record<string, unknown>): Record<string, string | boolean> {
  const out: Record<string, string | boolean> = {}
  for (const f of fields) {
    if (f.secret) {
      out[f.name] = ''
      continue
    }
    const cur = current?.[f.name]
    const v = cur !== undefined && cur !== null && cur !== '' ? cur : f.default
    switch (f.type) {
      case 'bool':
        out[f.name] = v === true || v === 'true'
        break
      case 'list':
        out[f.name] = Array.isArray(v) ? v.join(' ') : v === undefined || v === null ? '' : String(v)
        break
      default:
        out[f.name] = v === undefined || v === null ? '' : String(v)
    }
  }
  return out
}

/** 密钥字段是否已设置（当前值非空非占位逻辑在显示层用）。 */
export function secretSet(fields: SchemaField[], current?: Record<string, unknown>, name?: string): boolean {
  if (name === undefined) return false
  const f = fields.find((x) => x.name === name)
  if (!f || !f.secret) return false
  const v = current?.[name]
  return typeof v === 'string' ? v.trim() !== '' : v !== undefined && v !== null
}

// ---------- hermes 网关自动探测（降低注册难度） ----------

export interface HermesCandidate {
  port: number
  base_url: string
  platform?: string
  version?: string
  auth_required: boolean
}

export interface HermesDiscoverResult {
  scanned: number
  candidates: HermesCandidate[]
}

/** 扫描本机 8640-8649 端口段，返回实际运行的 hermes 网关候选。 */
export const discoverHermes = (host = '127.0.0.1'): Promise<HermesDiscoverResult> =>
  get<HermesDiscoverResult>(`/api/adapters/hermes/discover?host=${host}`)
