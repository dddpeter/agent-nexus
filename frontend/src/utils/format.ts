// 展示格式化（对应原 SSR 模板函数 relTimeAll / formatTime）。

export function relTime(iso: string | null | undefined): string {
  if (!iso) return '—'
  const t = new Date(iso)
  if (Number.isNaN(t.getTime())) return '—'
  const s = (Date.now() - t.getTime()) / 1000
  if (s < 8) return '刚刚'
  if (s < 60) return `${Math.floor(s)} 秒前`
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`
  if (s < 86400) return `${Math.floor(s / 3600)} 小时前`
  return `${Math.floor(s / 86400)} 天前`
}

export function fullTime(iso: string | null | undefined): string {
  if (!iso) return ''
  const t = new Date(iso)
  return Number.isNaN(t.getTime()) ? '' : t.toLocaleString()
}

export function clockTime(iso: string | null | undefined): string {
  if (!iso) return ''
  const t = new Date(iso)
  return Number.isNaN(t.getTime()) ? '' : t.toLocaleTimeString('zh-CN', { hour12: false })
}

export function initial(name: string): string {
  return name ? name.charAt(0).toUpperCase() : '?'
}
