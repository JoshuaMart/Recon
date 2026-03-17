const REFRESH_THRESHOLD_MS = 5 * 60 * 1000 // 5 minutes

export default defineNuxtPlugin(() => {
  const auth = useAuthStore()

  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState !== 'visible') return
    if (!auth.isAuthenticated) return

    if (auth.expiresInMs < REFRESH_THRESHOLD_MS) {
      auth.refresh()
    }
  })
})
