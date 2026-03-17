import { defineStore } from 'pinia'
import type { AuthResponse } from '~/types/api'

interface AuthState {
  token: string | null
  refreshToken: string | null
  expiresAt: string | null
}

export const useAuthStore = defineStore('auth', {
  state: (): AuthState => ({
    token: null,
    refreshToken: null,
    expiresAt: null,
  }),

  getters: {
    isAuthenticated(): boolean {
      if (!this.token || !this.expiresAt) return false
      return new Date(this.expiresAt) > new Date()
    },

    expiresInMs(): number {
      if (!this.expiresAt) return 0
      return new Date(this.expiresAt).getTime() - Date.now()
    },
  },

  actions: {
    hydrate() {
      if (import.meta.client) {
        this.token = localStorage.getItem('token')
        this.refreshToken = localStorage.getItem('refresh_token')
        this.expiresAt = localStorage.getItem('expires_at')
      }
    },

    setTokens(auth: AuthResponse) {
      this.token = auth.token
      this.refreshToken = auth.refresh_token
      this.expiresAt = auth.expires_at

      if (import.meta.client) {
        localStorage.setItem('token', auth.token)
        localStorage.setItem('refresh_token', auth.refresh_token)
        localStorage.setItem('expires_at', auth.expires_at)
      }
    },

    clearTokens() {
      this.token = null
      this.refreshToken = null
      this.expiresAt = null

      if (import.meta.client) {
        localStorage.removeItem('token')
        localStorage.removeItem('refresh_token')
        localStorage.removeItem('expires_at')
      }
    },

    async login(password: string) {
      const config = useRuntimeConfig()
      const data = await $fetch<AuthResponse>('/api/auth/login', {
        baseURL: config.public.apiUrl,
        method: 'POST',
        body: { password },
      })

      this.setTokens(data)
      await navigateTo('/')
    },

    async refresh() {
      if (!this.refreshToken) {
        this.logout()
        return
      }

      try {
        const config = useRuntimeConfig()
        const data = await $fetch<AuthResponse>('/api/auth/refresh', {
          baseURL: config.public.apiUrl,
          method: 'POST',
          body: { refresh_token: this.refreshToken },
        })

        this.setTokens(data)
      } catch {
        this.logout()
      }
    },

    async logout() {
      this.clearTokens()
      await navigateTo('/login')
    },
  },
})
