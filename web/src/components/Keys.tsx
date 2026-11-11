import type { Budget, Key } from '../api';
import { formatCount, formatLimit, formatUSD, percentOf } from '../format';

function BudgetCell({ b }: { b: Budget }) {
  const pct = percentOf(b.spent_usd, b.limit_usd);
  const reservedPct = percentOf(b.reserved_usd, b.limit_usd);
  const spentW = Math.min(100, pct);
  const reservedW = Math.min(100 - spentW, reservedPct);
  const label = `${formatUSD(b.spent_usd)} / ${formatUSD(b.limit_usd)} (${String(pct)}%)`;
  return (
    <div className="budget">
      <div
        className={pct >= 100 ? 'bar bar-full' : 'bar'}
        role="progressbar"
        aria-label="Budget spent"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.min(100, pct)}
        aria-valuetext={label}
      >
        <span className="bar-spent" style={{ width: `${String(spentW)}%` }} />
        <span className="bar-reserved" style={{ width: `${String(reservedW)}%` }} />
      </div>
      <div className="budget-text num">
        {label}
        {b.reserved_usd > 0 && <span> · {formatUSD(b.reserved_usd)} reserved</span>}
        <span className="muted"> · {b.period}</span>
      </div>
      {b.error && <div className="budget-error">budget unreadable: {b.error}</div>}
    </div>
  );
}

export function Keys({ keys }: { keys: Key[] }) {
  return (
    <section aria-labelledby="keys-h">
      <h2 id="keys-h">Keys</h2>
      <div className="table-wrap">
        <table>
          <caption className="sr-only">Configured API keys</caption>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Tenant</th>
              <th scope="col">Models</th>
              <th scope="col">Cache</th>
              <th scope="col">Request limit</th>
              <th scope="col">Token limit</th>
              <th scope="col">Budget</th>
              <th scope="col" className="num">
                Calls
              </th>
              <th scope="col" className="num">
                Spent
              </th>
            </tr>
          </thead>
          <tbody>
            {keys.length === 0 ? (
              <tr>
                <td colSpan={9} className="muted">
                  No keys configured.
                </td>
              </tr>
            ) : (
              keys.map((k) => (
                <tr key={k.name}>
                  <th scope="row">{k.name}</th>
                  <td>{k.tenant || '–'}</td>
                  <td>{k.models && k.models.length > 0 ? k.models.join(', ') : 'all'}</td>
                  <td>{k.cache || '–'}</td>
                  <td>{k.requests_limit ? formatLimit(k.requests_limit) : <span className="muted">none</span>}</td>
                  <td>{k.tokens_limit ? formatLimit(k.tokens_limit, 'tokens') : <span className="muted">none</span>}</td>
                  <td>{k.budget ? <BudgetCell b={k.budget} /> : <span className="muted">none</span>}</td>
                  <td className="num">{formatCount(k.calls)}</td>
                  <td className="num">{formatUSD(k.spent_usd)}</td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </section>
  );
}
