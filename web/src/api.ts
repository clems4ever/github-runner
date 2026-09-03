// The daemon's REST surface, typed.
//
// Authentication is HTTP Basic and is handled by the browser: the daemon sends
// a WWW-Authenticate header, the browser asks once and remembers. There is no
// token in local storage, and nothing here to steal from a compromised page.

export type Runtime = "vm" | "container";
export type ScopeKind = "repository" | "organization";

/**
 * How much a pool trusts the repository it serves.
 *
 * A repository can publish a file saying what its jobs need, and the daemon
 * builds it a layer on top of the pool's image. What is being decided is not
 * small — a recipe is a root shell on a build machine on this host — so it is
 * off until an operator turns it on, per pool.
 */
export type LayerPolicy = "off" | "approve" | "trust";

export interface Pool {
  id: number;
  name: string;
  scopeKind: ScopeKind;
  scope: string;
  runtime: Runtime;
  nested: boolean;
  ephemeral: boolean;
  /** What the pool falls back to when nothing is running. Never below one. */
  minReplicas: number;
  /** The ceiling. Equal to the minimum, the pool is a fixed size. */
  maxReplicas: number;
  labels: string[];
  cpus: number;
  memoryMb: number;
  diskGb: number;
  image: string;
  /** apt packages baked into this pool's image, on top of the ones every runner gets. */
  packages: string[];
  /** A shell script run as root while the image is built, after the packages are in. */
  recipe: string;
  /**
   * Whether this pool reads its repository's own definition, and whether an
   * operator has to approve each one. Only ever anything but 'off' on a
   * repository-scoped machine pool.
   */
  layers: LayerPolicy;
  credentialId: number;
  enabled: boolean;
  createdAt: string;
  updatedAt: string;
}

/**
 * Where a pool's golden image stands on this host.
 *
 * A machine pool has no runners until its image is built, so this is not
 * decoration: it is the difference between a pool that is empty because it is
 * idle and one that is empty because its recipe does not work.
 */
export type ImageState =
  "ready" | "unbuilt" | "queued" | "building" | "failed" | "none";

/** Where one attempt at building an image got to. */
export type ImagePhase =
  "queued" | "fetching" | "running" | "succeeded" | "failed";

/**
 * One attempt at building a pool's image, kept with everything it printed.
 *
 * Kept, and not only while it matters: the log of the build that failed last
 * Tuesday is the evidence for what a recipe did.
 */
export interface ImageBuild {
  id: number;
  pool: string;
  image: string;
  phase: ImagePhase;
  /** Who asked: the daemon needing an image, or somebody pressing the button. */
  trigger: "automatic" | "requested" | string;
  error?: string;
  startedAt: string;
  endedAt?: string;
  /** How long it has been going, or how long it took. */
  seconds: number;
  /** The last thing its log said. */
  detail?: string;
  /** It has printed nothing for a long time. Not the same as dead. */
  silent?: boolean;
  /** There is something to read. */
  hasLog: boolean;
}

/** A pool's image: where it stands, and the attempt that says so. */
export interface PoolImage {
  pool: string;
  image: string;
  state: ImageState;
  /** Whether this pool may have runners at all. */
  ready: boolean;
  summary: string;
  build?: ImageBuild;
}

/** What an operator has said about one definition. */
export type LayerApproval = "pending" | "approved" | "refused";

/**
 * One definition a repository has published, and what became of it.
 *
 * Identified by a digest over what would actually run — the effective package
 * list and the recipe — rather than over the file, so that reformatting the
 * file is not a new question and changing a command always is.
 */
export interface RepoLayer {
  id: number;
  pool: string;
  repo: string;
  digest: string;
  packages: string[];
  recipe: string;
  /** The layer's file name, once it has been built. */
  image: string;
  approval: LayerApproval;
  /** Who decided. 'policy' is a pool on trust, where nobody read it. */
  decidedBy: string;
  firstSeen: string;
  lastSeen: string;
  decidedAt: string;
}

export type RunnerState = "running" | "stopping" | "stopped";
export type JobState = "busy" | "idle" | "starting" | "offline" | "unknown";

export interface Runner {
  name: string;
  pool: string;
  runtime: string;
  state: RunnerState;
  job: JobState;
  generation: string;
  upToDate: boolean;
  /**
   * The host is bringing this runner up right now — launching it, or waiting
   * out the restart delay between two machines.
   */
  coming?: boolean;
  /**
   * What the host says is wrong with this runner, when it says anything.
   *
   * A runner can be dead and look busy: a unit that crashes on startup spends
   * most of its life in systemd's "activating" state, which read as running.
   * This is where the fleet admits it.
   */
  trouble?: string;
}

