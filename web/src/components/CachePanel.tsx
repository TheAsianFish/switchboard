import type { Cache } from '../api';
import { formatCount, formatPercent, formatUSD } from '../format';
import { Badge } from './Badge';

export function CachePanel({ cache }: { cache: Cache }) {
  const lookups = Object.entries(cache.lookups ?? {}).sort(([a], [b]) => a.localeCompare(b));
  return (
    <section aria-labelledby="cache-h">
      <h2 id="cache-h">Cache</h2>
      {!cache.enabled ? (
        <p>
          <Badge tone="muted">disabled</Badge>
        </p>
      ) : (
        <>
          <dl className="stats">
            <div>
              <dt>Hit rate</dt>
              <dd className="num">{formatPercent(cache.hit_rate)}</dd>
            </div>
            <div>
              <dt>Saved</dt>
              <dd className="num">{formatUSD(cache.saved_usd)}</dd>
            </div>
          </dl>
          <div className="table-wrap">
            <table>
              <caption className="sr-only">Cache lookups by status</caption>
              <thead>
                <tr>
                  <th scope="col">Status</th>
                  <th scope="col" className="num">
                    Lookups
                  </th>
                </tr>
              </thead>
              <tbody>
                {lookups.length === 0 ? (
                  <tr>
                    <td colSpan={2} className="muted">
                      No lookups yet.
                    </td>
                  </tr>
                ) : (
                  lookups.map(([status, n]) => (
                    <tr key={status}>
                      <th scope="row">{status}</th>
                      <td className="num">{formatCount(n)}</td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
        </>
      )}
    </section>
  );
}
