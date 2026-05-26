import {
  getBots,
  deleteBotsByBotIdMessages,
  getBotsByBotIdSessions,
  getBotsByBotIdSessionsBySessionIdAcpRuntime,
  postBotsByBotIdSessions,
  postBotsByBotIdSessionsBySessionIdAcpRuntime,
  deleteBotsByBotIdSessionsBySessionId,
  patchBotsByBotIdSessionsBySessionId,
  patchBotsByBotIdSessionsBySessionIdAcpRuntimeModel,
} from '@memohai/sdk'
import type { AcpagentRuntimeStatus } from '@memohai/sdk'
import type { Bot, SessionSummary } from './useChat.types'

export interface CreateSessionOptions {
  title?: string
  type?: string
  metadata?: Record<string, unknown>
}

export async function fetchBots(): Promise<Bot[]> {
  const { data } = await getBots({ throwOnError: true })
  return data?.items ?? []
}

export async function fetchSessions(botId: string): Promise<SessionSummary[]> {
  const id = botId.trim()
  if (!id) return []
  const { data } = await getBotsByBotIdSessions({
    path: { bot_id: id },
    throwOnError: true,
  })
  return (data as Record<string, unknown>)?.items as SessionSummary[] ?? []
}

export async function createSession(botId: string, options?: string | CreateSessionOptions): Promise<SessionSummary> {
  const id = botId.trim()
  if (!id) throw new Error('bot id is required')
  const body = typeof options === 'string'
    ? { title: options, channel_type: 'local' }
    : {
        title: options?.title ?? '',
        channel_type: 'local',
        type: options?.type,
        metadata: options?.metadata,
      }
  const { data } = await postBotsByBotIdSessions({
    path: { bot_id: id },
    body,
    throwOnError: true,
  })
  return data as SessionSummary
}

export async function updateSessionTitle(botId: string, sessionId: string, title: string): Promise<SessionSummary> {
  const { data } = await patchBotsByBotIdSessionsBySessionId({
    path: { bot_id: botId.trim(), session_id: sessionId.trim() },
    body: { title },
    throwOnError: true,
  })
  return data as SessionSummary
}

export async function updateSessionAgent(botId: string, sessionId: string, type: string, metadata: Record<string, unknown>): Promise<SessionSummary> {
  const { data } = await patchBotsByBotIdSessionsBySessionId({
    path: { bot_id: botId.trim(), session_id: sessionId.trim() },
    body: { type, metadata },
    throwOnError: true,
  })
  return data as SessionSummary
}

export async function ensureACPRuntime(botId: string, sessionId: string): Promise<AcpagentRuntimeStatus> {
  const { data } = await postBotsByBotIdSessionsBySessionIdAcpRuntime({
    path: { bot_id: botId.trim(), session_id: sessionId.trim() },
    throwOnError: true,
  })
  return data as AcpagentRuntimeStatus
}

export async function getACPRuntime(botId: string, sessionId: string): Promise<AcpagentRuntimeStatus> {
  const { data } = await getBotsByBotIdSessionsBySessionIdAcpRuntime({
    path: { bot_id: botId.trim(), session_id: sessionId.trim() },
    throwOnError: true,
  })
  return data as AcpagentRuntimeStatus
}

export async function setACPRuntimeModel(botId: string, sessionId: string, modelId: string): Promise<AcpagentRuntimeStatus> {
  const { data } = await patchBotsByBotIdSessionsBySessionIdAcpRuntimeModel({
    path: { bot_id: botId.trim(), session_id: sessionId.trim() },
    body: { model_id: modelId },
    throwOnError: true,
  })
  return data as AcpagentRuntimeStatus
}

export async function deleteSession(botId: string, sessionId: string): Promise<void> {
  await deleteBotsByBotIdSessionsBySessionId({
    path: { bot_id: botId.trim(), session_id: sessionId.trim() },
    throwOnError: true,
  })
}

export async function deleteAllMessages(botId: string): Promise<void> {
  await deleteBotsByBotIdMessages({
    path: { bot_id: botId },
    throwOnError: true,
  })
}
