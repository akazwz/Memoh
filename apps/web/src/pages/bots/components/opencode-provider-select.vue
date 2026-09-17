<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Input, Select, SelectContent, SelectItem, SelectTrigger, SelectValue, SettingsRow } from '@felinic/ui'
import { isKnownOpenCodeProvider, OPENCODE_PROVIDERS } from '@/utils/opencode-provider'

defineProps<{ disabled?: boolean }>()
const providerID = defineModel<string>({ required: true })
const custom = ref(!isKnownOpenCodeProvider(providerID.value))
watch(providerID, value => { custom.value = !isKnownOpenCodeProvider(value) })
const selection = computed({
  get: () => custom.value ? '__custom__' : providerID.value,
  set: (value: string) => {
    custom.value = value === '__custom__'
    providerID.value = custom.value ? '' : value
  },
})
</script>

<template>
  <SettingsRow
    :label="$t('bots.agent.openCodeProvider')"
    :description="providerID === 'opencode-go' ? $t('bots.agent.openCodeGoDescription') : providerID === 'opencode' ? $t('bots.agent.openCodeZenDescription') : undefined"
    stack="sm"
  >
    <Select
      v-model="selection"
      :disabled="disabled"
    >
      <SelectTrigger
        class="w-full sm:w-56"
        :aria-label="$t('bots.agent.openCodeProvider')"
      >
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem
          v-for="provider in OPENCODE_PROVIDERS"
          :key="provider.id"
          :value="provider.id"
        >
          {{ provider.name }}
        </SelectItem>
        <SelectItem value="__custom__">
          {{ $t('bots.agent.openCodeCustomProvider') }}
        </SelectItem>
      </SelectContent>
    </Select>
  </SettingsRow>
  <SettingsRow
    v-if="custom"
    :label="$t('bots.agent.openCodeProviderID')"
    :description="$t('bots.agent.openCodeProviderDescription')"
    stack="sm"
  >
    <Input
      v-model="providerID"
      class="w-full sm:w-56"
      :aria-label="$t('bots.agent.openCodeProviderID')"
      :disabled="disabled"
      placeholder="my-provider"
    />
  </SettingsRow>
</template>
