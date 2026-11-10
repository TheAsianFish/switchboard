import { act, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Overview } from './api';
import { App } from './App';
import { makeOverview } from './fixture';

function ok(body: Overview): Response {
  return new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } });
}

async function flush(ms = 0) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

function stubFetch(handler: (signal: AbortSignal | null | undefined) => Promise<Response>) {
  const fn = vi.fn((_url: string, init?: RequestInit) => handler(init?.signal));
  vi.stubGlobal('fetch', fn);
  return fn;
}

async function renderApp(body: Overview = makeOverview()) {
  const fn = stubFetch(() => Promise.resolve(ok(body)));
  const view = render(<App />);
  await flush();
  return { fn, ...view };
}

function rowOf(name: string | RegExp): HTMLElement {
  return screen.getByRole('row', { name });
}

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('header and fleet', () => {
  it('shows the answering replica and a fleet summary', async () => {
    await renderApp();
    expect(screen.getByRole('heading', { name: 'Switchboard' })).toBeTruthy();
    expect(screen.getByText('2 up / 3 total · 8.4 rps total')).toBeTruthy();
  });

  it('renders replicas with status badges and dims the down one', async () => {
    await renderApp();
    const fleet = within(screen.getByRole('region', { name: 'Fleet' }));
    const gw1 = fleet.getByRole('row', { name: /^gw-1/ });
    const gw3 = fleet.getByRole('row', { name: /^gw-3/ });
    expect(within(gw1).getByText('self')).toBeTruthy();
    expect(within(gw1).getByText('up').className).toContain('badge-ok');
    expect(within(gw3).getByText('down').className).toContain('badge-bad');
    expect(gw3.className).toContain('row-down');
    expect(gw1.className).not.toContain('row-down');
    expect(within(gw3).getByText('1 m ago')).toBeTruthy();
  });

  it('shows degraded components and provider breakers', async () => {
    await renderApp();
    const gw2 = screen.getByRole('row', { name: /^gw-2/ });
    expect(within(gw2).getByText('ratelimit on fallback')).toBeTruthy();
    expect(within(gw2).queryByText('budget on fallback')).toBeNull();
    expect(within(gw2).getByText('anthropic: open').className).toContain('badge-bad');
    expect(within(gw2).getByText('ollama: half-open').className).toContain('badge-warn');
  });

  it('copes with null collections', async () => {
    await renderApp(makeOverview({ replicas: null, keys: null, recent: null }));
    expect(screen.getByText('No replicas reported.')).toBeTruthy();
    expect(screen.getByText('No keys configured.')).toBeTruthy();
    expect(screen.getByText('No matching requests.')).toBeTruthy();
  });
});

describe('banners', () => {
  it('shows the fleet_error banner', async () => {
    await renderApp(makeOverview({ fleet_error: 'redis: connection refused' }));
    expect(screen.getByText('fleet view from this replica only: redis: connection refused')).toBeTruthy();
  });

  it('shows a banner when the view is not shared', async () => {
    await renderApp(makeOverview({ shared: false }));
    expect(screen.getByText(/^fleet view from this replica only:/)).toBeTruthy();
  });

  it('shows no banner for a healthy shared view', async () => {
    await renderApp();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByText(/fleet view from this replica only/)).toBeNull();
  });

  it('reports an unreachable API and keeps the last data greyed', async () => {
    let fail = false;
    stubFetch(() =>
      fail ? Promise.resolve(new Response('down', { status: 502 })) : Promise.resolve(ok(makeOverview())),
    );
    render(<App />);
    await flush();
    expect(screen.queryByRole('alert')).toBeNull();

    fail = true;
    await flush(2000);
    const alert = screen.getByRole('alert');
    expect(alert.textContent).toContain('admin API unreachable');
    expect(screen.getByRole('row', { name: /^gw-1/ })).toBeTruthy();
    expect(document.querySelector('main')?.className).toContain('is-stale');

    fail = false;
    await flush(2000);
    expect(screen.queryByRole('alert')).toBeNull();
    expect(document.querySelector('main')?.className).not.toContain('is-stale');
  });

  it('shows the banner without data when the first fetch fails', async () => {
    stubFetch(() => Promise.reject(new TypeError('failed')));
    render(<App />);
    await flush();
    expect(screen.getByRole('alert').textContent).toContain('admin API unreachable');
    expect(screen.queryByText('Fleet')).toBeNull();
  });
});

