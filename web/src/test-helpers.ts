// A faked `fetch` for the tests: no browser, no network. It records every request it was
// asked for, so a test can hold the page to what it sends and where.

import type { Fetch } from "./lib/api";

export interface Call {
  method: string;
  /** The URL exactly as the page passed it. */
  url: string;
  /** The path of that URL, without its query. */
  path: string;
  query: URLSearchParams;
  headers: Record<string, string>;
  body: string | null;
  signal: AbortSignal | undefined;
}

export type Route = (call: Call) => Response | Promise<Response>;

export function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

export function plain(body: string, status: number): Response {
  return new Response(body + "\n", { status, headers: { "Content-Type": "text/plain; charset=utf-8" } });
}

export function fakeFetch(route: Route): { fetch: Fetch; calls: Call[] } {
  const calls: Call[] = [];
  const fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const [path = "", query = ""] = url.split("?", 2);
    const call: Call = {
      method: init?.method ?? "GET",
      url,
      path,
      query: new URLSearchParams(query),
      headers: { ...(init?.headers as Record<string, string> | undefined) },
      body: typeof init?.body === "string" ? init.body : null,
      signal: init?.signal ?? undefined,
    };
    calls.push(call);
    if (init?.signal?.aborted === true) {
      return Promise.reject(new DOMException("aborted", "AbortError"));
    }
    return Promise.resolve(route(call));
  }) as Fetch;
  return { fetch, calls };
}

/** A synthetic job row with every field the API reference names for the views. */
export function jobRow(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    path: "/library/films/example-a.mkv",
    status: "pending",
    fail_count: 0,
    updated_at: 1790000000,
    profile: "",
    priority: null,
    vmaf_mean: null,
    vmaf_min: null,
    source_codec: null,
    source_bytes: null,
    output_bytes: null,
    encode_ms: null,
    source_width: null,
    source_height: null,
    output_width: null,
    output_height: null,
    progress_seconds: null,
    progress_duration_seconds: null,
    progress_fraction: null,
    library_root: null,
    profile_digest: null,
    ...over,
  };
}

/**
 * A total as the server writes one. `age` is how old the count is, in seconds; a total
 * that could not be read carries a null age beside its null count, as the server's does.
 */
export function total(count: number | null, cap: number, unavailable = "", age = 0): Record<string, unknown> {
  return {
    available: count !== null,
    unavailable,
    covers: "every matching row in the ledger",
    cap,
    age_seconds: count === null ? null : age,
    count,
  };
}

/** A promise and the function that settles it, for an answer a test delivers late. */
export function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve: (value: T) => void = () => {};
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

/** An element's text with its whitespace collapsed, as a reader sees it. */
export function textOf(element: Element | null): string {
  return (element?.textContent ?? "").replace(/\s+/g, " ").trim();
}

/** The data cells of the table row whose row header reads `header`, as text. */
export function cellsOf(root: HTMLElement, header: string): string[] {
  const row = Array.from(root.querySelectorAll("tr")).find((tr) => textOf(tr.querySelector("th[scope=row]")) === header);
  if (row === undefined) {
    throw new Error(`no table row is headed ${JSON.stringify(header)}`);
  }
  return Array.from(row.querySelectorAll("td")).map(textOf);
}

/** The column headers of a table, as text. */
export function columnsOf(table: Element): string[] {
  return Array.from(table.querySelectorAll("thead th")).map(textOf);
}
