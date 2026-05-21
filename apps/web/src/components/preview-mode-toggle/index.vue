<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { Eye, Code } from 'lucide-vue-next'
import { Tabs, TabsList, TabsTrigger } from '@memohai/ui'

export type PreviewMode = 'preview' | 'raw'

const props = withDefaults(defineProps<{
  modelValue: PreviewMode
  previewLabel?: string
  rawLabel?: string
  class?: string
}>(), {
  previewLabel: undefined,
  rawLabel: undefined,
  class: undefined,
})

const emit = defineEmits<{
  'update:modelValue': [value: PreviewMode]
}>()

const { t } = useI18n()

const previewText = computed(() => props.previewLabel ?? t('common.preview'))
const rawText = computed(() => props.rawLabel ?? t('common.raw'))

const current = computed({
  get: () => props.modelValue,
  set: (value) => emit('update:modelValue', value as PreviewMode),
})
</script>

<template>
  <Tabs
    v-model="current"
    :class="['inline-flex', props.class]"
  >
    <TabsList class="h-7 p-0.5 gap-0.5">
      <TabsTrigger
        value="preview"
        class="h-6 gap-1 px-2 text-[11px]"
        :aria-label="previewText"
      >
        <Eye class="size-3.5" />
        {{ previewText }}
      </TabsTrigger>
      <TabsTrigger
        value="raw"
        class="h-6 gap-1 px-2 text-[11px]"
        :aria-label="rawText"
      >
        <Code class="size-3.5" />
        {{ rawText }}
      </TabsTrigger>
    </TabsList>
  </Tabs>
</template>
