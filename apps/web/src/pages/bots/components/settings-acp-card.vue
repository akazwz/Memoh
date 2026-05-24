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

    <div class="flex items-center justify-between gap-4 rounded-md border border-border p-3 shadow-none bg-background">
      <div class="space-y-0.5">
        <Label class="text-xs font-medium text-foreground">{{ $t('bots.settings.codexACP') }}</Label>
        <p class="text-[10px] text-muted-foreground">
          {{ $t('bots.settings.codexACPDescription') }}
        </p>
      </div>
      <Switch
        :model-value="form.acp_codex_enabled"
        class="shadow-none scale-[0.8] origin-right"
        @update:model-value="(val) => form.acp_codex_enabled = !!val"
      />
    </div>
  </div>
</template>

<script setup lang="ts">
import { Label, Switch } from '@memohai/ui'

defineProps<{
  form: {
    acp_codex_enabled: boolean
  }
}>()
</script>
