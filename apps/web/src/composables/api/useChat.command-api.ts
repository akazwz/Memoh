import { getCommands } from '@memohai/sdk'
import type { CommandCommandManifest } from '@memohai/sdk'
import type { CommandManifest } from './useChat.types'

export async function fetchCommandManifests(
  botId?: string,
  sessionId?: string,
  scope = 'web',
): Promise<CommandManifest[]> {
  const query: Record<string, string> = { scope }
  const normalizedBotId = botId?.trim()
  const normalizedSessionId = sessionId?.trim()
  if (normalizedBotId) query.bot_id = normalizedBotId
  if (normalizedSessionId) query.session_id = normalizedSessionId

  const { data } = await getCommands({
    query,
    throwOnError: true,
  })

  return (data?.commands ?? []).map(normalizeCommandManifest)
}

function normalizeCommandManifest(command: CommandCommandManifest): CommandManifest {
  return {
    id: command.id ?? '',
    command: command.command ?? '',
    insert_text: command.insert_text ?? '',
    title: command.title ?? '',
    description: command.description,
    source: command.source ?? '',
    plugin_id: command.plugin_id,
    plugin_name: command.plugin_name,
    capability: command.capability ?? '',
    action: command.action ?? '',
    icon: command.icon,
    enabled: command.enabled ?? false,
    scopes: command.scopes,
  }
}
