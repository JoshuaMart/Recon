<template>
  <div class="pt-4">
    <PageTitle title="Jobs">
      <template #subtitle>Recon job history and status.</template>
    </PageTitle>

    <!-- Filters -->
    <div class="flex flex-wrap gap-3 mb-8">
      <select
        v-model="filters.wildcard_id"
        class="bg-bg border border-border px-3 py-2 text-sm font-mono
               focus:outline-none focus:border-accent transition-colors"
        @change="resetAndFetch"
      >
        <option value="">All wildcards</option>
        <option v-for="w in wildcards" :key="w.id" :value="w.id">{{ w.value }}</option>
      </select>
      <select
        v-model="filters.status"
        class="bg-bg border border-border px-3 py-2 text-sm font-mono
               focus:outline-none focus:border-accent transition-colors"
        @change="resetAndFetch"
      >
        <option value="">All statuses</option>
        <option value="pending">Pending</option>
        <option value="running">Running</option>
        <option value="completed">Completed</option>
        <option value="failed">Failed</option>
      </select>
    </div>

    <!-- Table -->
    <div v-if="loading" class="text-sm text-text-secondary">Loading...</div>
    <div v-else-if="jobs.length === 0" class="text-sm text-text-secondary">No jobs found.</div>
    <div v-else>
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-border text-left text-xs text-text-secondary uppercase tracking-wider">
              <th class="pb-3 pr-4">Wildcard</th>
              <th class="pb-3 pr-4">Mode</th>
              <th class="pb-3 pr-4">Status</th>
              <th class="pb-3 pr-4 hidden md:table-cell">Job ID</th>
              <th class="pb-3 pr-4 hidden lg:table-cell">Started</th>
              <th class="pb-3 pr-4 hidden lg:table-cell">Completed</th>
              <th class="pb-3 hidden sm:table-cell">Created</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="job in jobs"
              :key="job.id"
              class="table-row cursor-pointer"
              @click="openDetail(job)"
            >
              <td class="py-3 pr-4 font-medium">{{ wildcardValue(job.wildcard_id) }}</td>
              <td class="py-3 pr-4">{{ job.mode }}</td>
              <td class="py-3 pr-4">
                <span :class="statusBadgeClass(job.status)">
                  <span :class="statusDotClass(job.status)" class="status-dot" />
                  {{ job.status }}
                </span>
              </td>
              <td class="py-3 pr-4 hidden md:table-cell text-text-secondary text-xs">
                {{ job.scaleway_job_id || '—' }}
              </td>
              <td class="py-3 pr-4 hidden lg:table-cell text-text-secondary">
                {{ job.started_at ? timeAgo(job.started_at) : '—' }}
              </td>
              <td class="py-3 pr-4 hidden lg:table-cell text-text-secondary">
                {{ job.completed_at ? timeAgo(job.completed_at) : '—' }}
              </td>
              <td class="py-3 hidden sm:table-cell text-text-secondary">
                {{ timeAgo(job.created_at) }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- Pagination -->
      <div class="flex items-center justify-between mt-6 text-sm">
        <p class="text-text-secondary">
          {{ total }} job{{ total === 1 ? '' : 's' }} total
        </p>
        <div class="flex gap-2">
          <button
            class="btn-ghost px-3 py-1.5 inline-flex items-center gap-1.5"
            :disabled="page <= 1"
            @click="goToPage(page - 1)"
          >
            <ChevronLeft :size="14" />
            Prev
          </button>
          <span class="px-3 py-1.5 text-text-secondary">{{ page }} / {{ totalPages }}</span>
          <button
            class="btn-ghost px-3 py-1.5 inline-flex items-center gap-1.5"
            :disabled="page >= totalPages"
            @click="goToPage(page + 1)"
          >
            Next
            <ChevronRight :size="14" />
          </button>
        </div>
      </div>
    </div>

    <!-- Detail modal -->
    <div v-if="detail" class="fixed inset-0 z-50 flex items-center justify-center">
      <div class="absolute inset-0 bg-black/30" @click="detail = null" />
      <div class="relative bg-bg-card border border-border p-8 w-full max-w-md mx-4">
        <div class="flex items-center justify-between mb-6">
          <h3 class="text-sm font-bold uppercase tracking-wider">Job Detail</h3>
          <button class="text-text-secondary hover:text-text-primary" @click="detail = null">
            <X :size="18" />
          </button>
        </div>
        <dl class="space-y-3 text-sm">
          <div class="flex justify-between">
            <dt class="text-text-secondary">Wildcard</dt>
            <dd class="font-medium">{{ wildcardValue(detail.wildcard_id) }}</dd>
          </div>
          <div class="flex justify-between">
            <dt class="text-text-secondary">Mode</dt>
            <dd>{{ detail.mode }}</dd>
          </div>
          <div class="flex justify-between">
            <dt class="text-text-secondary">Status</dt>
            <dd>
              <span :class="statusBadgeClass(detail.status)">
                <span :class="statusDotClass(detail.status)" class="status-dot" />
                {{ detail.status }}
              </span>
            </dd>
          </div>
          <div class="flex justify-between">
            <dt class="text-text-secondary">Scaleway Job</dt>
            <dd class="text-xs">{{ detail.scaleway_job_id || '—' }}</dd>
          </div>
          <div class="border-t border-border my-2" />
          <div class="flex justify-between">
            <dt class="text-text-secondary">Created</dt>
            <dd>{{ formatDate(detail.created_at) }}</dd>
          </div>
          <div class="flex justify-between">
            <dt class="text-text-secondary">Started</dt>
            <dd>{{ detail.started_at ? formatDate(detail.started_at) : '—' }}</dd>
          </div>
          <div class="flex justify-between">
            <dt class="text-text-secondary">Completed</dt>
            <dd>{{ detail.completed_at ? formatDate(detail.completed_at) : '—' }}</dd>
          </div>
          <div v-if="detail.started_at && detail.completed_at" class="flex justify-between">
            <dt class="text-text-secondary">Duration</dt>
            <dd>{{ duration(detail.started_at, detail.completed_at) }}</dd>
          </div>
        </dl>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import type { PaginatedResponse, ReconJob, Wildcard } from '~/types/api'

const { api } = useApi()

const jobs = ref<ReconJob[]>([])
const wildcards = ref<Wildcard[]>([])
const loading = ref(true)
const total = ref(0)
const page = ref(1)
const perPage = 25
const detail = ref<ReconJob | null>(null)

const filters = reactive({
  wildcard_id: '',
  status: '',
})

const totalPages = computed(() => Math.max(1, Math.ceil(total.value / perPage)))

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

function formatDate(dateStr: string): string {
  return new Date(dateStr).toLocaleString('en-GB', {
    day: '2-digit',
    month: 'short',
    year: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}

function duration(start: string, end: string): string {
  const ms = new Date(end).getTime() - new Date(start).getTime()
  const seconds = Math.floor(ms / 1000)
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  const remainSec = seconds % 60
  if (minutes < 60) return `${minutes}m ${remainSec}s`
  const hours = Math.floor(minutes / 60)
  const remainMin = minutes % 60
  return `${hours}h ${remainMin}m`
}

function statusBadgeClass(status: string): string {
  const base = 'inline-flex items-center gap-1.5 px-2.5 py-0.5 text-xs font-medium rounded-full'
  const map: Record<string, string> = {
    pending: `${base} bg-gray-100 text-gray-700`,
    running: `${base} bg-blue-100 text-blue-700`,
    completed: `${base} bg-emerald-100 text-emerald-700`,
    failed: `${base} bg-red-100 text-red-700`,
  }
  return map[status] || base
}

function statusDotClass(status: string): string {
  const map: Record<string, string> = {
    pending: 'bg-gray-500',
    running: 'bg-blue-500',
    completed: 'bg-emerald-500',
    failed: 'bg-red-500',
  }
  return map[status] || 'bg-gray-500'
}

function resetAndFetch() {
  page.value = 1
  fetchJobs()
}

function goToPage(p: number) {
  page.value = p
  fetchJobs()
}

function openDetail(job: ReconJob) {
  detail.value = job
}

async function fetchJobs() {
  loading.value = true
  try {
    const params: Record<string, string | number> = {
      page: page.value,
      per_page: perPage,
    }
    if (filters.wildcard_id) params.wildcard_id = filters.wildcard_id
    if (filters.status) params.status = filters.status

    const res = await api<PaginatedResponse<ReconJob>>('/api/jobs', { params })
    jobs.value = res.data
    total.value = res.total
  } catch {
    jobs.value = []
    total.value = 0
  } finally {
    loading.value = false
  }
}

async function fetchWildcards() {
  try {
    wildcards.value = await api<Wildcard[]>('/api/wildcards')
  } catch {
    wildcards.value = []
  }
}

onMounted(async () => {
  await Promise.all([fetchWildcards(), fetchJobs()])
})
</script>
