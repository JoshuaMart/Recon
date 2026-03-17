<template>
  <div class="pt-4">
    <PageTitle title="Wildcards">
      <template #subtitle>Manage your wildcard targets.</template>
    </PageTitle>

    <!-- Add wildcard -->
    <div class="mb-8">
      <form class="flex gap-3 items-end" @submit.prevent="addWildcard">
        <div class="flex-1 max-w-md">
          <label class="block text-xs text-text-secondary mb-2 uppercase tracking-wider">
            New wildcard
          </label>
          <input
            v-model="newValue"
            type="text"
            class="w-full bg-bg border border-border px-4 py-2.5 font-mono text-sm
                   focus:outline-none focus:border-accent transition-colors
                   placeholder:text-text-secondary/40"
            placeholder="*.example.com"
            :disabled="addLoading"
          />
        </div>
        <button type="submit" class="btn-primary px-5 py-2.5" :disabled="addLoading || !newValue.trim()">
          {{ addLoading ? 'Adding...' : 'Add' }}
        </button>
      </form>
      <p v-if="addError" class="mt-2 text-sm text-accent">{{ addError }}</p>
    </div>

    <!-- Table -->
    <div v-if="loading" class="text-sm text-text-secondary">Loading...</div>
    <div v-else-if="wildcards.length === 0" class="text-sm text-text-secondary">
      No wildcards yet. Add one above.
    </div>
    <div v-else class="overflow-x-auto">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b border-border text-left text-xs text-text-secondary uppercase tracking-wider">
            <th class="pb-3 pr-4">Wildcard</th>
            <th class="pb-3 pr-4 hidden sm:table-cell">Hostnames</th>
            <th class="pb-3 pr-4 hidden md:table-cell">Alive</th>
            <th class="pb-3 pr-4 hidden md:table-cell">Dead</th>
            <th class="pb-3 pr-4 hidden md:table-cell">Unreachable</th>
            <th class="pb-3 pr-4 hidden lg:table-cell">Web</th>
            <th class="pb-3 pr-4 hidden lg:table-cell">Last Recon</th>
            <th class="pb-3 pr-4 hidden xl:table-cell">Last Revalidated</th>
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
            <td class="py-3 pr-4 hidden md:table-cell">
              <span class="text-amber-600">{{ w.stats.hostnames_unreachable }}</span>
            </td>
            <td class="py-3 pr-4 hidden lg:table-cell">{{ w.stats.web_services }}</td>
            <td class="py-3 pr-4 hidden lg:table-cell text-text-secondary">
              {{ w.last_recon_at ? timeAgo(w.last_recon_at) : '—' }}
            </td>
            <td class="py-3 pr-4 hidden xl:table-cell text-text-secondary">
              {{ w.last_revalidated_at ? timeAgo(w.last_revalidated_at) : '—' }}
            </td>
            <td class="py-3">
              <div class="flex gap-1.5">
                <button
                  class="btn-ghost text-xs px-2 py-1 inline-flex items-center gap-1.5"
                  :disabled="reconModal.visible"
                  title="Launch recon job"
                  @click="openReconModal(w)"
                >
                  <Play :size="13" />
                  <span class="hidden sm:inline">Recon</span>
                </button>
                <button
                  class="btn-ghost text-xs px-2 py-1 inline-flex items-center gap-1.5"
                  :disabled="revalidateLoading === w.id"
                  title="Re-check all hostnames status"
                  @click="revalidate(w.id)"
                >
                  <Loader2 v-if="revalidateLoading === w.id" :size="13" class="animate-spin" />
                  <RefreshCw v-else :size="13" />
                  <span class="hidden sm:inline">Revalidate</span>
                </button>
                <button
                  class="btn-ghost text-xs px-2 py-1 inline-flex items-center gap-1.5 hover:!text-red-600 hover:!border-red-200"
                  title="Delete wildcard and all related data"
                  @click="openDeleteModal(w)"
                >
                  <Trash2 :size="13" />
                  <span class="hidden sm:inline">Delete</span>
                </button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- Recon modal -->
    <div v-if="reconModal.visible" class="fixed inset-0 z-50 flex items-center justify-center">
      <div class="absolute inset-0 bg-black/30" @click="reconModal.visible = false" />
      <div class="relative bg-bg-card border border-border p-8 w-full max-w-sm mx-4">
        <h3 class="text-sm font-bold uppercase tracking-wider mb-6">Launch Recon</h3>
        <p class="text-sm text-text-secondary mb-4">
          Target: <span class="text-text-primary font-medium">{{ reconModal.wildcard?.value }}</span>
        </p>
        <div class="mb-6">
          <label class="block text-xs text-text-secondary mb-2 uppercase tracking-wider">Mode</label>
          <div class="flex gap-3">
            <button
              v-for="m in ['normal', 'intensive'] as const"
              :key="m"
              class="flex-1 py-2 text-sm font-medium border transition-colors"
              :class="reconModal.mode === m
                ? 'border-accent bg-accent/10 text-accent'
                : 'border-border text-text-secondary hover:border-border hover:bg-bg'"
              @click="reconModal.mode = m"
            >
              {{ m }}
            </button>
          </div>
        </div>
        <div class="flex gap-3">
          <button class="btn-ghost flex-1" @click="reconModal.visible = false">Cancel</button>
          <button
            class="btn-primary flex-1"
            :disabled="reconModal.loading"
            @click="launchRecon"
          >
            {{ reconModal.loading ? 'Launching...' : 'Launch' }}
          </button>
        </div>
        <p v-if="reconModal.error" class="mt-3 text-sm text-accent">{{ reconModal.error }}</p>
      </div>
    </div>

    <!-- Delete confirm modal -->
    <div v-if="deleteModal.visible" class="fixed inset-0 z-50 flex items-center justify-center">
      <div class="absolute inset-0 bg-black/30" @click="deleteModal.visible = false" />
      <div class="relative bg-bg-card border border-border p-8 w-full max-w-sm mx-4">
        <h3 class="text-sm font-bold uppercase tracking-wider mb-4">Delete Wildcard</h3>
        <p class="text-sm text-text-secondary mb-2">
          This will permanently delete
          <span class="text-text-primary font-medium">{{ deleteModal.wildcard?.value }}</span>
          and all related data:
        </p>
        <ul class="text-sm text-text-secondary mb-6 list-disc list-inside">
          <li>All hostnames</li>
          <li>All web results & fingerprints</li>
          <li>All recon jobs</li>
        </ul>
        <div class="flex gap-3">
          <button class="btn-ghost flex-1" @click="deleteModal.visible = false">Cancel</button>
          <button
            class="btn-primary flex-1 !bg-red-600 hover:!brightness-110"
            :disabled="deleteModal.loading"
            @click="confirmDelete"
          >
            {{ deleteModal.loading ? 'Deleting...' : 'Delete' }}
          </button>
        </div>
        <p v-if="deleteModal.error" class="mt-3 text-sm text-accent">{{ deleteModal.error }}</p>
      </div>
    </div>

    <!-- Toast -->
    <div
      v-if="toast"
      class="fixed bottom-6 right-6 text-sm px-4 py-3 z-50"
      :class="toast.type === 'error'
        ? 'bg-bg-card border border-accent text-accent'
        : 'bg-bg-card border border-emerald-400 text-emerald-700'"
    >
      {{ toast.message }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { Loader2, Play, RefreshCw, Trash2 } from 'lucide-vue-next'
import type { Wildcard } from '~/types/api'

const { api } = useApi()

const wildcards = ref<Wildcard[]>([])
const loading = ref(true)
const newValue = ref('')
const addLoading = ref(false)
const addError = ref('')
const revalidateLoading = ref<string | null>(null)
const toast = ref<{ message: string; type: 'success' | 'error' } | null>(null)

const reconModal = reactive({
  visible: false,
  wildcard: null as Wildcard | null,
  mode: 'normal' as 'normal' | 'intensive',
  loading: false,
  error: '',
})

const deleteModal = reactive({
  visible: false,
  wildcard: null as Wildcard | null,
  loading: false,
  error: '',
})

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

function showToast(message: string, type: 'success' | 'error' = 'success') {
  toast.value = { message, type }
  setTimeout(() => {
    toast.value = null
  }, 4000)
}

async function fetchWildcards() {
  try {
    wildcards.value = await api<Wildcard[]>('/api/wildcards')
  } catch {
    wildcards.value = []
  } finally {
    loading.value = false
  }
}

async function addWildcard() {
  if (!newValue.value.trim() || addLoading.value) return
  addLoading.value = true
  addError.value = ''

  try {
    await api('/api/wildcards', {
      method: 'POST',
      body: { value: newValue.value.trim() },
    })
    newValue.value = ''
    await fetchWildcards()
    showToast('Wildcard added')
  } catch (e: any) {
    const msg = e?.data?.error || e?.message || 'Failed to add wildcard'
    if (e?.response?.status === 409) {
      addError.value = 'This wildcard already exists'
    } else if (e?.response?.status === 400) {
      addError.value = 'Invalid format — use *.domain.tld'
    } else {
      addError.value = msg
    }
  } finally {
    addLoading.value = false
  }
}

function openReconModal(w: Wildcard) {
  reconModal.wildcard = w
  reconModal.mode = 'normal'
  reconModal.error = ''
  reconModal.loading = false
  reconModal.visible = true
}

async function launchRecon() {
  if (!reconModal.wildcard || reconModal.loading) return
  reconModal.loading = true
  reconModal.error = ''

  try {
    await api(`/api/wildcards/${reconModal.wildcard.id}/recon`, {
      method: 'POST',
      body: { mode: reconModal.mode },
    })
    reconModal.visible = false
    showToast('Recon job launched')
    await fetchWildcards()
  } catch (e: any) {
    if (e?.response?.status === 409) {
      reconModal.error = e?.data?.error || 'A job is already running or max concurrent jobs reached'
    } else {
      reconModal.error = e?.data?.error || 'Failed to launch recon'
    }
  } finally {
    reconModal.loading = false
  }
}

function openDeleteModal(w: Wildcard) {
  deleteModal.wildcard = w
  deleteModal.error = ''
  deleteModal.loading = false
  deleteModal.visible = true
}

async function confirmDelete() {
  if (!deleteModal.wildcard || deleteModal.loading) return
  deleteModal.loading = true
  deleteModal.error = ''

  try {
    await api(`/api/wildcards/${deleteModal.wildcard.id}`, { method: 'DELETE' })
    deleteModal.visible = false
    showToast('Wildcard deleted')
    await fetchWildcards()
  } catch (e: any) {
    deleteModal.error = e?.data?.error || 'Failed to delete wildcard'
  } finally {
    deleteModal.loading = false
  }
}

async function revalidate(wildcardId: string) {
  revalidateLoading.value = wildcardId
  try {
    await api(`/api/wildcards/${wildcardId}/revalidate`, { method: 'PUT' })
    showToast('Revalidation started')
    await fetchWildcards()
  } catch (e: any) {
    showToast(e?.data?.error || 'Failed to revalidate', 'error')
  } finally {
    revalidateLoading.value = null
  }
}

onMounted(fetchWildcards)
</script>
