<template>
  <div class="mb-8">
    <h1 class="text-lg font-bold uppercase tracking-widest text-text-primary">
      {{ title }}
    </h1>
    <p v-if="$slots.subtitle" class="text-sm text-text-secondary mt-1">
      <slot name="subtitle" />
    </p>
    <div class="mt-3 border-b border-border" />
  </div>
</template>

<script setup lang="ts">
defineProps<{
  title: string
}>()
</script>
