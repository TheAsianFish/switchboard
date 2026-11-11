import type { Router } from '../api';
import { formatCount } from '../format';
import { Badge } from './Badge';

export function RouterPanel({ router }: { router: Router }) {
  const decisions = [...(router.decisions ?? [])].sort((a, b) => b.count - a.count);
  const max = decisions.reduce((m, d) => Math.max(m, d.count), 0);
  const candidates = router.candidates ?? [];
  return (
    <section aria-labelledby="router-h">
      <h2 id="router-h">Router</h2>
      <p>
        <Badge tone={router.enabled ? 'ok' : 'muted'}>{router.enabled ? 'enabled' : 'disabled'}</Badge>
      </p>
      {router.enabled && (
        <>
          <dl className="stats">
            <div>
              <dt>Target</dt>
              <dd className="num">{router.target}</dd>
            </div>
            <div>
              <dt>Fallback</dt>
              <dd>{router.fallback ?? '–'}</dd>
            </div>
            <div>
              <dt>Candidates</dt>
              <dd>{candidates.length > 0 ? candidates.join(', ') : '–'}</dd>
            </div>
          </dl>
          <div className="table-wrap">
            <table>
              <caption className="sr-only">Router decisions</caption>
              <thead>
                <tr>
                  <th scope="col">Model</th>
                  <th scope="col">Reason</th>
                  <th scope="col" className="num">
                    Count
                  </th>
                  <th scope="col">
                    <span className="sr-only">Share</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {decisions.length === 0 ? (
                  <tr>
                    <td colSpan={4} className="muted">
                      No decisions yet.
                    </td>
                  </tr>
                ) : (
                  decisions.map((d) => (
                    <tr key={`${d.model}/${d.reason}`}>
                      <th scope="row">{d.model}</th>
                      <td>{d.reason}</td>
                      <td className="num">{formatCount(d.count)}</td>
                      <td className="bar-cell" aria-hidden="true">
                        <span className="hbar" style={{ width: `${String(max > 0 ? (d.count / max) * 100 : 0)}%` }} />
                      </td>
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
