import { expect, it } from 'vitest'
import { botAgentRuntimeForProvider, botAgentRuntimeOptions, directBotAgentMetadata, isDirectBotAgentConfigured, isDirectBotAgentRuntime, sessionAgentProvider } from './bot-agent'

it('keeps Grok direct identity across selectors, creation and restored sessions', () => {
  expect(isDirectBotAgentRuntime('grok')).toBe(true)
  expect(botAgentRuntimeForProvider('grok')).toBe('grok')
  expect(botAgentRuntimeOptions([{ id: 'grok', display_name: 'ACP collision' }]).filter(option => option.value === 'grok')).toHaveLength(1)
  expect(directBotAgentMetadata('grok')).toEqual({ provider: 'grok', auth: 'oauth' })
  expect(sessionAgentProvider('grok', { acp_agent_id: 'other' }, {})).toBe('grok')
})
it('requires the matching Grok credential mode before starting a session', () => {
  expect(isDirectBotAgentConfigured({ runtime: 'grok', metadata: { auth: 'oauth' } })).toBe(false)
  expect(isDirectBotAgentConfigured({ runtime: 'grok', metadata: { auth: 'api_key' }, agent_credential_id: 'credential' })).toBe(true)
  expect(isDirectBotAgentConfigured({ runtime: 'grok', metadata: { auth: 'workspace' }, agent_credential_id: 'credential' })).toBe(false)
})