export type CredentialKind = "pat" | "app";

export interface Credential {
  id: number;
  name: string;
  kind: CredentialKind;
  /** Only meaningful for an app. */
  appId?: number;
  installationId?: number;
  hint: string;
  createdAt: string;
}

/** What goes in when a credential is created. The secret never comes back out. */
export interface NewCredential {
  name: string;
  kind: CredentialKind;
  /** A personal access token, or a GitHub App's PEM private key. */
  secret: string;
  appId?: number;
  installationId?: number;
}

/**
 * A pool template: the portable form of a fleet's pools.
 *
 * Nothing local to one installation is in it — no pool ids, no credential, no
 * timestamps — so the same document imports anywhere. The import supplies the
 * credential, and may replace the scope.
 */
export interface PoolTemplate {
  version: number;
  name?: string;
  description?: string;
  pools: unknown[];
}

export interface ImportRequest {
  document: unknown;
  credentialId: number;
  /** Replaces the scope of every pool in the document when given. */
  scope?: string;
  scopeKind?: ScopeKind;
  /** Import over pools of the same name instead of refusing. */
  replaceExisting?: boolean;
  /** Report what would happen and write nothing. */
  dryRun?: boolean;
}

/** What an import did to one pool, or — in a preview — would do. */
export interface ImportOutcome {
  name: string;
  action: "create" | "update";
  pool: Pool;
}

export interface ImportReport {
  pools: ImportOutcome[];
  dryRun: boolean;
  name?: string;
  description?: string;
}

/** One point of fleet history: the whole fleet at a moment. */
export interface ActivityPoint {
  at: string;
  running: number;
  busy: number;
}

/**
 * A window of fleet history, with what it could have been narrowed to.
 *
 * `scopes` is every repository and organisation the window has history for,
 * sent whatever the filter is — including scopes whose pool has since been
 * deleted, because the hours it worked still happened.
 */
export interface ActivityHistory {
  points: ActivityPoint[];
  pool: string;
  scope: string;
  scopes: string[];
  since: string;
  until: string;
}

/**
 * What one pool has run over a window.
 *
 * Both figures are observed rather than reported by GitHub: the daemon asks
 * what each runner is doing once a reconcile pass and adds up what it was
 * told. A job shorter than one pass is never seen. They are close enough to
 * size a pool from and not exact enough to bill anybody for.
 */
export interface PoolJobs {
  pool: string;
  jobs: number;
  /** Runner-time, not wall-clock: two runners busy for a minute is two minutes. */
  seconds: number;
}

/** One pool's tally for one UTC day. */
export interface JobDay {
  /** YYYY-MM-DD, UTC. */
  day: string;
  pool: string;
  jobs: number;
  seconds: number;
}

/** What the whole machine is doing. */
export interface HostResources {
  cpus: number;
  /** 0 to 100 across every core together, not per core. */
  cpuPercent: number;
  memoryUsedBytes: number;
  memoryTotalBytes: number;
  /** The filesystem the fleet fills: golden images and every machine's disk. */
  diskPath: string;
  diskUsedBytes: number;
  diskTotalBytes: number;
  load1: number;
  load5: number;
  load15: number;
}

/** What one runner is consuming. */
export interface RunnerUsage {
  name: string;
  pool: string;
  runtime: string;
  /**
   * Null until the runner has been measured twice.
   *
   * Processor time is a counter, so a rate needs two readings and a runner
   * created a moment ago has one. Zero would show a machine that is busily
   * booting as idle, so the daemon says nothing instead.
   */
  cpuPercent: number | null;
  memoryBytes: number;
}

/** What the pools would take if every one of them grew to its ceiling. */
export interface Commitment {
  runners: number;
  cpus: number;
  memoryBytes: number;
  /** Machines only: a container reserves no disk. */
  diskBytes: number;
}

/**
 * What the whole fleet may take from this host, as opposed to what any one
 * pool was promised.
 *
 * Zero in any field is that dimension uncapped, which is what an install that
 * has never been configured has.
 */
