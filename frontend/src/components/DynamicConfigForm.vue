<script setup lang="ts">
// schema 驱动动态表单（FR1.1a）—— 等价迁移 config_form.gohtml：
// string+options → select · bool → select(true/false) · int → number · list → 空格分隔文本 ·
// secret → password（已设置时空占位「留空保持不变」）· 其余 string → text。
// 控件变化经 set() 整体上抛新对象（defineModel 对象属性原地修改不会触发 emit）。
import { computed } from 'vue'
import type { SchemaField } from '../api/types'
import { secretSet } from '../api/agents'

const props = defineProps<{
  fields: SchemaField[]
  /** 当前配置（编辑场景，密钥为脱敏 "***"；可空 = 全用默认值） */
  current?: Record<string, unknown>
  modelValue: Record<string, string | boolean>
}>()

const emit = defineEmits<{ 'update:modelValue': [value: Record<string, string | boolean>] }>()

function set(name: string, v: string | boolean): void {
  emit('update:modelValue', { ...props.modelValue, [name]: v })
}

const str = (name: string): string => String(props.modelValue[name] ?? '')
const boolStr = (name: string): string => (props.modelValue[name] === true || props.modelValue[name] === 'true' ? 'true' : 'false')
const val = (e: Event): string => (e.target as HTMLInputElement).value
const sel = (e: Event): string => (e.target as HTMLSelectElement).value

// 已设置的密钥字段（显示占位用）
const setSecrets = computed<Set<string>>(() => {
  const s = new Set<string>()
  for (const f of props.fields) {
    if (f.secret && secretSet(props.fields, props.current, f.name)) s.add(f.name)
  }
  return s
})
</script>

<template>
  <template v-for="f in fields" :key="f.name">
    <label class="f-label">
      {{ f.label }} <span class="mono muted small">({{ f.name }})</span>
      <span v-if="f.secret" class="f-secret-tag">密钥</span>
      <span v-if="f.required" class="f-req">*</span>
    </label>

    <select v-if="f.options?.length" :value="str(f.name)" @change="set(f.name, sel($event))">
      <option v-for="opt in f.options" :key="opt" :value="opt">{{ opt }}</option>
    </select>

    <select v-else-if="f.type === 'bool'" :value="boolStr(f.name)" @change="set(f.name, sel($event) === 'true')">
      <option value="true">true</option>
      <option value="false">false</option>
    </select>

    <input v-else-if="f.type === 'int'" :value="str(f.name)" class="f-input" type="number" @input="set(f.name, val($event))" />

    <input
      v-else-if="f.type === 'list'"
      :value="str(f.name)"
      class="f-input"
      type="text"
      placeholder="空格分隔多个值"
      @input="set(f.name, val($event))"
    />

    <input
      v-else-if="f.secret"
      :value="str(f.name)"
      class="f-input"
      type="password"
      autocomplete="new-password"
      :placeholder="setSecrets.has(f.name) ? '••••••（留空保持不变）' : '输入密钥值'"
      @input="set(f.name, val($event))"
    />

    <input v-else :value="str(f.name)" class="f-input" type="text" @input="set(f.name, val($event))" />

    <div v-if="f.help" class="f-hint">{{ f.help }}</div>
  </template>
</template>
