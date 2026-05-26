import type { Component } from 'vue'
import { Bot as BotIcon } from 'lucide-vue-next'
import { Codex, CodexColor } from '@memohai/icon'
import { normalizeACPAgentID } from './metadata'

export function acpAgentIcon(agentID: unknown, color = false): Component {
  return isCodexAgent(agentID)
    ? (color ? CodexColor : Codex)
    : BotIcon
}

export function isCodexAgent(agentID: unknown): boolean {
  return normalizeACPAgentID(agentID) === 'codex'
}

export function acpAgentDisplayName(agentID: unknown, fallback = ''): string {
  const normalized = normalizeACPAgentID(agentID)
  if (!normalized) return fallback
  if (isCodexAgent(normalized)) return 'Codex'
  return typeof agentID === 'string' ? agentID.trim() : normalized
}
