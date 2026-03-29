import { existsSync, readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { join } from 'node:path'
import { createError, defineEventHandler, send, setResponseHeader } from 'h3'

const require = createRequire(import.meta.url)

// Cache resolved SVGs in memory
const cache = new Map<string, string>()

// Build a title→icon lookup from simple-icons at startup
let simpleIconsByTitle: Map<string, { svg: string; hex: string }> | null = null

async function getSimpleIcons() {
  if (simpleIconsByTitle) return simpleIconsByTitle
  simpleIconsByTitle = new Map()
  try {
    const icons = await import('simple-icons')
    for (const key of Object.keys(icons)) {
      const icon = icons[key as keyof typeof icons] as any
      if (icon?.title && icon?.svg) {
        simpleIconsByTitle.set(icon.title.toLowerCase(), { svg: icon.svg, hex: icon.hex })
      }
    }
  } catch {
    // simple-icons not available
  }
  return simpleIconsByTitle
}

async function findInSimpleIcons(name: string): Promise<string | null> {
  const icons = await getSimpleIcons()
  const entry = icons.get(name.toLowerCase())
  if (entry) return entry.svg
  return null
}

function findInDevicon(name: string): string | null {
  const slug = name.toLowerCase().replace(/[^a-z0-9]/g, '')

  const variants = [
    `${slug}-original.svg`,
    `${slug}-plain.svg`,
    `${slug}-line.svg`,
    `${slug}-original-wordmark.svg`,
  ]

  for (const variant of variants) {
    try {
      const iconPath = require.resolve(`devicon/icons/${slug}/${variant}`)
      return readFileSync(iconPath, 'utf-8')
    } catch {
      // not found, try next
    }
  }
  return null
}

function findCustomIcon(name: string): string | null {
  const basePath = join(process.cwd(), 'public', 'icons', 'tech')
  const filePath = join(basePath, `${name}.svg`)
  if (existsSync(filePath)) {
    return readFileSync(filePath, 'utf-8')
  }
  return null
}

export default defineEventHandler(async (event) => {
  const raw = event.context.params?.name
  if (!raw) {
    throw createError({ statusCode: 400, statusMessage: 'Missing icon name' })
  }
  // Strip .svg extension if present
  const name = raw.replace(/\.svg$/i, '')

  // Check cache
  const cached = cache.get(name)
  if (cached) {
    setResponseHeader(event, 'Content-Type', 'image/svg+xml')
    setResponseHeader(event, 'Cache-Control', 'public, max-age=86400')
    return send(event, cached)
  }

  // 1. Try simple-icons
  let svg = await findInSimpleIcons(name)

  // 2. Try devicon
  if (!svg) {
    svg = findInDevicon(name)
  }

  // 3. Try custom local icon
  if (!svg) {
    svg = findCustomIcon(name)
  }

  if (!svg) {
    throw createError({ statusCode: 404, statusMessage: `Icon not found: ${name}` })
  }

  // Cache and serve
  cache.set(name, svg)
  setResponseHeader(event, 'Content-Type', 'image/svg+xml')
  setResponseHeader(event, 'Cache-Control', 'public, max-age=86400')
  return send(event, svg)
})
