export const DEFAULT_OPENCODE_PROVIDER = 'opencode-go'

export const OPENCODE_PROVIDERS = [
  { id: 'opencode-go', name: 'OpenCode Go' },
  { id: 'opencode', name: 'OpenCode Zen' },
  { id: 'anthropic', name: 'Anthropic' },
  { id: 'openai', name: 'OpenAI' },
  { id: 'google', name: 'Google' },
  { id: 'openrouter', name: 'OpenRouter' },
] as const

export function isKnownOpenCodeProvider(id: string): boolean {
  return OPENCODE_PROVIDERS.some(provider => provider.id === id)
}
