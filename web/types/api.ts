export interface AuthResponse {
  token: string
  refresh_token: string
  expires_at: string
}

export interface WildcardStats {
  hostnames_total: number
  hostnames_alive: number
  hostnames_dead: number
  hostnames_unreachable: number
  web_services: number
}

export interface Wildcard {
  id: string
  value: string
  active: boolean
  last_recon_at: string | null
  last_revalidated_at: string | null
  stats: WildcardStats
}

export interface HostnameDNS {
  A?: string[]
  AAAA?: string[]
  CNAME?: string[]
}

export interface HostnamePorts {
  tcp: Record<string, { web: string | null }>
  udp: Record<string, unknown>
}

export interface Hostname {
  id: string
  wildcard_id: string
  fqdn: string
  ip: string
  cdn: string
  status: 'alive' | 'dead' | 'unreachable'
  type: 'web' | 'other' | 'unknown'
  last_seen_at: string
  created_at: string
  updated_at: string
  dns?: HostnameDNS
  ports?: HostnamePorts
}

export interface ChainEntry {
  url: string
  title: string | null
  headers: Record<string, string>
  status_code: number
  response_size: number
}

export interface Technology {
  name: string
  version: string
  category: string
}

export interface WebResult {
  id: string
  hostname_id: string
  url: string
  status_code: string
  title: string
  chain?: ChainEntry[]
  technologies?: Technology[]
  cookies?: unknown[]
  metadata?: Record<string, unknown>
  external_hosts?: string[]
  scanned_at: string
  created_at?: string
  updated_at?: string
}

export interface ReconJob {
  id: string
  wildcard_id: string
  mode: 'normal' | 'intensive'
  status: 'pending' | 'running' | 'completed' | 'failed' | 'timeout'
  scaleway_job_id: string
  started_at: string | null
  completed_at: string | null
  created_at: string
}

export interface ReconJobLaunchResponse {
  job_id: string
  scaleway_job_id: string
  status: string
}

export interface PaginatedResponse<T> {
  data: T[]
  total: number
  page: number
  per_page: number
}