export interface Budget {
  /** Processors across every machine together. 0 is no cap. */
  cpus: number;
  /**
   * The fleet's share when the host is contended, which is a different
   * question from the cap. systemd's default is 100; 0 leaves it alone.
   */
  cpuWeight: number;
  /** MiB across every machine together. 0 is no cap. */
  memoryMb: number;
  /**
   * GiB across the machines' disks and the golden images underneath them
   * together, because they share one filesystem. 0 is no cap.
   *
   * Unlike the other two this is not held by a control group: the daemon does
   * not start a machine that would cross it, and collects golden images
   * nothing is booting until the fleet fits back underneath.
   */
  diskGb: number;
  /**
   * Whether to add a hard limit above the ceiling, past which the kernel kills
   * a machine mid-job. Off by default: the ceiling on its own makes the fleet
   * slower, and the alternative costs somebody their job.
   */
  hardMemory: boolean;
}

/**
 * The host and its runners at one moment.
 *
 * `ready` is false for the first second after the daemon starts, before it has
 * taken a reading. It is not an error, and it is not a host with no memory.
 */
export interface ResourceReport {
  ready: boolean;
  at?: string;
  host?: HostResources;
  runners?: RunnerUsage[];
  warnings?: string[];
  committed?: Commitment;
  /** What the fleet is allowed to take, beside what it has promised itself. */
  budget?: Budget;
}

/** One point of host history. Percentages, so three units share one axis. */
export interface HostPoint {
  at: string;
  cpuPercent: number;
  memoryPercent: number;
  diskPercent: number;
}

/** What the autoscaler decided for a pool, and why. */
export interface Scale {
  target: number;
  floor: number;
  ceiling: number;
  reason: string;
  scaledUp: boolean;
}

export interface Health {
  status: string;
  version: string;
  configured: boolean;
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
    super(message);
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    ...init,
    headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
  });

  if (!response.ok) {
    // The daemon puts something worth reading in "error"; anything else means
    // it never got that far.
    let message = response.statusText;
    let grantUrl: string | undefined;
    try {
      const body = await response.json();
      if (body?.error) message = body.error;
      if (body?.grantUrl) grantUrl = body.grantUrl;
    } catch {
      /* keep the status text */
    }
    throw new ApiError(message, response.status, grantUrl);
  }

  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export const api = {
  health: () => request<Health>("/api/health"),

  pools: () => request<Pool[]>("/api/pools"),
  createPool: (pool: Partial<Pool>) =>
    request<Pool>("/api/pools", { method: "POST", body: JSON.stringify(pool) }),
  updatePool: (id: number, pool: Partial<Pool>) =>
    request<Pool>(`/api/pools/${id}`, {
      method: "PUT",
      body: JSON.stringify(pool),
    }),
  deletePool: (id: number) =>
    request<void>(`/api/pools/${id}`, { method: "DELETE" }),
  importPools: (body: ImportRequest) =>
    request<ImportReport>("/api/pools/import", {
      method: "POST",
      body: JSON.stringify(body),
    }),
  /** The fleet's pools as a template, ready to be saved next to a repository. */
  exportPools: async () => {
    const document = await request<PoolTemplate>("/api/pools/export");
    return JSON.stringify(document, null, 2);
  },

  runners: () =>
    request<{
      runners: Runner[];
      warnings: string[];
      scaling: Record<string, Scale>;
    }>("/api/runners"),
  /** Pass a pool name to narrow the history to it; omit it for the whole fleet. */
  activity: (hours: number, pool?: string, scope?: string) =>
    request<ActivityHistory>(
      `/api/activity?hours=${hours}` +
        (pool ? `&pool=${encodeURIComponent(pool)}` : "") +
        (scope ? `&scope=${encodeURIComponent(scope)}` : ""),
    ),

  /**
   * What each pool has run, over a window of whole UTC days. Kept for a
   * quarter, unlike the activity history, because sizing is argued about with
   * weeks of evidence rather than with this afternoon's.
   */
  jobs: (days: number) =>
    request<{
      pools: PoolJobs[];
      days: JobDay[];
      since: string;
      until: string;
    }>(`/api/jobs?days=${days}`),

  /** Where every pool's image stands, which is what the pools table shows. */
  poolImages: () => request<PoolImage[]>("/api/pool-images"),
  /** One pool's image and every attempt this host has made at it. */
  poolImage: (id: number) =>
    request<{ status: PoolImage; builds: ImageBuild[] }>(
      `/api/pools/${id}/image`,
    ),
  /**
   * Ask for a build. A failed one is never retried on its own — a recipe that
   * cannot work should fail once, not for ever — so this is how it is tried
   * again.
   */
  buildPoolImage: (id: number) =>
    request<ImageBuild>(`/api/pools/${id}/image/builds`, { method: "POST" }),
  /** The whole account of one build. It is a console: read, not parsed. */
  imageBuildLog: async (id: number): Promise<string> => {
    const response = await fetch(`/api/image-builds/${id}/log`);
    if (!response.ok) throw new ApiError(response.statusText, response.status);
    return response.text();
  },

  /**
   * What repositories have asked their pools for. A pool name narrows it;
   * without one it is the whole fleet's queue of decisions.
   */
  layers: (pool?: string) =>
    request<RepoLayer[]>(
      "/api/layers" + (pool ? `?pool=${encodeURIComponent(pool)}` : ""),
    ),
  /**
   * Approve or refuse one. The digest goes with it: it says which definition
   * was on screen, so a repository that edits its file while the page is open
   * gets a refusal rather than an approval of something nobody read.
   */
  decideLayer: (id: number, approval: "approved" | "refused", digest: string) =>
    request<RepoLayer>(`/api/layers/${id}/decision`, {
      method: "POST",
      body: JSON.stringify({ approval, digest }),
    }),

  /** What the host and its runners are using, as of the daemon's last reading. */

  resources: () => request<ResourceReport>("/api/resources"),
  /** What the host has been using, over a window. */
  resourceHistory: (hours: number) =>
    request<{ points: HostPoint[]; since: string; until: string }>(
      `/api/resources/history?hours=${hours}`,
    ),

  reconcile: () =>
    request<{ actions: unknown[]; errors: string[] }>("/api/reconcile", {
      method: "POST",
    }),

  credentials: () => request<Credential[]>("/api/credentials"),
  createCredential: (credential: NewCredential) =>
    request<Credential>("/api/credentials", {
      method: "POST",
      body: JSON.stringify(credential),
    }),
  rotateCredential: (id: number, secret: string) =>
    request<void>(`/api/credentials/${id}/secret`, {
      method: "PUT",
      body: JSON.stringify({ secret }),
    }),
  deleteCredential: (id: number) =>
    request<void>(`/api/credentials/${id}`, { method: "DELETE" }),

  settings: () =>
    request<{ authUser: string; version: string; budget: Budget }>(
      "/api/settings",
    ),
  setBudget: (budget: Budget) =>
    request<Budget>("/api/settings/budget", {
      method: "PUT",
      body: JSON.stringify(budget),
    }),
  setPassword: (user: string, password: string) =>
    request<void>("/api/settings/auth", {
      method: "PUT",
      body: JSON.stringify({ user, password }),
    }),
};

