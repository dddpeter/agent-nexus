import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import DynamicConfigForm from './DynamicConfigForm.vue'
import type { SchemaField } from '../api/types'

const fields: SchemaField[] = [
  { name: 'mode', label: '模式', type: 'string', options: ['fast', 'safe'], default: 'safe' },
  { name: 'debug', label: '调试', type: 'bool', default: false },
  { name: 'workers', label: '并发', type: 'int', default: 1 },
  { name: 'tags', label: '标签', type: 'list' },
  { name: 'api_key', label: '密钥', type: 'string', secret: true },
  { name: 'endpoint', label: '端点', type: 'string', required: true },
]

describe('DynamicConfigForm', () => {
  it('按字段类型渲染对应控件', () => {
    const wrapper = mount(DynamicConfigForm, {
      props: { fields, current: undefined, modelValue: { mode: 'safe', debug: false, workers: '1', tags: '', api_key: '', endpoint: '' } },
    })
    expect(wrapper.find('select').exists()).toBe(true) // options → select
    const inputs = wrapper.findAll('input')
    expect(inputs.some((i) => i.attributes('type') === 'number')).toBe(true) // int
    expect(inputs.some((i) => i.attributes('type') === 'password')).toBe(true) // secret
    expect(wrapper.text()).toContain('密钥')
    expect(wrapper.text()).toContain('*') // required 标记
  })

  it('已设置密钥显示留空保持占位', () => {
    const wrapper = mount(DynamicConfigForm, {
      props: {
        fields,
        current: { api_key: '***' },
        modelValue: { mode: 'safe', debug: false, workers: '1', tags: '', api_key: '', endpoint: '' },
      },
    })
    const pwd = wrapper.find('input[type="password"]')
    expect(pwd.attributes('placeholder')).toContain('留空保持不变')
  })

  it('输入变化通过 update:modelValue 上抛', async () => {
    const wrapper = mount(DynamicConfigForm, {
      props: { fields, modelValue: { mode: 'safe', debug: false, workers: '1', tags: '', api_key: '', endpoint: '' } },
    })
    // fields 顺序中第一个 text 输入是 list 型 tags 字段
    const text = wrapper.findAll('input').find((i) => i.attributes('type') === 'text')!
    await text.setValue('a b')
    const emitted = wrapper.emitted('update:modelValue')
    expect(emitted).toBeTruthy()
    const last = emitted![emitted!.length - 1][0] as Record<string, string>
    expect(last.tags).toBe('a b')
  })
})
