// 统一 fetch 封装：JSON 编解码、错误 envelope 解析、401 统一处理（跳登录）。
// cookie 会话由浏览器自动携带（credentials: 'include' 兼容 dev 代理）。

export class ApiError extends Error {
  status: number
  code: string
  requestId: string

  constructor(status: number, code: string, message: string, requestId = '') {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.requestId = requestId
  }
}

type UnauthorizedHandler = () => void

let unauthorizedHandler: UnauthorizedHandler | null = null

/** 由 main.ts 注入路由跳转，避免 client ↔ router 循环依赖。 */
export function setUnauthorizedHandler(fn: UnauthorizedHandler): void {
  unauthorizedHandler = fn
}

export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, { credentials: 'include', ...init })
  if (res.status === 401) {
    unauthorizedHandler?.()
    throw new ApiError(401, 'unauthorized', '登录已失效，请重新登录')
  }
  if (res.status === 204) return undefined as T

  const isJson = res.headers.get('content-type')?.includes('application/json') ?? false
  const body = isJson ? await res.json().catch(() => null) : null
  if (!res.ok) {
    const err = (body as { error?: { code?: string; message?: string; request_id?: string } } | null)?.error
    throw new ApiError(
      res.status,
      err?.code ?? 'http_error',
      err?.message ?? `HTTP ${res.status}`,
      err?.request_id ?? '',
    )
  }
  return body as T
}

export const get = <T>(path: string): Promise<T> => request<T>(path)

export function post<T>(path: string, body?: unknown): Promise<T> {
  return request<T>(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
}

export function put<T>(path: string, body: unknown): Promise<T> {
  return request<T>(path, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

export const del = (path: string): Promise<void> => request<void>(path, { method: 'DELETE' })
