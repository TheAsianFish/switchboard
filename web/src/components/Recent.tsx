import { useId, useMemo, useState } from 'react';
import type { Call } from '../api';
import { formatCount, formatMS, formatTime, formatUSD, shortId, traceHref } from '../format';
import { Badge, outcomeTone } from './Badge';

const MAX_ROWS = 100;
const KNOWN_OUTCOMES = ['ok', 'cache_hit', 'rate_limited', 'budget_exhausted', 'error', 'abandoned'];

function routerTitle(c: Call): string | undefined {
  const probs = Object.entries(c.router_probs ?? {})
    .sort(([, a], [, b]) => b - a)
    .map(([model, p]) => `${model}: ${p.toFixed(3)}`);
  const parts = [c.router_reason ? `router: ${c.router_reason}` : '', ...probs];
  const text = parts.filter(Boolean).join('\n');
  return text || undefined;
}

function CallRow({ c, traceUrl }: { c: Call; traceUrl: string | undefined }) {
  const href = traceHref(traceUrl, c.trace_id);
  const modelText = c.requested_model !== c.model ? `${c.requested_model} → ${c.model}` : c.model;
  return (
    <tr>
      <td className="num nowrap">{formatTime(c.time)}</td>
      <td>{c.replica}</td>
      <td>{c.key}</td>
      <td title={routerTitle(c)}>
        {modelText}
        {c.router_reason && <div className="sub">{c.router_reason}</div>}
      </td>
      <td>
        {c.stream ? (
          <span title="streamed" aria-label="streamed">
            ⇢
          </span>
        ) : (
          <span className="muted" title="not streamed" aria-label="not streamed">
            –
          </span>
        )}
      </td>
      <td>
        <Badge tone={outcomeTone(c.outcome)}>{c.outcome}</Badge>
        {c.error_kind && <div className="sub">{c.error_kind}</div>}
      </td>
      <td>{c.cache ?? '–'}</td>
      <td className="num">{formatMS(c.duration_ms)}</td>
      <td className="num">{formatMS(c.ttft_ms)}</td>
      <td className="num">
        {formatCount(c.input_tokens)} / {formatCount(c.output_tokens)}
      </td>
      <td className="num">{formatUSD(c.cost_usd)}</td>
      <td>
        {c.hedged && <Badge tone="info">hedged</Badge>}
        {c.attempts !== undefined && c.attempts > 1 && <Badge tone="warn">{`${String(c.attempts)} attempts`}</Badge>}
        {!c.hedged && !(c.attempts !== undefined && c.attempts > 1) && <span className="muted">–</span>}
      </td>
      <td className="nowrap">
        {c.trace_id ? (
          href ? (
            <a href={href} target="_blank" rel="noreferrer" title={c.trace_id}>
              {shortId(c.trace_id)}
            </a>
          ) : (
            <span title={c.trace_id}>{shortId(c.trace_id)}</span>
          )
        ) : (
          <span className="muted">–</span>
        )}
      </td>
    </tr>
  );
}

export function Recent({ calls, traceUrl }: { calls: Call[]; traceUrl: string | undefined }) {
  const [text, setText] = useState('');
  const [outcome, setOutcome] = useState('');
  const textId = useId();
  const outcomeId = useId();

  const outcomes = useMemo(() => {
    const seen = new Set(KNOWN_OUTCOMES);
    for (const c of calls) seen.add(c.outcome);
    return [...seen];
  }, [calls]);

  const q = text.trim().toLowerCase();
  const filtered = calls.filter(
    (c) =>
      (outcome === '' || c.outcome === outcome) &&
      (q === '' ||
        c.key.toLowerCase().includes(q) ||
        c.model.toLowerCase().includes(q) ||
        c.requested_model.toLowerCase().includes(q)),
  );
  const shown = filtered.slice(0, MAX_ROWS);

  return (
    <section aria-labelledby="recent-h">
      <h2 id="recent-h">Recent requests</h2>
      <div className="filters">
        <label htmlFor={textId}>Key or model</label>
        <input
          id={textId}
          type="search"
          value={text}
          onChange={(e) => {
            setText(e.target.value);
          }}
          placeholder="filter"
        />
        <label htmlFor={outcomeId}>Outcome</label>
        <select
          id={outcomeId}
          value={outcome}
          onChange={(e) => {
            setOutcome(e.target.value);
          }}
        >
          <option value="">all</option>
          {outcomes.map((o) => (
            <option key={o} value={o}>
              {o}
            </option>
          ))}
        </select>
        <span className="muted" role="status">
          Showing {shown.length} of {calls.length}
        </span>
      </div>
      <div className="table-wrap">
        <table>
          <caption className="sr-only">Recent requests, newest first</caption>
          <thead>
            <tr>
              <th scope="col">Time</th>
              <th scope="col">Replica</th>
              <th scope="col">Key</th>
              <th scope="col">Model</th>
              <th scope="col">Stream</th>
              <th scope="col">Outcome</th>
              <th scope="col">Cache</th>
              <th scope="col" className="num">
                Duration
              </th>
              <th scope="col" className="num">
                TTFT
              </th>
              <th scope="col" className="num">
                Tokens in / out
              </th>
              <th scope="col" className="num">
                Cost
              </th>
              <th scope="col">Retries</th>
              <th scope="col">Trace</th>
            </tr>
          </thead>
          <tbody>
            {shown.length === 0 ? (
              <tr>
                <td colSpan={13} className="muted">
                  No matching requests.
                </td>
              </tr>
            ) : (
              shown.map((c, i) => <CallRow key={`${c.replica}/${c.request_id}/${String(i)}`} c={c} traceUrl={traceUrl} />)
            )}
          </tbody>
        </table>
      </div>
    </section>
  );
}
