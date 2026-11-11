import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError, fetchOverview } from './api';
import { makeOverview } from './fixture';

afterEach(() => {
  vi.unstubAllGlobals();
});

function stub(res: Response | Error) {
  const fn = vi.fn<typeof fetch>(() => (res instanceof Response ? Promise.resolve(res) : Promise.reject(res)));
  vi.stubGlobal('fetch', fn);
  return fn;
}

describe('fetchOverview', () => {
  it('returns the parsed document and requests /api/overview', async () => {
    const want = makeOverview();
    const fn = stub(new Response(JSON.stringify(want)));
    const got = await fetchOverview();
    expect(got.self).toBe('gw-1');
    expect(fn.mock.calls[0]?.[0]).toBe('/api/overview');
  });

  it('accepts null collections', async () => {
    const doc = { ...makeOverview(), replicas: null, keys: null, recent: null };
    stub(new Response(JSON.stringify(doc)));
    const got = await fetchOverview();
    expect(got.replicas).toBeNull();
  });

  it('throws a typed http error on non-2xx', async () => {
    stub(new Response('boom', { status: 503 }));
    await expect(fetchOverview()).rejects.toMatchObject({ name: 'ApiError', kind: 'http', status: 503 });
  });

  it('throws a typed parse error on bad JSON and wrong shape', async () => {
    stub(new Response('<html>'));
    await expect(fetchOverview()).rejects.toMatchObject({ kind: 'parse' });
    stub(new Response('[1,2]'));
    await expect(fetchOverview()).rejects.toBeInstanceOf(ApiError);
  });

  it('throws a typed network error when fetch rejects', async () => {
    stub(new TypeError('failed to fetch'));
    await expect(fetchOverview()).rejects.toMatchObject({ kind: 'network' });
  });

  it('passes aborts through unchanged', async () => {
    const abort = new DOMException('aborted', 'AbortError');
    stub(abort);
    await expect(fetchOverview()).rejects.toBe(abort);
  });
});
