<template>
  <div class="pt-4">
    <PageTitle title="Web Services">
      <template #subtitle>Discovered URLs and fingerprint data.</template>
    </PageTitle>

    <!-- Filters + manual fingerprint -->
    <div class="flex flex-wrap gap-3 mb-8 items-end">
      <select
        v-model="filters.wildcard_id"
        class="bg-bg border border-border px-3 py-2 text-sm font-mono focus:outline-none focus:border-accent transition-colors"
        @change="resetAndFetch"
      >
        <option value="">All wildcards</option>
        <option v-for="w in wildcards" :key="w.id" :value="w.id">{{ w.value }}</option>
      </select>

      <div class="ml-auto flex gap-2 items-end">
        <input
          v-model="fingerprintUrl"
          type="text"
          class="bg-bg border border-border px-3 py-2 text-sm font-mono w-64
                 focus:outline-none focus:border-accent transition-colors
                 placeholder:text-text-secondary/40"
          placeholder="https://example.com"
          :disabled="fingerprintLoading"
          @keydown.enter="submitFingerprint"
        />
        <button
          class="btn-primary px-4 py-2 text-sm inline-flex items-center gap-1.5"
          :disabled="fingerprintLoading || !fingerprintUrl.trim()"
          @click="submitFingerprint"
        >
          <Loader2 v-if="fingerprintLoading" :size="13" class="animate-spin" />
          <Scan v-else :size="13" />
          Fingerprint
        </button>
      </div>
    </div>
    <p
      v-if="fingerprintMsg"
      class="mb-6 text-sm"
      :class="fingerprintError ? 'text-accent' : 'text-emerald-700'"
    >
      {{ fingerprintMsg }}
    </p>

    <!-- Table -->
    <div v-if="loading" class="text-sm text-text-secondary">Loading...</div>
    <div v-else-if="urls.length === 0" class="text-sm text-text-secondary">No web services found.</div>
    <div v-else>
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-border text-left text-xs text-text-secondary uppercase tracking-wider">
              <th class="pb-3 pr-4">URL</th>
              <th class="pb-3 pr-4">Chain</th>
              <th class="pb-3 pr-4 hidden md:table-cell">Sizes</th>
              <th class="pb-3 pr-4 hidden md:table-cell">Title</th>
              <th class="pb-3 pr-4 hidden lg:table-cell">Tech</th>
              <th class="pb-3 pr-4 hidden sm:table-cell">Ext.</th>
              <th class="pb-3 hidden sm:table-cell">Scanned</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="u in urls"
              :key="u.id"
              class="table-row cursor-pointer"
              @click="openDetail(u.id)"
            >
              <td class="py-3 pr-4 font-medium max-w-[280px] truncate">{{ u.url }}</td>
              <td class="py-3 pr-4">
                <div class="flex items-center gap-0.5 flex-wrap">
                  <template v-for="(entry, i) in (u.chain || [])" :key="i">
                    <ArrowRight v-if="i > 0" :size="10" class="text-text-secondary/50 mx-0.5 shrink-0" />
                    <span :class="statusCodeClass(String(entry.status_code))" class="text-xs">
                      {{ entry.status_code }}
                    </span>
                  </template>
                  <span v-if="!u.chain?.length" class="text-text-secondary">—</span>
                </div>
              </td>
              <td class="py-3 pr-4 hidden md:table-cell">
                <div class="flex items-center gap-0.5 text-xs text-text-secondary flex-wrap">
                  <template v-for="(entry, i) in (u.chain || [])" :key="i">
                    <ArrowRight v-if="i > 0" :size="10" class="opacity-30 mx-0.5 shrink-0" />
                    <span>{{ formatSize(entry.response_size) }}</span>
                  </template>
                  <span v-if="!u.chain?.length">—</span>
                </div>
              </td>
              <td class="py-3 pr-4 hidden md:table-cell text-text-secondary max-w-[180px] truncate">
                {{ u.title || '—' }}
              </td>
              <td class="py-3 pr-4 hidden md:table-cell">
                <div v-if="u.technologies?.length" class="flex items-center gap-1.5">
                  <template v-for="tech in u.technologies.slice(0, 5)" :key="tech.name">
                    <span
                      class="inline-flex items-center gap-1"
                      :title="`${tech.name}${tech.version ? ' ' + tech.version : ''}`"
                    >
                      <img
                        :src="techIconUrl(tech.name)"
                        :alt="tech.name"
                        class="w-4 h-4 object-contain"
                        @error="onIconError"
                      />
                    </span>
                  </template>
                  <span
                    v-if="u.technologies.length > 5"
                    class="text-xs text-text-secondary"
                  >
                    +{{ u.technologies.length - 5 }}
                  </span>
                </div>
                <span v-else class="text-text-secondary">—</span>
              </td>
              <td class="py-3 pr-4 hidden sm:table-cell text-text-secondary">
                {{ u.external_hosts?.length || 0 }}
              </td>
              <td class="py-3 hidden sm:table-cell text-text-secondary">
                {{ timeAgo(u.scanned_at) }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- Pagination -->
      <div class="flex items-center justify-between mt-6 text-sm">
        <p class="text-text-secondary">
          {{ total }} URL{{ total === 1 ? '' : 's' }} total
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
      <div class="relative bg-bg-card border border-border p-8 w-full max-w-2xl mx-4 max-h-[85vh] overflow-y-auto">
        <div class="flex items-center justify-between mb-6">
          <h3 class="text-sm font-bold uppercase tracking-wider">Web Service Detail</h3>
          <button class="text-text-secondary hover:text-text-primary" @click="detail = null">
            <X :size="18" />
          </button>
        </div>

        <div v-if="detailLoading" class="text-sm text-text-secondary">Loading...</div>
        <div v-else-if="detail">
          <!-- URL -->
          <div class="mb-6">
            <dt class="text-text-secondary text-xs uppercase tracking-wider mb-1">URL</dt>
            <dd class="font-medium break-all">
              <a
                :href="detail.url"
                target="_blank"
                rel="noopener"
                class="text-accent hover:underline inline-flex items-center gap-1"
              >
                {{ detail.url }}
                <ExternalLink :size="12" />
              </a>
            </dd>
          </div>

          <!-- Redirect chain -->
          <div v-if="detail.chain?.length" class="mb-6">
            <h4 class="section-title">Redirect Chain</h4>
            <div class="space-y-2">
              <div
                v-for="(entry, i) in detail.chain"
                :key="i"
                class="bg-bg border border-border p-3"
              >
                <div class="flex items-center gap-3 mb-1.5">
                  <span class="text-xs text-text-secondary">{{ i + 1 }}.</span>
                  <span :class="statusCodeClass(String(entry.status_code))" class="text-sm">
                    {{ entry.status_code }}
                  </span>
                  <span class="text-sm break-all flex-1">{{ entry.url }}</span>
                  <span class="text-xs text-text-secondary shrink-0">
                    {{ formatSize(entry.response_size) }}
                  </span>
                </div>
                <p v-if="entry.title" class="text-xs text-text-secondary ml-7">
                  {{ entry.title }}
                </p>
              </div>
            </div>
          </div>

          <!-- Technologies -->
          <div v-if="detail.technologies?.length" class="mb-6">
            <h4 class="section-title">Technologies</h4>
            <div class="space-y-1.5">
              <div
                v-for="tech in detail.technologies"
                :key="tech.name"
                class="flex items-center gap-3 py-1.5"
              >
                <span class="text-sm font-medium">{{ tech.name }}</span>
                <span v-if="tech.version" class="text-xs text-text-secondary">{{ tech.version }}</span>
                <span class="text-xs text-text-secondary ml-auto">{{ tech.category }}</span>
              </div>
            </div>
          </div>

          <!-- External hosts -->
          <div v-if="detail.external_hosts?.length" class="mb-6">
            <h4 class="section-title">External Hosts ({{ detail.external_hosts.length }})</h4>
            <div class="flex flex-wrap gap-1.5">
              <span
                v-for="host in detail.external_hosts"
                :key="host"
                class="px-2.5 py-1 bg-bg border border-border text-xs"
              >
                {{ host }}
              </span>
            </div>
          </div>

          <!-- Cookies -->
          <div v-if="detail.cookies?.length" class="mb-6">
            <h4 class="section-title">Cookies</h4>
            <div class="space-y-1">
              <div
                v-for="(cookie, i) in detail.cookies"
                :key="i"
                class="px-3 py-2 bg-bg border border-border text-xs font-mono break-all"
              >
                {{ cookie }}
              </div>
            </div>
          </div>

          <!-- Metadata -->
          <div v-if="detail.metadata && Object.keys(detail.metadata).length" class="mb-6">
            <h4 class="section-title">Metadata</h4>
            <dl class="space-y-2 text-sm">
              <div
                v-for="(val, key) in detail.metadata"
                :key="String(key)"
                class="flex gap-3"
              >
                <dt class="text-text-secondary shrink-0 min-w-[120px]">{{ key }}</dt>
                <dd class="break-all font-medium">{{ val }}</dd>
              </div>
            </dl>
          </div>

          <!-- Timestamps -->
          <div class="border-t border-border pt-4">
            <dl class="space-y-2 text-sm text-text-secondary">
              <div class="flex justify-between">
                <dt>Scanned</dt>
                <dd>{{ formatDate(detail.scanned_at) }}</dd>
              </div>
              <div v-if="detail.created_at" class="flex justify-between">
                <dt>Created</dt>
                <dd>{{ formatDate(detail.created_at) }}</dd>
              </div>
            </dl>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import type { PaginatedResponse, WebResult, Wildcard } from '~/types/api'

const ICON_BASE = '/api/_icons/'

const { api } = useApi()

const urls = ref<WebResult[]>([])
const wildcards = ref<Wildcard[]>([])
const loading = ref(true)
const total = ref(0)
const page = ref(1)
const perPage = 50
const detail = ref<WebResult | null>(null)
const detailLoading = ref(false)

const fingerprintUrl = ref('')
const fingerprintLoading = ref(false)
const fingerprintMsg = ref('')
const fingerprintError = ref(false)

const filters = reactive({
  wildcard_id: '',
})

const totalPages = computed(() => Math.max(1, Math.ceil(total.value / perPage)))

function techIconUrl(name: string): string {
  return `${ICON_BASE}${encodeURIComponent(name)}.svg`
}

function onIconError(e: Event) {
  const img = e.target as HTMLImageElement
  img.style.display = 'none'
}

function formatSize(bytes: number): string {
  if (bytes === 0) return '0B'
  if (bytes < 1024) return `${bytes}B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)}KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)}MB`
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

function statusCodeClass(code: string): string {
  const n = Number(code)
  if (n >= 200 && n < 300) return 'text-emerald-600 font-medium'
  if (n >= 300 && n < 400) return 'text-blue-600 font-medium'
  if (n >= 400 && n < 500) return 'text-amber-600 font-medium'
  if (n >= 500) return 'text-red-600 font-medium'
  return 'text-text-secondary font-medium'
}

function resetAndFetch() {
  page.value = 1
  fetchUrls()
}

function goToPage(p: number) {
  page.value = p
  fetchUrls()
}

async function openDetail(id: string) {
  detailLoading.value = true
  detail.value = urls.value.find((u) => u.id === id) || null

  try {
    const full = await api<WebResult>(`/api/urls/${id}`)
    detail.value = full
  } catch {
    // keep list version
  } finally {
    detailLoading.value = false
  }
}

async function submitFingerprint() {
  const url = fingerprintUrl.value.trim()
  if (!url || fingerprintLoading.value) return

  fingerprintLoading.value = true
  fingerprintMsg.value = ''
  fingerprintError.value = false

  try {
    await api('/api/urls/fingerprint', {
      method: 'POST',
      body: { url },
    })
    fingerprintMsg.value = 'URL enqueued for fingerprinting'
    fingerprintUrl.value = ''
  } catch (e: any) {
    fingerprintError.value = true
    if (e?.response?.status === 404) {
      fingerprintMsg.value = 'Hostname not found — URL must belong to a tracked wildcard'
    } else if (e?.response?.status === 400) {
      fingerprintMsg.value = 'Invalid URL — must start with http:// or https://'
    } else {
      fingerprintMsg.value = e?.data?.error || 'Failed to enqueue fingerprint'
    }
  } finally {
    fingerprintLoading.value = false
    setTimeout(() => {
      fingerprintMsg.value = ''
    }, 5000)
  }
}

async function fetchUrls() {
  loading.value = true
  try {
    const params: Record<string, string | number> = {
      page: page.value,
      per_page: perPage,
    }
    if (filters.wildcard_id) params.wildcard_id = filters.wildcard_id

    const res = await api<PaginatedResponse<WebResult>>('/api/urls', { params })
    urls.value = res.data
    total.value = res.total
  } catch {
    urls.value = []
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
  await Promise.all([fetchWildcards(), fetchUrls()])
})
</script>
