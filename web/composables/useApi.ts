import type { FetchOptions } from 'ofetch'

export function useApi() {
  const config = useRuntimeConfig()
  const auth = useAuthStore()

  async function api<T>(url: string, opts: FetchOptions = {}): Promise<T> {
    const headers: Record<string, string> = {
      ...((opts.headers as Record<string, string>) || {}),
    }

    if (auth.token) {
      headers.Authorization = `Bearer ${auth.token}`
    }

    try {
      return await $fetch<T>(url, {
        baseURL: config.public.apiUrl,
        ...opts,
        headers,
      })
    } catch (error: any) {
      if (error?.response?.status === 401 && auth.refreshToken) {
        await auth.refresh()

        if (auth.token) {
          headers.Authorization = `Bearer ${auth.token}`
          return await $fetch<T>(url, {
            baseURL: config.public.apiUrl,
            ...opts,
            headers,
          })
        }
      }
      throw error
    }
  }

  return { api }
}
