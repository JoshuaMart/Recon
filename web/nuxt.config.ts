export default defineNuxtConfig({
  ssr: false,

  modules: ['@nuxtjs/tailwindcss', '@pinia/nuxt', '@nuxtjs/google-fonts'],

  css: ['~/assets/css/main.css'],

  runtimeConfig: {
    public: {
      apiUrl: process.env.NUXT_PUBLIC_API_URL || 'http://localhost:3002',
    },
  },

  googleFonts: {
    families: {
      'JetBrains Mono': [400, 500, 600, 700],
    },
    display: 'swap',
  },

  compatibilityDate: '2025-01-01',
})
