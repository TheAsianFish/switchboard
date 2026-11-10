import { useEffect, useState } from 'react';
import { fetchOverview, isAbortError, type Overview } from './api';

export interface OverviewState {
  data: Overview | undefined;
  /** Set while the latest fetch failed; data then holds the last good one. */
  error: Error | undefined;
}

export const POLL_INTERVAL_MS = 2000;

/**
 * Polls the overview. The next request starts one interval after the
 * previous one finished, polling pauses while the tab is hidden, and the
 * in-flight request is aborted on unmount.
 */
export function useOverview(intervalMs: number = POLL_INTERVAL_MS): OverviewState {
  const [state, setState] = useState<OverviewState>({ data: undefined, error: undefined });

  useEffect(() => {
    // Mutable loop state, shared by the callbacks below.
    const run: {
      stopped: boolean;
      inflight: boolean;
      timer: ReturnType<typeof setTimeout> | undefined;
      controller: AbortController | undefined;
    } = { stopped: false, inflight: false, timer: undefined, controller: undefined };

    // Reads through functions so control-flow narrowing does not assume the
    // values are unchanged across an await.
    const stopped = (): boolean => run.stopped;
    const hidden = (): boolean => document.hidden;

    const tick = async (): Promise<void> => {
      run.timer = undefined;
      if (stopped() || run.inflight || hidden()) return;
      run.inflight = true;
      run.controller = new AbortController();
      try {
        const data = await fetchOverview(run.controller.signal);
        if (!stopped()) setState({ data, error: undefined });
      } catch (err) {
        if (stopped() || isAbortError(err)) return;
        const error = err instanceof Error ? err : new Error(String(err));
        setState((prev) => ({ data: prev.data, error }));
      } finally {
        run.inflight = false;
      }
      if (!stopped() && !hidden()) {
        run.timer = setTimeout(() => void tick(), intervalMs);
      }
    };

    const onVisibility = (): void => {
      if (hidden()) return;
      if (run.timer !== undefined) clearTimeout(run.timer);
      void tick();
    };

    document.addEventListener('visibilitychange', onVisibility);
    void tick();

    return () => {
      run.stopped = true;
      document.removeEventListener('visibilitychange', onVisibility);
      if (run.timer !== undefined) clearTimeout(run.timer);
      run.controller?.abort();
    };
  }, [intervalMs]);

  return state;
}