describe('keys, cache and router', () => {
  it('renders the budget bar with spent, limit, percent and reserved', async () => {
    await renderApp();
    const row = rowOf(/^team-a/);
    const bar = within(row).getByRole('progressbar');
    expect(bar.getAttribute('aria-valuenow')).toBe('25');
    expect(within(row).getByText(/\$1\.25 \/ \$5\.00 \(25%\)/)).toBeTruthy();
    expect(within(row).getByText(/\$0\.5000 reserved/)).toBeTruthy();
    expect(within(row).getByText('100 / 60 s, burst 20')).toBeTruthy();
    expect(within(row).getByText('all')).toBeTruthy();
  });

  it('shows the budget error and the allow list', async () => {
    await renderApp();
    const row = rowOf(/^broken-budget/);
    expect(within(row).getByText('budget unreadable: redis: connection refused')).toBeTruthy();
    expect(within(row).getByText('$0.000123')).toBeTruthy();
    expect(within(rowOf(/^batch/)).getByText('qwen2.5:3b, auto')).toBeTruthy();
  });

  it('shows cache stats, and disabled when off', async () => {
    const { unmount } = await renderApp();
    const cache = within(screen.getByRole('region', { name: 'Cache' }));
    expect(cache.getByText('25.0%')).toBeTruthy();
    expect(cache.getByText('hit-semantic')).toBeTruthy();
    unmount();

    await renderApp(makeOverview({ cache: { enabled: false, lookups: null, hit_rate: 0, saved_usd: 0 } }));
    expect(within(screen.getByRole('region', { name: 'Cache' })).getByText('disabled')).toBeTruthy();
  });

  it('shows router decisions', async () => {
    await renderApp();
    const router = within(screen.getByRole('region', { name: 'Router' }));
    expect(router.getByText('enabled')).toBeTruthy();
    expect(router.getByText('fallback', { selector: 'td' })).toBeTruthy();
    expect(router.getAllByRole('row').length).toBe(4);
  });
});