/** A pool with the fields the daemon fills in itself left out. */
export function emptyPool(credentialId: number): Partial<Pool> {
  return {
    name: "",
    scopeKind: "repository",
    scope: "",
    runtime: "vm",
    nested: false,
    ephemeral: true,
    minReplicas: 1,
    maxReplicas: 1,
    labels: [],
    cpus: 2,
    memoryMb: 4096,
    diskGb: 40,
    image: "default",
    packages: [],
    recipe: "",
    layers: "off",
    credentialId,
    enabled: true,
  };
}

/** The largest a pool may be. The daemon enforces the same number. */
export const maxReplicas = 64;

/** Whether a pool is a fixed size rather than one the autoscaler moves. */
export function isFixed(pool: Pool): boolean {
  return pool.maxReplicas <= pool.minReplicas;
}

/**
 * The same pool, one runner bigger or smaller — or null when it cannot go that
 * way.
 *
 * A step moves the ceiling, because the ceiling is the number that decides how
 * big a pool is allowed to get; the autoscaler owns everything below it. A
 * fixed pool has no room underneath, so both bounds move together and it stays
 * fixed. An autoscaling one stops a step above its minimum for the mirror
 * reason: nudging a row in a list should never quietly switch a pool's kind.
 */
export function scaled(pool: Pool, delta: number): Pool | null {
  const fixed = isFixed(pool);
  const max = pool.maxReplicas + delta;
  const min = fixed ? max : pool.minReplicas;
  if (min < 1 || max > maxReplicas) return null;
  if (fixed ? max < min : max <= min) return null;
  return { ...pool, minReplicas: min, maxReplicas: max };
}

/**
 * The labels a pool's runners will actually register with: what the operator
 * typed, plus the ones describing what the runner is.
 *
 * This mirrors the daemon so the editor can show the real list as it is being
 * edited, rather than after saving.
 */
export function effectiveLabels(pool: Partial<Pool>): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  const add = (label: string) => {
    const key = label.toLowerCase();
    if (!label || seen.has(key)) return;
    seen.add(key);
    out.push(label);
  };

  add(pool.runtime === "container" ? "container" : "vm");
  if (pool.nested) add("nestedvirt");
  if (pool.ephemeral) add("ephemeral");
  (pool.labels ?? []).forEach(add);
  return out;
}
