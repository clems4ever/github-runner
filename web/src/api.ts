// The daemon's REST surface, typed.
//
// Authentication is HTTP Basic and is handled by the browser: the daemon sends
// a WWW-Authenticate header, the browser asks once and remembers. There is no
// token in local storage, and nothing here to steal from a compromised page.

export type Runtime = 'vm' | 'container'
export type ScopeKind = 'repository' | 'organization'

export interface Pool {
  id: number
  name: string
  scopeKind: ScopeKind
  scope: string
  runtime: Runtime
  nested: boolean
  ephemeral: boolean
  replicas: number
  labels: string[]
  cpus: number
  memoryMb: number
  diskGb: number
  image: string
  credentialId: number
  enabled: boolean
  createdAt: string
  updatedAt: string
}

export type RunnerState = 'running' | 'stopping' | 'stopped'
export type JobState = 'busy' | 'idle' | 'offline' | 'unknown'

export interface Runner {
  name: string
  pool: string
  runtime: string
  state: RunnerState
  job: JobState
  generation: string
  upToDate: boolean
}

export interface Credential {
  id: number
  name: string
  hint: string
  createdAt: string
}

export interface Health {
  status: string
  version: string
  configured: boolean
}

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message)
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) },
  })

  if (!response.ok) {
    // The daemon puts something worth reading in "error"; anything else means
    // it never got that far.
    let message = response.statusText
    try {
      const body = await response.json()
      if (body?.error) message = body.error
    } catch {
      /* keep the status text */
    }
    throw new ApiError(message, response.status)
  }

  if (response.status === 204) return undefined as T
  return (await response.json()) as T
}

export const api = {
  health: () => request<Health>('/api/health'),

  pools: () => request<Pool[]>('/api/pools'),
  createPool: (pool: Partial<Pool>) =>
    request<Pool>('/api/pools', { method: 'POST', body: JSON.stringify(pool) }),
  updatePool: (id: number, pool: Partial<Pool>) =>
    request<Pool>(`/api/pools/${id}`, { method: 'PUT', body: JSON.stringify(pool) }),
  deletePool: (id: number) => request<void>(`/api/pools/${id}`, { method: 'DELETE' }),

  runners: () => request<{ runners: Runner[]; warnings: string[] }>('/api/runners'),
  reconcile: () => request<{ actions: unknown[]; errors: string[] }>('/api/reconcile', { method: 'POST' }),

  credentials: () => request<Credential[]>('/api/credentials'),
  createCredential: (name: string, token: string) =>
    request<Credential>('/api/credentials', { method: 'POST', body: JSON.stringify({ name, token }) }),
  rotateCredential: (id: number, token: string) =>
    request<void>(`/api/credentials/${id}/token`, { method: 'PUT', body: JSON.stringify({ token }) }),
  deleteCredential: (id: number) => request<void>(`/api/credentials/${id}`, { method: 'DELETE' }),

  settings: () => request<{ authUser: string; version: string }>('/api/settings'),
  setPassword: (user: string, password: string) =>
    request<void>('/api/settings/auth', { method: 'PUT', body: JSON.stringify({ user, password }) }),
}

/** A pool with the fields the daemon fills in itself left out. */
export function emptyPool(credentialId: number): Partial<Pool> {
  return {
    name: '',
    scopeKind: 'repository',
    scope: '',
    runtime: 'vm',
    nested: false,
    ephemeral: true,
    replicas: 1,
    labels: [],
    cpus: 2,
    memoryMb: 4096,
    diskGb: 40,
    image: 'default',
    credentialId,
    enabled: true,
  }
}

/**
 * The labels a pool's runners will actually register with: what the operator
 * typed, plus the ones describing what the runner is.
 *
 * This mirrors the daemon so the editor can show the real list as it is being
 * edited, rather than after saving.
 */
export function effectiveLabels(pool: Partial<Pool>): string[] {
  const out: string[] = []
  const seen = new Set<string>()
  const add = (label: string) => {
    const key = label.toLowerCase()
    if (!label || seen.has(key)) return
    seen.add(key)
    out.push(label)
  }

  add(pool.runtime === 'container' ? 'container' : 'vm')
  if (pool.nested) add('nested')
  if (pool.ephemeral) add('ephemeral')
  ;(pool.labels ?? []).forEach(add)
  return out
}