describe('recent requests', () => {
  it('shows requested to resolved model only when they differ', async () => {
    await renderApp();
    const recent = within(screen.getByRole('region', { name: 'Recent requests' }));
    expect(recent.getByText('auto → qwen2.5:3b')).toBeTruthy();
    expect(recent.getAllByText('qwen2.5:3b').length).toBeGreaterThan(0);
  });

  it('builds the trace link from the template', async () => {
    await renderApp();
    const link = screen.getByRole('link', { name: '4bf92f35' });
    expect(link.getAttribute('href')).toBe('http://localhost:16686/trace/4bf92f3577b34da6a3ce929d0e0e4736');
    expect(link.getAttribute('target')).toBe('_blank');
    expect(link.getAttribute('rel')).toBe('noreferrer');
  });

  it('shows plain ids without trace_url', async () => {
    await renderApp(makeOverview({ trace_url: undefined }));
    expect(screen.queryByRole('link')).toBeNull();
    expect(screen.getByText('4bf92f35')).toBeTruthy();
  });

  it('shows outcome badges with error kind, cost, and retry flags', async () => {
    await renderApp();
    const recent = within(screen.getByRole('region', { name: 'Recent requests' }));
    const badge = (text: string) => recent.getAllByText(text).find((el) => el.classList.contains('badge'))?.className;
    expect(badge('ok')).toContain('badge-ok');
    expect(badge('cache_hit')).toContain('badge-info');
    expect(badge('rate_limited')).toContain('badge-warn');
    expect(badge('budget_exhausted')).toContain('badge-warn');
    expect(badge('error')).toContain('badge-bad');
    expect(badge('abandoned')).toContain('badge-bad');
    expect(recent.getByText('upstream_5xx')).toBeTruthy();
    expect(recent.getByText('$0.000123')).toBeTruthy();
    expect(recent.getByText('hedged')).toBeTruthy();
    expect(recent.getByText('3 attempts')).toBeTruthy();
  });

  it('puts the router reason and probabilities in the tooltip', async () => {
    await renderApp();
    const cell = screen.getByText('auto → qwen2.5:3b').closest('td');
    expect(cell?.getAttribute('title')).toContain('qwen2.5:3b: 0.870');
  });

  it('filters by outcome and by text', async () => {
    vi.useRealTimers();
    stubFetch(() => Promise.resolve(ok(makeOverview())));
    const user = userEvent.setup();
    render(<App />);
    await screen.findByRole('region', { name: 'Recent requests' });

    const recent = within(screen.getByRole('region', { name: 'Recent requests' }));
    expect(recent.getAllByRole('row').length).toBe(7);

    await user.selectOptions(recent.getByLabelText('Outcome'), 'error');
    expect(recent.getAllByRole('row').length).toBe(2);
    expect(recent.getByText('Showing 1 of 6')).toBeTruthy();

    await user.selectOptions(recent.getByLabelText('Outcome'), '');
    await user.type(recent.getByLabelText('Key or model'), 'BROKEN');
    expect(recent.getAllByRole('row').length).toBe(3);
    await user.clear(recent.getByLabelText('Key or model'));
    await user.type(recent.getByLabelText('Key or model'), 'nothing-matches');
    expect(recent.getByText('No matching requests.')).toBeTruthy();
  });

  it('caps the table at 100 rows', async () => {
    const base = makeOverview();
    const first = base.recent?.[0];
    if (!first) throw new Error('fixture has no calls');
    const many = Array.from({ length: 130 }, (_, i) => ({ ...first, request_id: `r${String(i)}` }));
    await renderApp({ ...base, recent: many });
    const recent = within(screen.getByRole('region', { name: 'Recent requests' }));
    expect(recent.getAllByRole('row').length).toBe(101);
    expect(recent.getByText('Showing 100 of 130')).toBeTruthy();
  });
});

describe('polling', () => {
  it('fetches again every 2 seconds', async () => {
    const { fn } = await renderApp();
    expect(fn).toHaveBeenCalledTimes(1);
    await flush(1900);
    expect(fn).toHaveBeenCalledTimes(1);
    await flush(100);
    expect(fn).toHaveBeenCalledTimes(2);
    await flush(2000);
    expect(fn).toHaveBeenCalledTimes(3);
  });

  it('aborts the in-flight request and stops on unmount', async () => {
    let signal: AbortSignal | null | undefined;
    const fn = stubFetch((s) => {
      signal = s;
      return new Promise<Response>((_resolve, reject) => {
        s?.addEventListener('abort', () => {
          reject(new DOMException('aborted', 'AbortError'));
        });
      });
    });
    const { unmount } = render(<App />);
    await flush();
    expect(fn).toHaveBeenCalledTimes(1);
    expect(signal?.aborted).toBe(false);
    unmount();
    expect(signal?.aborted).toBe(true);
    await flush(10000);
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it('pauses while the tab is hidden and resumes when it is shown', async () => {
    let hidden = false;
    vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
    const { fn } = await renderApp();
    expect(fn).toHaveBeenCalledTimes(1);

    hidden = true;
    await flush(2000);
    await flush(6000);
    expect(fn).toHaveBeenCalledTimes(1);

    hidden = false;
    await act(async () => {
      document.dispatchEvent(new Event('visibilitychange'));
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(fn).toHaveBeenCalledTimes(2);
    await flush(2000);
    expect(fn).toHaveBeenCalledTimes(3);
  });
});
