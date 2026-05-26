<!-- eslint-disable vue/no-mutating-props -->
<template>
  <div class="space-y-4 rounded-md border border-border bg-background p-4 shadow-none">
    <div class="space-y-0.5">
      <h4 class="text-xs font-medium text-foreground">
        {{ $t('bots.settings.blocks.acp') }}
      </h4>
      <p class="text-[11px] text-muted-foreground">
        {{ $t('bots.settings.blocks.acpDescription') }}
      </p>
    </div>

    <div
      v-if="loading"
      class="flex items-center gap-2 rounded-md border border-border p-3 text-xs text-muted-foreground"
    >
      <LoaderCircle class="size-3 animate-spin" />
      {{ $t('common.loading') }}
    </div>

    <div
      v-else-if="profiles.length === 0"
      class="rounded-md border border-border p-3 text-xs text-muted-foreground"
    >
      {{ $t('common.noData') }}
    </div>

    <template v-else>
      <div
        v-for="profile in profiles"
        :key="profile.id"
        class="space-y-4 rounded-md border border-border p-3"
      >
        <div class="flex items-start justify-between gap-4">
          <div class="min-w-0 space-y-0.5">
            <div class="flex min-w-0 items-center gap-2">
              <component
                :is="acpAgentIcon(profile.id, true)"
                class="size-4 shrink-0"
              />
              <Label class="truncate text-xs font-medium text-foreground">
                {{ profile.display_name || profile.id }}
              </Label>
            </div>
            <p
              v-if="profile.description"
              class="text-[10px] text-muted-foreground"
            >
              {{ profile.description }}
            </p>
          </div>
          <Switch
            :model-value="agentForm(profile).enabled"
            class="origin-right scale-[0.8] shadow-none"
            @update:model-value="(val) => agentForm(profile).enabled = !!val"
          />
        </div>

        <div
          v-if="agentForm(profile).enabled"
          class="space-y-3 border-t border-border/70 pt-3"
        >
          <div
            v-if="isLocalWorkspace"
            class="rounded-md border border-border/70 bg-muted/30 px-3 py-2 text-[11px] text-muted-foreground"
          >
            {{ $t('bots.settings.acpLocalModeHint') }}
          </div>

          <template v-else>
            <div class="space-y-1.5">
              <Label class="text-xs font-medium text-foreground">
                {{ $t('bots.settings.acpSetupMode') }}
              </Label>
              <div class="grid grid-cols-2 gap-2">
                <button
                  v-for="mode in setupModes(profile)"
                  :key="mode"
                  type="button"
                  class="h-8 rounded-md border px-3 text-xs font-medium transition-colors"
                  :class="agentForm(profile).setup_mode === mode ? 'border-foreground bg-foreground text-background' : 'border-border bg-background text-foreground hover:bg-muted'"
                  @click="agentForm(profile).setup_mode = mode"
                >
                  {{ setupModeLabel(mode) }}
                </button>
              </div>
            </div>

            <div
              v-if="agentForm(profile).setup_mode === 'managed'"
              class="space-y-3"
            >
              <div
                v-for="field in profile.managed_fields ?? []"
                :key="field.id"
                class="space-y-1.5"
              >
                <Label class="text-xs font-medium text-foreground">
                  {{ field.label || field.id }}
                </Label>
                <Input
                  :model-value="agentForm(profile).managed[field.id || ''] || ''"
                  :type="inputType(field.type)"
                  :placeholder="field.placeholder"
                  class="h-8 text-xs shadow-none"
                  @update:model-value="(val) => setManagedField(profile, field.id, String(val ?? ''))"
                />
                <p
                  v-if="field.help"
                  class="text-[10px] text-muted-foreground"
                >
                  {{ field.help }}
                </p>
              </div>
            </div>

            <div
              v-else
              class="break-words rounded-md border border-border/70 bg-muted/30 px-3 py-2 text-[11px] text-muted-foreground"
            >
              {{ $t('bots.settings.acpSelfModeHint') }}
            </div>
          </template>
        </div>
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { Input, Label, Switch } from '@memohai/ui'
import { LoaderCircle } from 'lucide-vue-next'
import type { AcpprofilePublicProfile } from '@memohai/sdk'
import { acpAgentIcon, ensureACPAgentForm, normalizeACPAgentID, type ACPAgentForm, type ACPForm } from '@/utils/acp'

const props = defineProps<{
  profiles: AcpprofilePublicProfile[]
  form: ACPForm
  loading?: boolean
  isLocalWorkspace?: boolean
}>()

function agentForm(profile: AcpprofilePublicProfile): ACPAgentForm {
  return ensureACPAgentForm(props.form, profile)
}

function setupModes(profile: AcpprofilePublicProfile): string[] {
  const modes = profile.setup_modes?.filter(Boolean) ?? []
  return modes.length > 0 ? modes : ['managed']
}

function setupModeLabel(mode: string): string {
  if (mode === 'managed') return 'Managed'
  if (mode === 'self') return 'Self'
  return mode
}

function inputType(type: string | undefined): string {
  if (type === 'password') return 'password'
  if (type === 'url') return 'url'
  return 'text'
}

function setManagedField(profile: AcpprofilePublicProfile, fieldID: string | undefined, value: string) {
  const id = normalizeACPAgentID(fieldID)
  if (!id) return
  agentForm(profile).managed[id] = value
}
</script>
