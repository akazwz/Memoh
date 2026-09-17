<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { InlineLoadingRow } from '@felinic/ui'
import { useAgentModelCatalog } from '@/composables/useAgentModelCatalog'
import { resolveApiErrorMessage } from '@/utils/api-error'
import ModelSelect from './model-select.vue'

const props = defineProps<{ botId: string, agentId: string }>()
const model = defineModel<string>({ required: true })
const { t } = useI18n()
const { catalog, isLoading, error } = useAgentModelCatalog({
  botId: () => props.botId,
  botAgentId: () => props.agentId,
  runtime: 'opencode',
  selectedModelId: model,
  projectPath: '/data',
  acpModels: [], acpCurrentModelId: '', acpReasoningEfforts: [],
  acpCurrentReasoningEffort: '', acpLoading: false,
})
</script>

<template>
  <InlineLoadingRow v-if="isLoading">
    {{ t('bots.schedule.execution.loadingAgentModels') }}
  </InlineLoadingRow>
  <p
    v-else-if="error"
    class="text-caption text-destructive"
    role="alert"
  >
    {{ resolveApiErrorMessage(error, t('bots.agent.openCodeModelsLoadFailed')) }}
  </p>
  <ModelSelect
    v-else
    v-model="model"
    :models="catalog.models"
    :providers="catalog.providers"
    model-type="chat"
    :placeholder="t('bots.schedule.execution.agentDefaultModel')"
    :none-label="t('bots.schedule.execution.agentDefaultModel')"
  />
</template>
