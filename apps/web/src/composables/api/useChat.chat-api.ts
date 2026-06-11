import {
  getBots,
  deleteBotsByBotIdMessages,
  deleteBotsByBotIdAcpRuntimesByRuntimeId,
  getBotsByBotIdSessions,
  getBotsByBotIdSessionsBySessionIdAcpRuntime,
  getBotsByBotIdSessionsBySessionIdAcpRuntimeTurn,
  postBotsByBotIdAcpRuntimes,
  postBotsByBotIdSessions,
  postBotsByBotIdSessionsBySessionIdAcpRuntime,
  postBotsByBotIdSessionsBySessionIdAcpRuntimeAbortTurn,
  deleteBotsByBotIdSessionsBySessionId,
  patchBotsByBotIdAcpRuntimesByRuntimeIdModel,
  patchBotsByBotIdSessionsBySessionId,
  patchBotsByBotIdSessionsBySessionIdAcpRuntimeModel,
} from '@memohai/sdk'
import type { AcpagentRuntimeStatus, FlowAcpTurnSnapshot } from '@memohai/sdk'
import type { Bot, SessionSummary } from './useChat.types'

export interface CreateSessionOptions {
  title?: string
  type?: string
  metadata?: Record<string, unknown>
  /** Warm pre-session ACP runtime to bind at creation time. */
  acpRuntimeId?: string
}

export interface CreateACPRuntimeOptions {
  agentId: string
  projectPath?: string
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
        acp_runtime_id: options?.acpRuntimeId?.trim() || undefined,
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

/**
 * Abort the session's in-flight ACP turn out-of-band: works from any client
 * or connection, not just the one that started the turn. The runtime stays
 * warm and the round persists as cancelled. Passing turnId makes the abort
 * precise — a stale client cannot kill a newer turn it never saw.
 */
export async function abortACPTurn(botId: string, sessionId: string, turnId?: string): Promise<AcpagentRuntimeStatus> {
  const { data } = await postBotsByBotIdSessionsBySessionIdAcpRuntimeAbortTurn({
    path: { bot_id: botId.trim(), session_id: sessionId.trim() },
    body: turnId?.trim() ? { turn_id: turnId.trim() } : {},
    throwOnError: true,
  })
  return data as AcpagentRuntimeStatus
}

/**
 * Reconnect backfill: the UI messages accumulated server-side for the
 * in-flight (or most recent) ACP turn, plus the turn identity used to dedupe
 * the live `acp_turn_stream` mirror and target an abort.
 */
export async function getACPTurnSnapshot(botId: string, sessionId: string): Promise<FlowAcpTurnSnapshot> {
  const { data } = await getBotsByBotIdSessionsBySessionIdAcpRuntimeTurn({
    path: { bot_id: botId.trim(), session_id: sessionId.trim() },
    throwOnError: true,
  })
  return data as FlowAcpTurnSnapshot
}

export async function setACPRuntimeModel(botId: string, sessionId: string, modelId: string): Promise<AcpagentRuntimeStatus> {
  const { data } = await patchBotsByBotIdSessionsBySessionIdAcpRuntimeModel({
    path: { bot_id: botId.trim(), session_id: sessionId.trim() },
    body: { model_id: modelId },
    throwOnError: true,
  })
  return data as AcpagentRuntimeStatus
}

export async function createACPRuntime(botId: string, options: CreateACPRuntimeOptions): Promise<AcpagentRuntimeStatus> {
  const { data } = await postBotsByBotIdAcpRuntimes({
    path: { bot_id: botId.trim() },
    body: {
      acp_agent_id: options.agentId.trim(),
      project_path: options.projectPath?.trim(),
    },
    throwOnError: true,
  })
  return data as AcpagentRuntimeStatus
}

export async function setACPRuntimeModelByID(botId: string, runtimeId: string, modelId: string): Promise<AcpagentRuntimeStatus> {
  const { data } = await patchBotsByBotIdAcpRuntimesByRuntimeIdModel({
    path: { bot_id: botId.trim(), runtime_id: runtimeId.trim() },
    // An empty model_id resets the runtime to the agent default model.
    body: { model_id: modelId.trim() },
    throwOnError: true,
  })
  return data as AcpagentRuntimeStatus
}

export async function closeACPRuntime(botId: string, runtimeId: string): Promise<void> {
  await deleteBotsByBotIdAcpRuntimesByRuntimeId({
    path: { bot_id: botId.trim(), runtime_id: runtimeId.trim() },
    throwOnError: true,
  })
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
