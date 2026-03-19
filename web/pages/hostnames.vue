<template>
  <div class="pt-4">
    <PageTitle title="Hostnames">
      <template #subtitle>Browse discovered hostnames across all wildcards.</template>
    </PageTitle>

    <!-- Filters -->
    <div class="flex flex-wrap gap-3 mb-8">
      <select
        v-model="filters.wildcard_id"
        class="bg-bg border border-border px-3 py-2 text-sm font-mono focus:outline-none focus:border-accent transition-colors"
        @change="resetAndFetch"
      >
        <option value="">All wildcards</option>
        <option v-for="w in wildcards" :key="w.id" :value="w.id">{{ w.value }}</option>
      </select>
      <select
        v-model="filters.status"
        class="bg-bg border border-border px-3 py-2 text-sm font-mono focus:outline-none focus:border-accent transition-colors"
        @change="resetAndFetch"
      >
        <option value="">All statuses</option>
        <option value="alive">Alive</option>
        <option value="dead">Dead</option>
        <option value="unreachable">Unreachable</option>
      </select>
      <select
        v-model="filters.type"
        class="bg-bg border border-border px-3 py-2 text-sm font-mono focus:outline-none focus:border-accent transition-colors"
        @change="resetAndFetch"
      >
        <option value="">All types</option>
        <option value="web">Web</option>
        <option value="other">Other</option>
        <option value="unknown">Unknown</option>
      </select>
    </div>

    <!-- Table -->
    <div v-if="loading" class="text-sm text-text-secondary">Loading...</div>
    <div v-else-if="hostnames.length === 0" class="text-sm text-text-secondary">No hostnames found.</div>
    <div v-else>
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-border text-left text-xs text-text-secondary uppercase tracking-wider">
              <th class="pb-3 pr-4">FQDN</th>
              <th class="pb-3 pr-4 hidden sm:table-cell">IP</th>
              <th class="pb-3 pr-4 hidden md:table-cell">CDN</th>
              <th class="pb-3 pr-4">Status</th>
              <th class="pb-3 pr-4 hidden md:table-cell">Ports</th>
              <th class="pb-3 pr-4 hidden md:table-cell">Type</th>
              <th class="pb-3 hidden lg:table-cell">Last Seen</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="h in hostnames"
              :key="h.id"
              class="table-row cursor-pointer"
              @click="openDetail(h.id)"
            >
              <td class="py-3 pr-4 font-medium">{{ h.fqdn }}</td>
              <td class="py-3 pr-4 hidden sm:table-cell text-text-secondary">{{ h.ip || '—' }}</td>
              <td class="py-3 pr-4 hidden md:table-cell text-text-secondary">{{ h.cdn || '—' }}</td>
              <td class="py-3 pr-4">
                <span :class="`badge-${h.status}`">
                  <span :class="`status-dot ${h.status}`" />
                  {{ h.status }}
                </span>
              </td>
              <td class="py-3 pr-4 hidden md:table-cell">
                <div v-if="h.ports && h.ports.tcp && Object.keys(h.ports.tcp).length" class="flex flex-wrap gap-1">
                  <span
                    v-for="port in Object.keys(h.ports.tcp)"
                    :key="port"
                    class="text-xs px-1.5 py-0.5 bg-bg border border-border font-mono"
                  >
                    {{ port }}
                  </span>
                </div>
                <span v-else class="text-text-secondary">—</span>
              </td>
              <td class="py-3 pr-4 hidden md:table-cell">
                <span class="text-xs text-text-secondary px-2 py-0.5 border border-border rounded-full">
                  {{ h.type }}
                </span>
              </td>
              <td class="py-3 hidden lg:table-cell text-text-secondary">
                {{ timeAgo(h.last_seen_at) }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- Pagination -->
      <div class="flex items-center justify-between mt-6 text-sm">
        <p class="text-text-secondary">
          {{ total }} hostname{{ total === 1 ? '' : 's' }} total
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
      <div class="relative bg-bg-card border border-border p-8 w-full max-w-lg mx-4 max-h-[85vh] overflow-y-auto">
        <div class="flex items-center justify-between mb-6">
          <h3 class="text-sm font-bold uppercase tracking-wider">Hostname Detail</h3>
          <button class="text-text-secondary hover:text-text-primary" @click="detail = null">
            <X :size="18" />
          </button>
        </div>

        <div v-if="detailLoading" class="text-sm text-text-secondary">Loading...</div>
        <div v-else-if="detail">
          <!-- General info -->
          <dl class="space-y-3 text-sm mb-6">
            <div class="flex justify-between">
              <dt class="text-text-secondary">FQDN</dt>
              <dd class="font-medium">{{ detail.fqdn }}</dd>
            </div>
            <div class="flex justify-between">
              <dt class="text-text-secondary">IP</dt>
              <dd>{{ detail.ip || '—' }}</dd>
            </div>
            <div class="flex justify-between">
              <dt class="text-text-secondary">CDN</dt>
              <dd>{{ detail.cdn || '—' }}</dd>
            </div>
            <div class="flex justify-between">
              <dt class="text-text-secondary">Status</dt>
              <dd>
                <span :class="`badge-${detail.status}`">
                  <span :class="`status-dot ${detail.status}`" />
                  {{ detail.status }}
                </span>
              </dd>
            </div>
            <div class="flex justify-between">
              <dt class="text-text-secondary">Type</dt>
              <dd>{{ detail.type }}</dd>
            </div>
            <div class="flex justify-between">
              <dt class="text-text-secondary">Last Seen</dt>
              <dd>{{ formatDate(detail.last_seen_at) }}</dd>
            </div>
          </dl>

          <!-- DNS -->
          <div v-if="detail.dns && hasDnsRecords(detail.dns)" class="mb-6">
            <h4 class="section-title">DNS Records</h4>
            <div class="space-y-2 text-sm">
              <div v-if="detail.dns.A?.length" class="flex gap-3">
                <span class="text-text-secondary w-16 shrink-0">A</span>
                <div class="flex flex-wrap gap-1.5">
                  <span
                    v-for="record in detail.dns.A"
                    :key="record"
                    class="px-2 py-0.5 bg-bg border border-border text-xs"
                  >
                    {{ record }}
                  </span>
                </div>
              </div>
              <div v-if="detail.dns.AAAA?.length" class="flex gap-3">
                <span class="text-text-secondary w-16 shrink-0">AAAA</span>
                <div class="flex flex-wrap gap-1.5">
                  <span
                    v-for="record in detail.dns.AAAA"
                    :key="record"
                    class="px-2 py-0.5 bg-bg border border-border text-xs"
                  >
                    {{ record }}
                  </span>
                </div>
              </div>
              <div v-if="detail.dns.CNAME?.length" class="flex gap-3">
                <span class="text-text-secondary w-16 shrink-0">CNAME</span>
                <div class="flex flex-wrap gap-1.5">
                  <span
                    v-for="record in detail.dns.CNAME"
                    :key="record"
                    class="px-2 py-0.5 bg-bg border border-border text-xs"
                  >
                    {{ record }}
                  </span>
                </div>
              </div>
            </div>
          </div>

          <!-- Ports -->
          <div v-if="detail.ports && hasPortEntries(detail.ports)" class="mb-6">
            <h4 class="section-title">Ports</h4>
            <div class="space-y-2 text-sm">
              <div
                v-for="(info, port) in detail.ports.tcp"
                :key="port"
                class="flex items-center justify-between py-1.5 border-b border-border last:border-0"
              >
                <div class="flex items-center gap-3">
                  <span class="font-medium">{{ port }}/tcp</span>
                  <span v-if="info.web" class="badge-alive">web</span>
                </div>
                <a
                  v-if="info.web"
                  :href="info.web"
                  target="_blank"
                  rel="noopener"
                  class="text-accent hover:underline text-xs flex items-center gap-1"
                >
                  <ExternalLink :size="12" />
                  {{ info.web }}
                </a>
              </div>
            </div>
          </div>

          <!-- Timestamps -->
          <div class="border-t border-border pt-4">
            <dl class="space-y-2 text-sm text-text-secondary">
              <div class="flex justify-between">
                <dt>Created</dt>
                <dd>{{ formatDate(detail.created_at) }}</dd>
              </div>
              <div class="flex justify-between">
                <dt>Updated</dt>
                <dd>{{ formatDate(detail.updated_at) }}</dd>
              </div>
            </dl>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ChevronLeft, ChevronRight, ExternalLink, X } from 'lucide-vue-next'
