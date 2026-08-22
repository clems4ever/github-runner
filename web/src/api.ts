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
  /** What the pool falls back to when nothing is running. Never below one. */
  minReplicas: number
  /** The ceiling. Equal to the minimum, the pool is a fixed size. */
  maxReplicas: number
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
export type JobState = 'busy' | 'idle' | 'starting' | 'offline' | 'unknown'

export interface Runner {
  name: string
  pool: string
  runtime: string
  state: RunnerState
  job: JobState
  generation: string
  upToDate: boolean
  /**
   * What the host says is wrong with this runner, when it says anything.
   *
   * A runner can be dead and look busy: a unit that crashes on startup spends
   * most of its life in systemd's "activating" state, which read as running.
   * This is where the fleet admits it.
   */
  trouble?: string
}

export type CredentialKind = 'pat' | 'app'

export interface Credential {
  id: number
  name: string
  kind: CredentialKind
  /** Only meaningful for an app. */
  appId?: number
  installationId?: number
  hint: string
  createdAt: string
}

/** What goes in when a credential is created. The secret never comes back out. */
export interface NewCredential {
  name: string
  kind: CredentialKind
  /** A personal access token, or a GitHub App's PEM private key. */
  secret: string
  appId?: number
  installationId?: number
}

/**
 * A pool template: the portable form of a fleet's pools.
 *
 * Nothing local to one installation is in it — no pool ids, no credential, no
 * timestamps — so the same document imports anywhere. The import supplies the
 * credential, and may replace the scope.
 */
export interface PoolTemplate {
  version: number
  name?: string
  description?: string
  pools: unknown[]
}

export interface ImportRequest {
  document: unknown
  credentialId: number
  /** Replaces the scope of every pool in the document when given. */
  scope?: string
  scopeKind?: ScopeKind
  /** Import over pools of the same name instead of refusing. */
  replaceExisting?: boolean
  /** Report what would happen and write nothing. */
  dryRun?: boolean
}

/** What an import did to one pool, or — in a preview — would do. */
export interface ImportOutcome {
  name: string
  action: 'create' | 'update'
  pool: Pool
}

export interface ImportReport {
  pools: ImportOutcome[]
  dryRun: boolean
  name?: string
  description?: string
}

/** One point of fleet history: the whole fleet at a moment. */
export interface ActivityPoint {
  at: string
  running: number
  busy: number
}

/** What the autoscaler decided for a pool, and why. */
export interface Scale {
  target: number
  floor: number
  ceiling: number
  reason: string
  scaledUp: boolean
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
    /**
     * Where a person goes to fix this, when the daemon knows of such a place.
     * An app cannot widen its own access — GitHub does not allow it — so the
     * most that can be done is put the right page one click away.
     */
    readonly grantUrl?: string,
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
    let grantUrl: string | undefined
    try {
      const body = await response.json()
      if (body?.error) message = body.error
      if (body?.grantUrl) grantUrl = body.grantUrl
    } catch {
      /* keep the status text */
    }
    throw new ApiError(message, response.status, grantUrl)
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
  importPools: (body: ImportRequest) =>
    request<ImportReport>('/api/pools/import', { method: 'POST', body: JSON.stringify(body) }),
  /** The fleet's pools as a template, ready to be saved next to a repository. */
  exportPools: async () => {
    const document = await request<PoolTemplate>('/api/pools/export')
    return JSON.stringify(document, null, 2)
  },

  runners: () =>
    request<{ runners: Runner[]; warnings: string[]; scaling: Record<string, Scale> }>('/api/runners'),
  /** Pass a pool name to narrow the history to it; omit it for the whole fleet. */
  activity: (hours: number, pool?: string) =>
    request<{ points: ActivityPoint[]; pool: string; since: string; until: string }>(
      `/api/activity?hours=${hours}` + (pool ? `&pool=${encodeURIComponent(pool)}` : ''),
    ),

  reconcile: () => request<{ actions: unknown[]; errors: string[] }>('/api/reconcile', { method: 'POST' }),

  credentials: () => request<Credential[]>('/api/credentials'),
  createCredential: (credential: NewCredential) =>
    request<Credential>('/api/credentials', { method: 'POST', body: JSON.stringify(credential) }),
  rotateCredential: (id: number, secret: string) =>
    request<void>(`/api/credentials/${id}/secret`, { method: 'PUT', body: JSON.stringify({ secret }) }),
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
    minReplicas: 1,
    maxReplicas: 1,
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
  if (pool.nested) add('nestedvirt')
  if (pool.ephemeral) add('ephemeral')
  ;(pool.labels ?? []).forEach(add)
  return out
}
