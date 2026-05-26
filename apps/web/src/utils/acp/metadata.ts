import type { AcpprofileManagedField, AcpprofilePublicProfile } from '@memohai/sdk'

export const ACP_NO_PROJECT_MODE = 'none'
export const ACP_NO_PROJECT_ROOT = '/data/.memoh/acp-work/no-project'

export interface ACPAgentForm {
  enabled: boolean
  setup_mode: string
  managed: Record<string, string>
}

export interface ACPForm {
  agents: Record<string, ACPAgentForm>
}

export interface ACPAgentConfig {
  setupMode: string
  managed: Record<string, unknown>
}

export interface MissingACPRequiredField {
  profile: AcpprofilePublicProfile
  field: AcpprofileManagedField
}

export function readACPConfig(metadata: Record<string, unknown> | undefined, profiles: AcpprofilePublicProfile[]): ACPForm {
  const out: ACPForm = { agents: {} }
  const acp = isRecord(metadata?.acp) ? metadata.acp : {}
  const agents = isRecord(acp.agents) ? acp.agents : {}
  for (const profile of profiles) {
    const id = normalizeACPAgentID(profile.id)
    if (!id) continue
    const defaults = emptyACPAgentForm(profile)
    const raw = agents[id]
    if (typeof raw === 'boolean') {
      out.agents[id] = { ...defaults, enabled: raw }
      continue
    }
    const record = isRecord(raw) ? raw : {}
    const managed = isRecord(record.managed) ? record.managed : {}
    out.agents[id] = {
      enabled: typeof record.enabled === 'boolean' ? record.enabled : legacyEnabled(acp, id),
      setup_mode: typeof record.setup_mode === 'string' && record.setup_mode.trim() ? record.setup_mode.trim() : defaults.setup_mode,
      managed: fieldsFromProfile(profile, managed),
    }
  }
  return out
}

export function normalizeACPForm(source: ACPForm, profiles: AcpprofilePublicProfile[]): ACPForm {
  const out: ACPForm = { agents: {} }
  for (const profile of profiles) {
    const id = normalizeACPAgentID(profile.id)
    if (!id) continue
    const agent = source.agents[id] ?? emptyACPAgentForm(profile)
    out.agents[id] = {
      enabled: !!agent.enabled,
      setup_mode: agent.setup_mode || defaultSetupMode(profile),
      managed: fieldsFromProfile(profile, agent.managed),
    }
  }
  return out
}

export function withACPMetadata(metadata: Record<string, unknown> | undefined, acpForm: ACPForm, profiles: AcpprofilePublicProfile[] = []): Record<string, unknown> {
  const nextMetadata = isRecord(metadata) ? { ...metadata } : {}
  const currentACP = isRecord(nextMetadata.acp) ? nextMetadata.acp : {}
  const acp: Record<string, unknown> = { ...currentACP }
  const currentAgents = isRecord(acp.agents) ? acp.agents : {}
  acp.agents = {
    ...currentAgents,
    ...serializeACPAgents(metadata, acpForm, profiles),
  }
  delete acp.codex_enabled
  delete acp.enabled_agents
  nextMetadata.acp = acp
  return nextMetadata
}

export function findMissingRequiredACPField(value: ACPForm, profiles: AcpprofilePublicProfile[], isLocalWorkspace = false): MissingACPRequiredField | null {
  if (isLocalWorkspace) return null
  for (const profile of profiles) {
    const id = normalizeACPAgentID(profile.id)
    if (!id) continue
    const agent = value.agents[id]
    if (!agent?.enabled || agent.setup_mode !== 'managed') continue
    const field = findMissingRequiredManagedField(profile, agent.managed, agent.setup_mode)
    if (field) return { profile, field }
  }
  return null
}

export function findMissingRequiredManagedField(profile: AcpprofilePublicProfile | null | undefined, managed: Record<string, unknown>, setupMode: string): AcpprofileManagedField | null {
  if (!profile || setupMode !== 'managed') return null
  for (const field of profile.managed_fields ?? []) {
    const id = normalizeACPAgentID(field.id)
    if (!id || !field.required) continue
    if (!String(managed[id] ?? '').trim()) return field
  }
  return null
}

export function readACPAgentConfig(metadata: Record<string, unknown> | undefined, rawAgentID: string | undefined): ACPAgentConfig {
  const agentID = normalizeACPAgentID(rawAgentID)
  const acp = isRecord(metadata?.acp) ? metadata.acp : {}
  const agents = isRecord(acp.agents) ? acp.agents : {}
  const raw = agentID ? agents[agentID] : undefined
  const record = isRecord(raw) ? raw : {}
  return {
    setupMode: typeof record.setup_mode === 'string' && record.setup_mode.trim() ? record.setup_mode.trim().toLowerCase() : 'managed',
    managed: isRecord(record.managed) ? record.managed : {},
  }
}

