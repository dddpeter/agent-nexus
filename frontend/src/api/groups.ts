import { get, post, put } from './client'
import type { Group, GroupSendResult, GroupState, Message } from './types'

export const listGroups = (): Promise<Group[]> => get<Group[]>('/api/groups')

export const createGroup = (body: {
  name: string
  members: Array<{ id: string; type: string; trigger?: string; priority?: number; max_turns?: number; interrupt_level?: number; profile?: string }>
  max_turns_per_topic?: number
  human_silent_threshold?: number
}): Promise<Group> => post<Group>('/api/groups', body)

export interface GroupDetail {
  group: Group
  state: GroupState
}

export const getGroup = (id: string): Promise<GroupDetail> => get<GroupDetail>(`/api/groups/${id}`)

export const groupMessages = (id: string, limit = 200): Promise<Message[]> =>
  get<Message[]>(`/api/groups/${id}/messages?limit=${limit}`)

export const groupSend = (id: string, content: string, senderId = 'user'): Promise<GroupSendResult> =>
  post<GroupSendResult>(`/api/groups/${id}/messages`, { content, sender_id: senderId })

export const groupUnfreeze = (id: string): Promise<{ ok: boolean }> =>
  post<{ ok: boolean }>(`/api/groups/${id}/unfreeze`)

export const groupTopic = (id: string, topicId: string): Promise<{ ok: boolean }> =>
  post<{ ok: boolean }>(`/api/groups/${id}/topic`, { topic_id: topicId })

export const groupUpdateConfig = (
  id: string,
  body: { max_turns_per_topic?: number; human_silent_threshold?: number },
): Promise<GroupState> => put<GroupState>(`/api/groups/${id}/config`, body)
