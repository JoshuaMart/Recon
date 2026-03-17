<template>
  <div class="min-h-screen bg-bg">
    <!-- Mobile header -->
    <header class="lg:hidden fixed top-0 left-0 right-0 h-14 bg-bg-sidebar border-b border-border flex items-center justify-between px-4 z-50">
      <h1 class="text-sm font-bold tracking-wider text-text-primary">BUG_LOG</h1>
      <button
        class="p-2 text-text-secondary hover:text-text-primary transition-colors"
        @click="sidebarOpen = !sidebarOpen"
      >
        <span class="text-lg">{{ sidebarOpen ? '✕' : '☰' }}</span>
      </button>
    </header>

    <!-- Overlay -->
    <div
      v-if="sidebarOpen"
      class="lg:hidden fixed inset-0 bg-black/30 z-40"
      @click="sidebarOpen = false"
    />

    <!-- Sidebar -->
    <aside
      class="fixed top-0 left-0 h-full w-60 bg-bg-sidebar border-r border-border flex flex-col z-50 transition-transform duration-200"
      :class="sidebarOpen ? 'translate-x-0' : '-translate-x-full lg:translate-x-0'"
    >
      <!-- Logo -->
      <div class="px-6 py-4">
        <h1 class="text-lg font-bold tracking-wider text-text-primary">BUG_LOG</h1>
        <p class="text-xs text-text-secondary mt-1 tracking-wide">recon platform</p>
      </div>

      <!-- Navigation -->
      <nav class="flex-1 px-3">
        <ul class="space-y-1">
          <li v-for="item in navItems" :key="item.path">
            <NuxtLink
              :to="item.path"
              class="flex items-center gap-3 px-3 py-2.5 text-sm transition-colors duration-100 rounded-sm"
              :class="isActive(item.path)
                ? 'text-accent font-semibold border-l-2 border-accent bg-bg-card'
                : 'text-text-secondary hover:text-text-primary hover:bg-bg-card'"
              @click="sidebarOpen = false"
            >
              <span class="text-xs w-5 text-center opacity-60">{{ item.icon }}</span>
              {{ item.label }}
            </NuxtLink>
          </li>
        </ul>
      </nav>

      <!-- Logout -->
      <div class="px-3 pb-6">
        <button
          class="btn-ghost w-full text-left flex items-center gap-3"
          @click="handleLogout"
        >
          <span class="text-xs w-5 text-center opacity-60">→</span>
          Logout
        </button>
      </div>
    </aside>

    <!-- Main content -->
    <main class="min-h-screen pt-14 px-4 py-6 lg:pt-0 lg:ml-60 lg:px-12 lg:py-10">
      <slot />
    </main>
  </div>
</template>

<script setup lang="ts">
const route = useRoute()
const auth = useAuthStore()

const sidebarOpen = ref(false)

watch(
  () => route.path,
  () => {
    sidebarOpen.value = false
  },
)

const navItems = [
  { path: '/', label: 'Dashboard', icon: '◉' },
  { path: '/wildcards', label: 'Wildcards', icon: '✱' },
  { path: '/hostnames', label: 'Hostnames', icon: '◈' },
  { path: '/web-services', label: 'Web Services', icon: '◆' },
  { path: '/jobs', label: 'Jobs', icon: '⚙' },
]

function isActive(path: string): boolean {
  if (path === '/') return route.path === '/'
  return route.path.startsWith(path)
}

function handleLogout() {
  auth.logout()
}
</script>