export function isACPAgentEnabled(metadata: Record<string, unknown> | undefined, rawAgentID: unknown): boolean {
  const agentID = normalizeACPAgentID(rawAgentID)
  if (!agentID || !metadata) return false
  const acp = isRecord(metadata.acp) ? metadata.acp : {}
  const agents = isRecord(acp.agents) ? acp.agents : {}
  const raw = agents[agentID]
  if (typeof raw === 'boolean') return raw
  if (isRecord(raw) && typeof raw.enabled === 'boolean') return raw.enabled
  return legacyEnabled(acp, agentID)
}

export function isACPNoProject(metadata: Record<string, unknown> | undefined): boolean {
  return metadata?.acp_project_mode === ACP_NO_PROJECT_MODE
}

export function createACPNoProjectPath(): string {
  return `${ACP_NO_PROJECT_ROOT}/${randomID()}`
}

export function emptyACPAgentForm(profile: AcpprofilePublicProfile): ACPAgentForm {
  return {
    enabled: false,
    setup_mode: defaultSetupMode(profile),
    managed: fieldsFromProfile(profile, {}),
  }
}

export function ensureACPAgentForm(form: ACPForm, profile: AcpprofilePublicProfile): ACPAgentForm {
  const id = normalizeACPAgentID(profile.id)
  if (!id) return emptyACPAgentForm(profile)
  if (!form.agents[id]) {
    form.agents[id] = emptyACPAgentForm(profile)
  }
  return form.agents[id]
}

export function fieldsFromProfile(profile: AcpprofilePublicProfile, source: Record<string, unknown>): Record<string, string> {
  const values: Record<string, string> = {}
  for (const field of profile.managed_fields ?? []) {
    const id = normalizeACPAgentID(field.id)
    if (!id) continue
    const value = source[id]
    values[id] = typeof value === 'string' ? value : ''
  }
  return values
}

export function defaultSetupMode(profile: AcpprofilePublicProfile): string {
  return profile.setup_modes?.includes('managed') ? 'managed' : (profile.setup_modes?.[0] ?? 'managed')
}

export function normalizeACPAgentID(value: unknown): string {
  return typeof value === 'string' ? value.trim().toLowerCase() : ''
}

function legacyEnabled(acp: Record<string, unknown>, id: string): boolean {
  if (Array.isArray(acp.enabled_agents) && acp.enabled_agents.some((item) => normalizeACPAgentID(item) === id)) return true
  if (id === 'codex' && typeof acp.codex_enabled === 'boolean') return acp.codex_enabled
  return false
}

function serializeACPAgents(metadata: Record<string, unknown> | undefined, acpForm: ACPForm, profiles: AcpprofilePublicProfile[]): Record<string, unknown> {
  const profileByID = new Map(profiles.map(profile => [normalizeACPAgentID(profile.id), profile]))
  const out: Record<string, unknown> = {}
  for (const [rawAgentID, agent] of Object.entries(acpForm.agents)) {
    const agentID = normalizeACPAgentID(rawAgentID)
    const profile = profileByID.get(agentID)
    const managed: Record<string, unknown> = { ...agent.managed }
    if (profile) {
      const existingManaged = existingManagedFields(metadata, agentID)
      for (const field of profile.managed_fields ?? []) {
        const fieldID = normalizeACPAgentID(field.id)
        if (!fieldID || !isSensitiveManagedField(field)) continue
        const value = managed[fieldID]
        const existing = existingManaged[fieldID]
        if (typeof value === 'string' && value.trim() === '' && typeof existing === 'string' && existing.trim() !== '') {
          managed[fieldID] = null
        }
      }
    }
    out[agentID || rawAgentID] = {
      enabled: !!agent.enabled,
      setup_mode: agent.setup_mode,
      managed,
    }
  }
  return out
}

function existingManagedFields(metadata: Record<string, unknown> | undefined, agentID: string): Record<string, unknown> {
  const acp = isRecord(metadata?.acp) ? metadata.acp : {}
  const agents = isRecord(acp.agents) ? acp.agents : {}
  const agent = isRecord(agents[agentID]) ? agents[agentID] : {}
  return isRecord(agent.managed) ? agent.managed : {}
}

function isSensitiveManagedField(field: AcpprofileManagedField): boolean {
  return field.sensitive === true || field.type === 'password'
}

function randomID(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === 'object' && !Array.isArray(value)
}
