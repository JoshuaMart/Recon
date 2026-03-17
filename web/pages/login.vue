<template>
  <div>
    <div class="bg-bg-card border border-border p-6 sm:p-10 w-full max-w-[420px]">
      <!-- Header -->
      <div class="mb-10 text-center">
        <h1 class="text-2xl font-bold tracking-wider">BUG_LOG</h1>
        <p class="text-xs text-text-secondary mt-2 tracking-widest uppercase">recon platform</p>
      </div>

      <!-- Form -->
      <form @submit.prevent="handleLogin">
        <!-- Password field -->
        <div class="mb-6">
          <label class="block text-xs text-text-secondary mb-2 uppercase tracking-wider">
            Password
          </label>
          <div class="relative">
            <input
              ref="passwordInput"
              v-model="password"
              :type="showPassword ? 'text' : 'password'"
              class="w-full bg-bg border border-border px-4 py-3 font-mono text-sm
                     focus:outline-none focus:border-accent transition-colors
                     placeholder:text-text-secondary/40"
              placeholder="Enter access key"
              :disabled="loading"
              autocomplete="current-password"
            />
            <button
              type="button"
              class="absolute right-3 top-1/2 -translate-y-1/2 text-text-secondary hover:text-text-primary transition-colors"
              @click="showPassword = !showPassword"
              tabindex="-1"
            >
              <span class="text-sm">{{ showPassword ? '◉' : '○' }}</span>
            </button>
          </div>
        </div>

        <!-- Submit -->
        <button
          type="submit"
          class="btn-primary w-full"
          :disabled="loading || !password"
        >
          {{ loading ? 'CHECKING...' : 'ACCESS' }}
        </button>

        <!-- Error -->
        <p v-if="error" class="mt-4 text-sm text-accent text-center">
          {{ error }}
        </p>
      </form>
    </div>
  </div>
</template>

<script setup lang="ts">
definePageMeta({
  layout: 'auth',
})

const auth = useAuthStore()

const password = ref('')
const showPassword = ref(false)
const loading = ref(false)
const error = ref('')
const passwordInput = ref<HTMLInputElement>()

onMounted(() => {
  passwordInput.value?.focus()
})

async function handleLogin() {
  if (!password.value || loading.value) return

  loading.value = true
  error.value = ''

  try {
    await auth.login(password.value)
  } catch (e: any) {
    if (e?.name === 'FetchError' && !e?.response) {
      error.value = 'Connection error — is the API running?'
    } else {
      error.value = e?.data?.error || e?.message || 'Authentication failed'
    }
  } finally {
    loading.value = false
  }
}
</script>
