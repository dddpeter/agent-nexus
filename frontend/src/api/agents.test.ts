import { describe, expect, it } from 'vitest'
import { buildConfigPayload, initialFormValues } from './agents'
import type { SchemaField } from './types'

const fields: SchemaField[] = [
  { name: 'endpoint', label: '端点', type: 'string', required: true },
  { name: 'api_key', label: '密钥', type: 'string', secret: true },
  { name: 'timeout', label: '超时', type: 'duration', default: '2m' },
  { name: 'workers', label: '并发', type: 'int', default: 1 },
  { name: 'debug', label: '调试', type: 'bool', default: false },
  { name: 'tags', label: '标签', type: 'list' },
]

describe('buildConfigPayload', () => {
  it('按 schema 类型规范化表单值', () => {
    const out = buildConfigPayload(fields, {
      endpoint: 'http://127.0.0.1:9000',
      api_key: 'sk-xxx',
      timeout: '5s',
      workers: '4',
      debug: true,
      tags: 'a b  c',
    })
    expect(out).toEqual({
      endpoint: 'http://127.0.0.1:9000',
      api_key: 'sk-xxx',
      timeout: '5s',
      workers: 4,
      debug: true,
      tags: ['a', 'b', 'c'],
    })
  })

  it('secret 留空或脱敏占位不提交（后端沿用旧值）', () => {
    const out = buildConfigPayload(fields, { endpoint: 'x', api_key: '' })
    expect(out).not.toHaveProperty('api_key')
    const out2 = buildConfigPayload(fields, { endpoint: 'x', api_key: '***' })
    expect(out2).not.toHaveProperty('api_key')
  })

  it('空值字段省略，空 list 提交空数组', () => {
    const out = buildConfigPayload(fields, { endpoint: 'x', workers: '', tags: '' })
    expect(out).not.toHaveProperty('workers')
    expect(out).toHaveProperty('tags', [])
  })
})

describe('initialFormValues', () => {
  it('当前值优先于 schema 默认值', () => {
    const v = initialFormValues(fields, { endpoint: 'http://cur', workers: 8, debug: true, tags: ['x', 'y'] })
    expect(v.endpoint).toBe('http://cur')
    expect(v.workers).toBe('8')
    expect(v.debug).toBe(true)
    expect(v.tags).toBe('x y')
    expect(v.timeout).toBe('2m') // 默认值
  })

  it('secret 永远初始为空（脱敏回显不回填）', () => {
    const v = initialFormValues(fields, { api_key: '***', endpoint: 'x' })
    expect(v.api_key).toBe('')
  })

  it('无当前值时用默认值初始化', () => {
    const v = initialFormValues(fields)
    expect(v.endpoint).toBe('')
    expect(v.timeout).toBe('2m')
    expect(v.workers).toBe('1')
    expect(v.debug).toBe(false)
  })
})
