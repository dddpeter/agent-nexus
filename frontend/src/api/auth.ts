import { get, post } from './client'
import type { AuthStatus } from './types'

let cached: AuthStatus | null = null

/** 路由守卫用（缓存一次，登录/登出后 force 刷新）。 */
export async function authStatus(force = false): Promise<AuthStatus> {
  if (force || cached === null) {
    cached = await get<AuthStatus>('/api/auth/status')
  }
  return cached
}

export function invalidateAuthCache(): void {
  cached = null
}

export async function login(token: string): Promise<void> {
  await post('/api/auth/login', { token })
  invalidateAuthCache()
}

export async function logout(): Promise<void> {
  await post('/api/auth/logout')
  invalidateAuthCache()
}
