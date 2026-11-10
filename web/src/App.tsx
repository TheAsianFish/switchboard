import type { Overview } from './api';
import { CachePanel } from './components/CachePanel';
import { Fleet } from './components/Fleet';
import { Keys } from './components/Keys';
import { Recent } from './components/Recent';
import { RouterPanel } from './components/RouterPanel';
import { formatTime } from './format';
import { useOverview } from './useOverview';

function fleetNotice(data: Overview): string | undefined {
  if (data.fleet_error) {
    return `fleet view from this replica only: ${data.fleet_error}`;
  }
  if (!data.shared) {
    return 'fleet view from this replica only: shared state (Redis) is not configured';
  }
  return undefined;
}

export function App() {
  const { data, error } = useOverview();
  const notice = data ? fleetNotice(data) : undefined;

  return (
    <div className="page">
      <header className="header">
        <h1>Switchboard</h1>
        <dl className="meta">
          <div>
            <dt>Replica</dt>
            <dd>{data ? data.self : '–'}</dd>
          </div>
          <div>
            <dt>Updated</dt>
            <dd>{data ? formatTime(data.generated_at, false) : '–'}</dd>
          </div>
        </dl>
      </header>

      {error && (
        <div className="banner banner-error" role="alert">
          <strong>admin API unreachable</strong>
          <span>
            {data ? ' – showing the last data received.' : '.'} {error.message}
          </span>
        </div>
      )}
      {notice && (
        <div className="banner banner-warn" role="status">
          {notice}
        </div>
      )}

      {data ? (
        <main className={error ? 'content is-stale' : 'content'} aria-busy={error ? true : undefined}>
          <Fleet replicas={data.replicas ?? []} />
          <Keys keys={data.keys ?? []} />
          <div className="grid-2">
            <CachePanel cache={data.cache} />
            <RouterPanel router={data.router} />
          </div>
          <Recent calls={data.recent ?? []} traceUrl={data.trace_url} />
        </main>
      ) : (
        !error && (
          <p className="loading" role="status">
            Loading…
          </p>
        )
      )}
    </div>
  );
}
