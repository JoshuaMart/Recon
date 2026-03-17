export default defineNuxtRouteMiddleware(async (to) => {
  const auth = useAuthStore()
  auth.hydrate()

  const isLoginPage = to.path === '/login'

  if (!auth.isAuthenticated && !isLoginPage) {
    if (auth.refreshToken) {
      await auth.refresh()
      if (auth.isAuthenticated) return
    }
    return navigateTo('/login')
  }

  if (auth.isAuthenticated && isLoginPage) {
    return navigateTo('/')
  }
})
