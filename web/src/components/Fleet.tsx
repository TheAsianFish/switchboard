import type { Replica } from '../api';
import { formatAgo, formatCount, formatMS, formatPercent, formatRate, formatUSD } from '../format';
import { Badge, breakerTone, statusTone } from './Badge';

function ReplicaRow({ r }: { r: Replica }) {
  const degraded = Object.entries(r.degraded ?? {})
    .filter(([, on]) => on)
    .map(([name]) => name)
    .sort();
  const providers = r.providers ?? [];
  return (
    <tr className={r.status === 'down' ? 'row-down' : undefined}>
      <th scope="row">
        {r.id}
        {r.self && (
          <>
            {' '}
            <Badge tone="info">self</Badge>
          </>
        )}
      </th>
      <td>
        <Badge tone={statusTone(r.status)}>{r.status}</Badge>
      </td>
      <td className="num">{formatAgo(r.age_seconds)}</td>
      <td>{r.version || '–'}</td>
      <td className="num">{formatRate(r.window.rps)}</td>
      <td className="num">{formatPercent(r.window.error_rate)}</td>
      <td className="num">{formatMS(r.window.p50_ms)}</td>
      <td className="num">{formatMS(r.window.p99_ms)}</td>
      <td className="num">{formatPercent(r.window.cache_hit_rate)}</td>
      <td className="num">{formatCount(r.requests)}</td>
      <td className="num">{formatUSD(r.spent_usd)}</td>
      <td>
        <div className="badges">
          {degraded.length === 0 ? (
            <span className="muted">none</span>
          ) : (
            degraded.map((name) => (
              <Badge key={name} tone="warn">
                {name} on fallback
              </Badge>
            ))
          )}
        </div>
      </td>
      <td>
        <div className="badges">
          {providers.length === 0 ? (
            <span className="muted">none</span>
          ) : (
            providers.map((p) => {
              const hedge = Object.entries(p.hedge_delay_ms ?? {})
                .map(([model, ms]) => `${model}: ${formatMS(ms)}`)
                .join(', ');
              const title = [
                p.hedging ? 'hedging on' : 'hedging off',
                hedge ? `hedge delay ${hedge}` : '',
                p.retry_after_ms ? `retry after ${formatMS(p.retry_after_ms)}` : '',
              ]
                .filter(Boolean)
                .join('; ');
              return (
                <Badge key={p.provider} tone={breakerTone(p.breaker)} title={title}>
                  {p.provider}: {p.breaker}
                </Badge>
              );
            })
          )}
        </div>
      </td>
    </tr>
  );
}

export function Fleet({ replicas }: { replicas: Replica[] }) {
  const up = replicas.filter((r) => r.status === 'up');
  const rps = up.reduce((sum, r) => sum + r.window.rps, 0);
  return (
    <section aria-labelledby="fleet-h">
      <h2 id="fleet-h">Fleet</h2>
      <p className="summary">
        {up.length} up / {replicas.length} total · {formatRate(rps)} rps total
      </p>
      <div className="table-wrap">
        <table>
          <caption className="sr-only">Gateway replicas</caption>
          <thead>
            <tr>
              <th scope="col">Replica</th>
              <th scope="col">Status</th>
              <th scope="col">Last seen</th>
              <th scope="col">Version</th>
              <th scope="col" className="num">
                rps
              </th>
              <th scope="col" className="num">
                Errors
              </th>
              <th scope="col" className="num">
                p50
              </th>
              <th scope="col" className="num">
                p99
              </th>
              <th scope="col" className="num">
                Cache hits
              </th>
              <th scope="col" className="num">
                Requests
              </th>
              <th scope="col" className="num">
                Spent
              </th>
              <th scope="col">Degraded</th>
              <th scope="col">Providers</th>
            </tr>
          </thead>
          <tbody>
            {replicas.length === 0 ? (
              <tr>
                <td colSpan={13} className="muted">
                  No replicas reported.
                </td>
              </tr>
            ) : (
              replicas.map((r) => <ReplicaRow key={r.id} r={r} />)
            )}
          </tbody>
        </table>
      </div>
    </section>
  );
}
