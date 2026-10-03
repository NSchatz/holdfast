// One endpoint read on an interval, as reactive state a view renders from: the last
// answer, when it was read, and a way to read again. Leaving the view stops the reads and
// aborts the one in flight.

import { untrack } from "svelte";

import type { ApiResult } from "./api";
import { POLL_INTERVAL_MS, startPolling, type Poller, type VisibilitySource } from "./poll";

export interface Resource<T> {
  /** The last answer, or undefined before the first. */
  readonly result: ApiResult<T> | undefined;
  /** When that answer was read, by this browser's clock, in unix seconds. */
  readonly readAt: number | null;
  /** Read now. */
  refresh(): void;
  /** Start reading; the returned function stops it. */
  start(): () => void;
  /** Forget the last answer, so the view says it is asking again. */
  clear(): void;
}

export interface ResourceOptions {
  /** 0 reads once and then only on `refresh`. */
  intervalMs?: () => number | undefined;
  doc?: VisibilitySource;
  clock?: () => number;
}

export function createResource<T>(
  load: (signal: AbortSignal) => Promise<ApiResult<T>>,
  options: ResourceOptions = {},
): Resource<T> {
  let result = $state.raw<ApiResult<T> | undefined>(undefined);
  let readAt = $state<number | null>(null);
  let poller: Poller | undefined;
  const clock = options.clock ?? (() => Date.now());

  const read = async (signal: AbortSignal) => {
    const answer = await load(signal);
    // An abandoned read's answer is about a request nobody is waiting for.
    if (signal.aborted) {
      return;
    }
    result = answer;
    readAt = Math.floor(clock() / 1000);
  };

  return {
    get result() {
      return result;
    },
    get readAt() {
      return readAt;
    },
    refresh() {
      poller?.refresh();
    },
    clear() {
      result = undefined;
      readAt = null;
    },
    start() {
      // Untracked: the first read runs inside whatever effect called this, and what the
      // loader reads there must not make that effect run again.
      const mine = untrack(() =>
        startPolling(read, options.intervalMs?.() ?? POLL_INTERVAL_MS, options.doc ?? document),
      );
      poller = mine;
      return () => {
        mine.stop();
        if (poller === mine) {
          poller = undefined;
        }
      };
    },
  };
}
