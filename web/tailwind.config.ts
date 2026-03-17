import type { Config } from 'tailwindcss'

export default {
  content: [
    './components/**/*.{vue,ts}',
    './layouts/**/*.vue',
    './pages/**/*.vue',
    './composables/**/*.ts',
    './plugins/**/*.ts',
    './app.vue',
  ],
  theme: {
    extend: {
      colors: {
        bg: '#F5F0E8',
        'bg-sidebar': '#EDE8DC',
        'bg-card': '#FAF7F2',
        border: '#D9D3C7',
        'text-primary': '#1A1714',
        'text-secondary': '#6B6560',
        accent: '#C1654A',
        'accent-muted': '#E8C4B8',
      },
      fontFamily: {
        mono: ['JetBrains Mono', 'monospace'],
      },
    },
  },
} satisfies Config
