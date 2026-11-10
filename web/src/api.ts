// Types mirror internal/admin/api.go; field names are its json tags. Go
// nil slices, maps and pointers arrive as null (or are omitted), so they are
// typed as optional and nullable.

export type Nullable<T> = T | null | undefined;

export type ReplicaStatus = 'up' | 'stale' | 'down';
export type BreakerState = 'closed' | 'open' | 'half-open';

export interface Window {
  seconds: number;
  requests: number;
  rps: number;
  errors: number;
  error_rate: number;
  p50_ms: number;
  p99_ms: number;
  cache_hit_rate: number;
}

export interface ProviderHealth {
  provider: string;
  breaker: BreakerState | (string & {});
  retry_after_ms?: number;
  hedging: boolean;
  hedge_delay_ms?: Nullable<Record<string, number>>;
}

export interface Replica {
  id: string;
  version: string;
  started_at: string;
  last_seen: string;
  age_seconds: number;
  status: ReplicaStatus | (string & {});
  self: boolean;
  window: Window;
  requests: number;
  outcomes: Nullable<Record<string, number>>;
  spent_usd: number;
  degraded: Nullable<Record<string, boolean>>;
  providers: Nullable<ProviderHealth[]>;
}

export interface Limit {
  rate: number;
  period_seconds: number;
  burst: number;
}

export interface Budget {
  limit_usd: number;
  period: string;
  window?: string;
  spent_usd: number;
  reserved_usd: number;
  error?: string;
}

export interface Key {
  name: string;
  tenant: string;
  models: Nullable<string[]>;
  cache: string;
  requests_limit: Nullable<Limit>;
  tokens_limit: Nullable<Limit>;
  budget: Nullable<Budget>;
  calls: number;
  spent_usd: number;
}

export interface Cache {
  enabled: boolean;
  lookups: Nullable<Record<string, number>>;
  hit_rate: number;
  saved_usd: number;
}

export interface Decision {
  model: string;
  reason: string;
  count: number;
}

export interface Router {
  enabled: boolean;
  target: number;
  candidates: Nullable<string[]>;
  fallback?: string;
  decisions: Nullable<Decision[]>;
}

export interface Call {
  time: string;
  replica: string;
  request_id: string;
  trace_id?: string;
  key: string;
  requested_model: string;
  model: string;
  provider?: string;
  stream: boolean;
  outcome: string;
  error_kind?: string;
  cache?: string;
  router_reason?: string;
  router_probs?: Nullable<Record<string, number>>;
  duration_ms: number;
  ttft_ms?: number;
  input_tokens: number;
  output_tokens: number;
  cost_usd: number;
  attempts?: number;
  hedged?: boolean;
}

export interface Overview {
  generated_at: string;
  self: string;
  shared: boolean;
  fleet_error?: string;
  replicas: Nullable<Replica[]>;
  keys: Nullable<Key[]>;
  cache: Cache;
  router: Router;
  recent: Nullable<Call[]>;
  trace_url?: string;
}

export const OVERVIEW_URL = '/api/overview';

export type ApiErrorKind = 'network' | 'http' | 'parse';

export class ApiError extends Error {
  readonly kind: ApiErrorKind;
  readonly status: number | undefined;

  constructor(kind: ApiErrorKind, message: string, status?: number, options?: ErrorOptions) {
    super(message, options);
    this.name = 'ApiError';
    this.kind = kind;
    this.status = status;
  }
}

export function isAbortError(err: unknown): boolean {
  return typeof err === 'object' && err !== null && 'name' in err && err.name === 'AbortError';
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/**
 * Fetches the overview. Aborts reject with the original AbortError; every
 * other failure is an ApiError (network, non-2xx status, or a body that is
 * not a JSON object with the expected top-level shape).
 */
export async function fetchOverview(signal?: AbortSignal): Promise<Overview> {
  let res: Response;
  try {
    res = await fetch(OVERVIEW_URL, { signal: signal ?? null, headers: { Accept: 'application/json' } });
  } catch (err) {
    if (isAbortError(err)) throw err;
    throw new ApiError('network', 'admin API unreachable', undefined, { cause: err });
  }
  if (!res.ok) {
    throw new ApiError('http', `admin API returned ${String(res.status)}`, res.status);
  }
  let body: unknown;
  try {
    body = JSON.parse(await res.text());
  } catch (err) {
    if (isAbortError(err)) throw err;
    throw new ApiError('parse', 'admin API returned invalid JSON', res.status, { cause: err });
  }
  if (!isObject(body) || typeof body.self !== 'string' || !isObject(body.cache) || !isObject(body.router)) {
    throw new ApiError('parse', 'admin API returned an unexpected document', res.status);
  }
  return body as unknown as Overview;
}
