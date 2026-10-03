// Reading on an interval while the page is visible. The event stream is not used: an
// EventSource cannot carry an Authorization header, and the token goes nowhere else
// (docs/design/web-ui.md#views).

/** How often a view asks again, in milliseconds. */
export const POLL_INTERVAL_MS = 5000;

/** The part of `document` the poller reads; a test hands it a fake. */
export interface VisibilitySource {
  visibilityState: string;
  addEventListener(type: "visibilitychange", listener: () => void): void;
  removeEventListener(type: "visibilitychange", listener: () => void): void;
}

export interface Poller {
  /** Read now, abandoning a read still in flight. */
  refresh(): void;
  /** Stop for good: no further read starts, and the one in flight is aborted. */
  stop(): void;
}

/**
 * Calls `read` at once and then every `intervalMs` while the page is visible, with a
 * signal that is aborted when the read is superseded or the poller is stopped. A page
 * that becomes visible again reads at once. One read runs at a time: a tick that lands
 * while one is in flight is skipped, and `refresh` replaces it.
 */
export function startPolling(
  read: (signal: AbortSignal) => Promise<void>,
  intervalMs: number = POLL_INTERVAL_MS,
  doc: VisibilitySource = document,
): Poller {
  let stopped = false;
  let inFlight: AbortController | undefined;

  const run = (replace: boolean) => {
    if (stopped) {
      return;
    }
    if (inFlight !== undefined) {
      if (!replace) {
        return;
      }
      inFlight.abort();
    }
    const controller = new AbortController();
    inFlight = controller;
    const done = () => {
      if (inFlight === controller) {
        inFlight = undefined;
      }
    };
    read(controller.signal).then(done, done);
  };

  const tick = () => {
    if (doc.visibilityState === "visible") {
      run(false);
    }
  };
  const onVisibility = () => {
    if (doc.visibilityState === "visible") {
      run(false);
    }
  };

  const timer = intervalMs > 0 ? setInterval(tick, intervalMs) : undefined;
  doc.addEventListener("visibilitychange", onVisibility);
  run(true);

  return {
    refresh: () => {
      run(true);
    },
    stop: () => {
      stopped = true;
      if (timer !== undefined) {
        clearInterval(timer);
      }
      doc.removeEventListener("visibilitychange", onVisibility);
      inFlight?.abort();
      inFlight = undefined;
    },
  };
}
