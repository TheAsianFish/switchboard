import type { Limit } from './api';

/** Dollars: two decimals from $1, four from a cent, six below that. */
export function formatUSD(n: number): string {
  if (!Number.isFinite(n)) return '–';
  const a = Math.abs(n);
  if (a === 0) return '$0.00';
  if (a >= 1) return `$${n.toFixed(2)}`;
  if (a >= 0.01) return `$${n.toFixed(4)}`;
  return `$${n.toFixed(6)}`;
}

/** Milliseconds; 0 or negative means "not measured". */
export function formatMS(ms: number | undefined): string {
  if (ms === undefined || !Number.isFinite(ms) || ms <= 0) return '–';
  if (ms >= 1000) return `${(ms / 1000).toFixed(2)} s`;
  if (ms >= 100) return `${String(Math.round(ms))} ms`;
  return `${ms.toFixed(1)} ms`;
}

/** A fraction (0..1) as a percentage with one decimal. */
export function formatPercent(rate: number): string {
  if (!Number.isFinite(rate)) return '–';
  return `${(rate * 100).toFixed(1)}%`;
}

export function formatRate(rps: number): string {
  if (!Number.isFinite(rps)) return '–';
  return rps >= 100 ? rps.toFixed(0) : rps.toFixed(1);
}

export function formatCount(n: number): string {
  return Number.isFinite(n) ? n.toLocaleString('en-US') : '–';
}

/** "3 s ago", "2 m ago", "1 h ago". */
export function formatAgo(seconds: number): string {
  if (!Number.isFinite(seconds)) return '–';
  const s = Math.max(0, Math.round(seconds));
  if (s < 1) return 'just now';
  if (s < 60) return `${String(s)} s ago`;
  if (s < 3600) return `${String(Math.floor(s / 60))} m ago`;
  if (s < 86400) return `${String(Math.floor(s / 3600))} h ago`;
  return `${String(Math.floor(s / 86400))} d ago`;
}

function pad(n: number, width = 2): string {
  return String(n).padStart(width, '0');
}

/** Local time as HH:MM:SS.mmm, or the input when it is not a date. */
export function formatTime(iso: string, withMillis = true): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const base = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  return withMillis ? `${base}.${pad(d.getMilliseconds(), 3)}` : base;
}

/** "100 / 60 s, burst 20"; a unit such as "tokens" goes after the rate. */
export function formatLimit(l: Limit, unit?: string): string {
  const rate = unit ? `${formatCount(l.rate)} ${unit}` : formatCount(l.rate);
  return `${rate} / ${String(l.period_seconds)} s, burst ${formatCount(l.burst)}`;
}

/** Spent share of a limit as a whole percent; 0 when there is no limit. */
export function percentOf(part: number, limit: number): number {
  if (!(limit > 0) || !Number.isFinite(part)) return 0;
  return Math.round((part / limit) * 100);
}

/**
 * Builds a trace link from the template. Returns undefined without a
 * template or trace id, and for anything that is not an http(s) or
 * absolute-path URL.
 */
export function traceHref(template: string | undefined, traceId: string | undefined): string | undefined {
  if (!template || !traceId) return undefined;
  const href = template.split('{trace_id}').join(encodeURIComponent(traceId));
  return /^(https?:\/\/|\/)/i.test(href) ? href : undefined;
}

export function shortId(id: string, len = 8): string {
  return id.length > len ? id.slice(0, len) : id;
}
