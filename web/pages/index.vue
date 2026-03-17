<template>
  <div class="pt-4">
    <PageTitle title="Dashboard">
      <template #subtitle>Overview of your recon infrastructure.</template>
    </PageTitle>

    <!-- Stats strip -->
    <div class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-6 gap-3 mb-10">
      <div v-for="stat in stats" :key="stat.label" class="bg-bg-card border border-border p-4">
        <p class="text-xs text-text-secondary uppercase tracking-wider mb-1">{{ stat.label }}</p>
        <p class="text-2xl font-bold" :class="stat.color || 'text-text-primary'">{{ stat.value }}</p>
      </div>
    </div>

    <!-- Wildcards table -->
    <div class="mb-10">
      <h3 class="section-title">Wildcards</h3>
      <div v-if="loadingWildcards" class="text-sm text-text-secondary">Loading...</div>
      <div v-else-if="wildcards.length === 0" class="text-sm text-text-secondary">No wildcards configured.</div>
      <div v-else class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-border text-left text-xs text-text-secondary uppercase tracking-wider">
              <th class="pb-3 pr-4">Wildcard</th>
              <th class="pb-3 pr-4 hidden sm:table-cell">Hostnames</th>
              <th class="pb-3 pr-4 hidden md:table-cell">Alive</th>
              <th class="pb-3 pr-4 hidden md:table-cell">Dead</th>
              <th class="pb-3 pr-4 hidden lg:table-cell">Web</th>
              <th class="pb-3 pr-4 hidden lg:table-cell">Last Recon</th>
              <th class="pb-3">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="w in wildcards" :key="w.id" class="table-row">
              <td class="py-3 pr-4">
                <span class="font-medium">{{ w.value }}</span>
                <span v-if="!w.active" class="ml-2 text-xs text-text-secondary">(inactive)</span>
              </td>
              <td class="py-3 pr-4 hidden sm:table-cell">{{ w.stats.hostnames_total }}</td>
              <td class="py-3 pr-4 hidden md:table-cell">
                <span class="text-emerald-600">{{ w.stats.hostnames_alive }}</span>
              </td>
              <td class="py-3 pr-4 hidden md:table-cell">
                <span class="text-red-600">{{ w.stats.hostnames_dead }}</span>
              </td>
              <td class="py-3 pr-4 hidden lg:table-cell">{{ w.stats.web_services }}</td>
              <td class="py-3 pr-4 hidden lg:table-cell text-text-secondary">
                {{ w.last_recon_at ? timeAgo(w.last_recon_at) : '—' }}
              </td>
              <td class="py-3">
                <div class="flex gap-2">
                  <button
                    class="btn-ghost text-xs px-2 py-1 inline-flex items-center gap-1.5"
                    :disabled="reconLoading === w.id"
                    title="Launch a new recon job for this wildcard"
                    @click="launchRecon(w.id)"
                  >
                    <Loader2 v-if="reconLoading === w.id" :size="13" class="animate-spin" />
                    <Play v-else :size="13" />
                    Recon
                  </button>
                  <button
                    class="btn-ghost text-xs px-2 py-1 inline-flex items-center gap-1.5"
                    :disabled="revalidateLoading === w.id"
                    title="Re-check all hostnames status for this wildcard"
                    @click="revalidate(w.id)"
                  >
                    <Loader2 v-if="revalidateLoading === w.id" :size="13" class="animate-spin" />
                    <RefreshCw v-else :size="13" />
                    Revalidate
                  </button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Recent jobs -->
    <div>
      <h3 class="section-title">Recent Jobs</h3>
      <div v-if="loadingJobs" class="text-sm text-text-secondary">Loading...</div>
      <div v-else-if="recentJobs.length === 0" class="text-sm text-text-secondary">No jobs yet.</div>
      <div v-else class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-border text-left text-xs text-text-secondary uppercase tracking-wider">
              <th class="pb-3 pr-4">Wildcard</th>
              <th class="pb-3 pr-4">Mode</th>
              <th class="pb-3 pr-4">Status</th>
              <th class="pb-3 pr-4 hidden sm:table-cell">Created</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="job in recentJobs" :key="job.id" class="table-row">
              <td class="py-3 pr-4 font-medium">{{ wildcardValue(job.wildcard_id) }}</td>
              <td class="py-3 pr-4">{{ job.mode }}</td>
              <td class="py-3 pr-4">
                <span :class="jobBadgeClass(job.status)">
                  <span :class="jobDotClass(job.status)" class="status-dot" />
                  {{ job.status }}
                </span>
              </td>
              <td class="py-3 pr-4 text-text-secondary hidden sm:table-cell">
                {{ timeAgo(job.created_at) }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Action error toast -->
    <div
      v-if="actionError"
      class="fixed bottom-6 right-6 bg-bg-card border border-accent text-accent text-sm px-4 py-3 z-50"
    >
      {{ actionError }}
    </div>
  </div>
</template>

<script setup lang="ts">
import type { PaginatedResponse, ReconJob, Wildcard } from '~/types/api'

const { api } = useApi()

const wildcards = ref<Wildcard[]>([])
const recentJobs = ref<ReconJob[]>([])
const loadingWildcards = ref(true)
const loadingJobs = ref(true)
const reconLoading = ref<string | null>(null)
const revalidateLoading = ref<string | null>(null)
const actionError = ref('')

const stats = computed(() => {
  const totals = wildcards.value.reduce(
    (acc, w) => {
      acc.hostnames += w.stats.hostnames_total
      acc.alive += w.stats.hostnames_alive
      acc.dead += w.stats.hostnames_dead
      acc.unreachable += w.stats.hostnames_unreachable
      acc.web += w.stats.web_services
      return acc
    },
    { hostnames: 0, alive: 0, dead: 0, unreachable: 0, web: 0 },
  )

  return [
    { label: 'Wildcards', value: wildcards.value.length },
    { label: 'Hostnames', value: totals.hostnames },
    { label: 'Alive', value: totals.alive, color: 'text-emerald-600' },
    { label: 'Dead', value: totals.dead, color: 'text-red-600' },
    { label: 'Unreachable', value: totals.unreachable, color: 'text-amber-600' },
    { label: 'Web Services', value: totals.web },
  ]
})

function wildcardValue(id: string): string {
  return wildcards.value.find((w) => w.id === id)?.value || id.slice(0, 8)
}

function timeAgo(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime()
  const minutes = Math.floor(diff / 60000)
  if (minutes < 1) return 'just now'
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  return `${days}d ago`
}

function jobBadgeClass(status: string): string {
  const base = 'inline-flex items-center gap-1.5 px-2.5 py-0.5 text-xs font-medium rounded-full'
  const map: Record<string, string> = {
    pending: `${base} bg-gray-100 text-gray-700`,
    running: `${base} bg-blue-100 text-blue-700`,
    completed: `${base} bg-emerald-100 text-emerald-700`,
    failed: `${base} bg-red-100 text-red-700`,
  }
  return map[status] || base
}

function jobDotClass(status: string): string {
  const map: Record<string, string> = {
    pending: 'bg-gray-500',
    running: 'bg-blue-500',
    completed: 'bg-emerald-500',
    failed: 'bg-red-500',
  }
  return map[status] || 'bg-gray-500'
}

function showError(msg: string) {
  actionError.value = msg
  setTimeout(() => {
    actionError.value = ''
  }, 4000)
}

async function fetchData() {
  try {
    wildcards.value = await api<Wildcard[]>('/api/wildcards')
  } catch {
    wildcards.value = []
  } finally {
    loadingWildcards.value = false
  }

  try {
    const res = await api<PaginatedResponse<ReconJob>>('/api/jobs', {
      params: { per_page: 5 },
    })
    recentJobs.value = res.data
  } catch {
    recentJobs.value = []
  } finally {
    loadingJobs.value = false
  }
}

async function launchRecon(wildcardId: string) {
  reconLoading.value = wildcardId
  try {
    await api(`/api/wildcards/${wildcardId}/recon`, {
      method: 'POST',
      body: { mode: 'normal' },
    })
    await fetchData()
  } catch (e: any) {
    showError(e?.data?.error || 'Failed to launch recon')
  } finally {
    reconLoading.value = null
  }
}

async function revalidate(wildcardId: string) {
  revalidateLoading.value = wildcardId
  try {
    await api(`/api/wildcards/${wildcardId}/revalidate`, { method: 'PUT' })
    await fetchData()
  } catch (e: any) {
    showError(e?.data?.error || 'Failed to revalidate')
  } finally {
    revalidateLoading.value = null
  }
}

onMounted(fetchData)
</script>
