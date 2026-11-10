import type { ReactNode } from 'react';

export type Tone = 'ok' | 'info' | 'warn' | 'bad' | 'muted';

export function Badge({ tone, title, children }: { tone: Tone; title?: string; children: ReactNode }) {
  return (
    <span className={`badge badge-${tone}`} title={title}>
      {children}
    </span>
  );
}

export function statusTone(status: string): Tone {
  switch (status) {
    case 'up':
      return 'ok';
    case 'stale':
      return 'warn';
    case 'down':
      return 'bad';
    default:
      return 'muted';
  }
}

export function breakerTone(state: string): Tone {
  switch (state) {
    case 'closed':
      return 'ok';
    case 'half-open':
      return 'warn';
    case 'open':
      return 'bad';
    default:
      return 'muted';
  }
}

export function outcomeTone(outcome: string): Tone {
  switch (outcome) {
    case 'ok':
      return 'ok';
    case 'cache_hit':
      return 'info';
    case 'rate_limited':
    case 'budget_exhausted':
      return 'warn';
    case 'error':
    case 'abandoned':
      return 'bad';
    default:
      return 'muted';
  }
}