import type { Hostname, HostnameDNS, HostnamePorts, PaginatedResponse, Wildcard } from '~/types/api'

const { api } = useApi()

const hostnames = ref<Hostname[]>([])
const wildcards = ref<Wildcard[]>([])
const loading = ref(true)
const total = ref(0)
const page = ref(1)
const perPage = 50
const detail = ref<Hostname | null>(null)
const detailLoading = ref(false)

const filters = reactive({
  wildcard_id: '',
  status: '',
  type: '',
})

const totalPages = computed(() => Math.max(1, Math.ceil(total.value / perPage)))

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

function hasDnsRecords(dns: HostnameDNS): boolean {
  return Boolean(dns.A?.length || dns.AAAA?.length || dns.CNAME?.length)
}

function hasPortEntries(ports: HostnamePorts): boolean {
  return Object.keys(ports.tcp).length > 0
}

function resetAndFetch() {
  page.value = 1
  fetchHostnames()
}

function goToPage(p: number) {
  page.value = p
  fetchHostnames()
}

async function openDetail(id: string) {
  detailLoading.value = true
  detail.value = null
  // Set a placeholder so the modal opens
  detail.value = hostnames.value.find((h) => h.id === id) || null

  try {
    const full = await api<Hostname>(`/api/hostnames/${id}`)
    detail.value = full
  } catch {
    // keep the list version if detail fetch fails
  } finally {
    detailLoading.value = false
  }
}

async function fetchHostnames() {
  loading.value = true
  try {
    const params: Record<string, string | number> = {
      page: page.value,
      per_page: perPage,
    }
    if (filters.wildcard_id) params.wildcard_id = filters.wildcard_id
    if (filters.status) params.status = filters.status
    if (filters.type) params.type = filters.type

    const res = await api<PaginatedResponse<Hostname>>('/api/hostnames', { params })
    hostnames.value = res.data
    total.value = res.total
  } catch {
    hostnames.value = []
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
  await Promise.all([fetchWildcards(), fetchHostnames()])
})
</script>
