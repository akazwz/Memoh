<template>
  <div class="max-w-2xl mx-auto pb-6 space-y-5">
    <div class="flex items-center justify-between pb-4 border-b border-border/50">
      <div class="space-y-1">
        <h3 class="text-sm font-semibold text-foreground">
          {{ $t('bots.tabs.acp') }}
        </h3>
        <p class="text-[11px] text-muted-foreground">
          {{ $t('bots.settings.blocks.acpDescription') }}
        </p>
      </div>

      <div class="flex items-center gap-3 shrink-0">
        <Transition name="fade">
          <div
            v-if="hasChanges"
            class="flex items-center gap-1.5 px-2 py-0.5 rounded-full bg-muted/40 border border-border/50"
          >
            <div class="size-1 rounded-full bg-muted-foreground/40" />
            <span class="text-[10px] text-muted-foreground font-medium whitespace-nowrap">Unsaved</span>
          </div>
        </Transition>

        <Button
          size="sm"
          :disabled="!hasChanges || saveLoading"
          class="h-8 text-xs font-medium min-w-24 shadow-none"
          @click="handleSave"
        >
          <Spinner
            v-if="saveLoading"
            class="mr-1.5 size-3"
          />
          {{ $t('bots.settings.save') }}
        </Button>
      </div>
    </div>

    <SettingsAcpCard :form="form" />
  </div>
</template>

<script setup lang="ts">
import { computed, reactive, watch } from 'vue'
import { toast } from 'vue-sonner'
import { useI18n } from 'vue-i18n'
import { useMutation, useQuery, useQueryCache } from '@pinia/colada'
import { Button, Spinner } from '@memohai/ui'
import { getBotsById, putBotsById } from '@memohai/sdk'
import type { BotsUpdateBotRequest } from '@memohai/sdk'
import type { Ref } from 'vue'
import SettingsAcpCard from './settings-acp-card.vue'
import { resolveApiErrorMessage } from '@/utils/api-error'

const props = defineProps<{
  botId: string
}>()

const { t } = useI18n()
const queryCache = useQueryCache()
const botIdRef = computed(() => props.botId) as Ref<string>

const form = reactive({
  acp_codex_enabled: false,
})

const { data: bot } = useQuery({
  key: () => ['bot', botIdRef.value],
  query: async () => {
    const { data } = await getBotsById({ path: { id: botIdRef.value }, throwOnError: true })
    return data
  },
  enabled: () => !!botIdRef.value,
})

const { mutateAsync: updateBot, isLoading: saveLoading } = useMutation({
  mutation: async (body: BotsUpdateBotRequest) => {
    const { data } = await putBotsById({
      path: { id: botIdRef.value },
      body,
      throwOnError: true,
    })
    return data
  },
  onSettled: () => {
    queryCache.invalidateQueries({ key: ['bot', botIdRef.value] })
    queryCache.invalidateQueries({ key: ['bots'] })
  },
})

watch(bot, (value) => {
  form.acp_codex_enabled = readCodexACPEnabled(value?.metadata as Record<string, unknown> | undefined)
}, { immediate: true })

const hasChanges = computed(() =>
  !!bot.value &&
  form.acp_codex_enabled !== readCodexACPEnabled(bot.value.metadata as Record<string, unknown> | undefined),
)

async function handleSave() {
  try {
    await updateBot({
      metadata: withCodexACPEnabled(
        bot.value?.metadata as Record<string, unknown> | undefined,
        form.acp_codex_enabled,
      ),
    })
    toast.success(t('bots.settings.saveSuccess'))
  } catch (error) {
    toast.error(resolveApiErrorMessage(error, t('common.saveFailed')))
  }
}

const ACP_METADATA_KEY = 'acp'

function isRecord(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === 'object' && !Array.isArray(value)
}

function readCodexACPEnabled(metadata: Record<string, unknown> | undefined): boolean {
  if (!isRecord(metadata)) return false
  const acp = metadata[ACP_METADATA_KEY]
  if (!isRecord(acp)) return false
  const agents = acp.agents
  if (isRecord(agents)) {
    const codex = agents.codex
    if (typeof codex === 'boolean') return codex
    if (isRecord(codex) && typeof codex.enabled === 'boolean') return codex.enabled
  }
  if (Array.isArray(acp.enabled_agents)) {
    return acp.enabled_agents.some((agent) => typeof agent === 'string' && agent.trim().toLowerCase() === 'codex')
  }
  return typeof acp.codex_enabled === 'boolean' ? acp.codex_enabled : false
}

function withCodexACPEnabled(metadata: Record<string, unknown> | undefined, enabled: boolean): Record<string, unknown> {
  const nextMetadata = isRecord(metadata) ? { ...metadata } : {}
  const currentACP = isRecord(nextMetadata[ACP_METADATA_KEY]) ? nextMetadata[ACP_METADATA_KEY] : {}
  const acp = { ...currentACP }
  const currentAgents = isRecord(acp.agents) ? acp.agents : {}
  const agents = { ...currentAgents }
  const currentCodex = isRecord(agents.codex) ? agents.codex : {}
  agents.codex = { ...currentCodex, enabled }
  acp.agents = agents
  delete acp.codex_enabled
  nextMetadata[ACP_METADATA_KEY] = acp
  return nextMetadata
}
</script>
