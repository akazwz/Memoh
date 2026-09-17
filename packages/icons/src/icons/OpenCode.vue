<template>
  <svg
    xmlns="http://www.w3.org/2000/svg"
    :width="size"
    :height="size"
    viewBox="0 0 300 300"
    fill="currentColor"
    v-bind="$attrs"
  ><g transform="translate(30, 0)">
    <path
      d="M180 240H60V120H180V240Z"
      fill="currentColor"
      opacity="0.35"
    />
    <path
      d="M180 60H60V240H180V60ZM240 300H0V0H240V300Z"
      fill="currentColor"
    />
  </g></svg>
</template>

<script setup lang="ts">
withDefaults(defineProps<{ size?: string | number }>(), { size: '1em' })
defineOptions({ inheritAttrs: false })
</script>
