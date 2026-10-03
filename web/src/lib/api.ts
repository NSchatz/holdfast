// The one place the UI talks to the holdfast JSON API. Every call returns a value
// that says which of three things happened, and never throws: a view decides what to
// show from the answer, not from a catch block that may never run.

/** The path of the surface document. It needs no credential in any configuration. */
export const SCHEMA_PATH = "/api/schema";

export type ApiResult<T> =
  | { kind: "ok"; value: T }
  /** The server answered, and the answer was not a success. */
  | { kind: "refused"; status: number; message: string; body?: unknown }
  /** No usable answer: the request failed, or the body was not the JSON asked for. */
  | { kind: "unreachable"; message: string };

export type Fetch = typeof globalThis.fetch;

/**
 * Whether a path is one this page may send a credential to: a path under `/api/` on the
 * origin the page was loaded from. A full URL, a protocol-relative one, a path that
 * climbs out, a query string and a fragment are all refused by shape; a query is passed
 * as `query` and built here.
 */
export function isApiPath(path: string): boolean {
  return /^\/api\/[A-Za-z0-9_\-./]*$/.test(path) && !path.includes("//") && !path.includes("..");
}

export interface RequestOptions {
  /** Sent as `Authorization: Bearer`, and nowhere else. */
  token?: string;
  fetch?: Fetch;
  signal?: AbortSignal;
  /** Query parameters. A repeated key is a list. Never a credential. */
  query?: [string, string][];
}

/**
 * One request to the API, answered as an `ApiResult` and never thrown. `token`, when
 * given, is sent as a bearer credential to a path under `/api/` and to nothing else; it
 * is never stored, logged or put in a URL by this function. A request for any other path
 * is not sent at all.
 */
export async function request(
  method: "GET" | "POST" | "DELETE",
  path: string,
  options: RequestOptions & { body?: unknown } = {},
): Promise<ApiResult<unknown>> {
  if (!isApiPath(path)) {
    return { kind: "unreachable", message: `not sent: ${path} is not a path under /api/ on this server` };
  }
  const doFetch = options.fetch ?? globalThis.fetch;
  const headers: Record<string, string> = { Accept: "application/json" };
  if (options.token !== undefined && options.token !== "") {
    headers["Authorization"] = `Bearer ${options.token}`;
  }
  const init: RequestInit = { headers, signal: options.signal, cache: "no-store" };
  if (method !== "GET") {
    init.method = method;
  }
  if (options.body !== undefined) {
    headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(options.body);
  }
  let url = path;
  if (options.query !== undefined && options.query.length > 0) {
    const params = new URLSearchParams();
    for (const [key, value] of options.query) {
      params.append(key, value);
    }
    url = `${path}?${params.toString()}`;
  }

  let response: Response;
  try {
    response = await doFetch(url, init);
  } catch (err) {
    return { kind: "unreachable", message: describe(err) };
  }

  if (!response.ok) {
    return refusalOf(response);
  }

  try {
    return { kind: "ok", value: (await response.json()) as unknown };
  } catch (err) {
    return { kind: "unreachable", message: `the answer was not JSON: ${describe(err)}` };
  }
}

/**
 * GET one JSON document. `token`, when given, is sent as a bearer credential; it is
 * never stored, logged or put in a URL by this function.
 */
export async function getJSON<T>(path: string, options: RequestOptions = {}): Promise<ApiResult<T>> {
  return (await request("GET", path, options)) as ApiResult<T>;
}

/** The surface document, as far as this UI reads it. */
export interface SurfaceDocument {
  schema: string;
  schema_version: string;
  holdfast_version: string;
  endpoints: { method: string; path: string }[];
}

/**
 * What the shell states about the server it was loaded from. A document that is JSON
 * but not the surface document is `unreachable`: a count read off some other shape
 * would be a confident wrong figure.
 */
export async function loadSurface(
  options: { fetch?: Fetch; signal?: AbortSignal } = {},
): Promise<ApiResult<SurfaceDocument>> {
  const result = await getJSON<unknown>(SCHEMA_PATH, options);
  if (result.kind !== "ok") {
    return result;
  }
  if (!isSurfaceDocument(result.value)) {
    return { kind: "unreachable", message: `${SCHEMA_PATH} did not answer with the surface document` };
  }
  return { kind: "ok", value: result.value };
}

function isSurfaceDocument(v: unknown): v is SurfaceDocument {
  if (typeof v !== "object" || v === null) {
    return false;
  }
  const d = v as Record<string, unknown>;
  return (
    d["schema"] === "holdfast.http-surface" &&
    typeof d["schema_version"] === "string" &&
    typeof d["holdfast_version"] === "string" &&
    Array.isArray(d["endpoints"]) &&
    d["endpoints"].every(
      (e: unknown) =>
        typeof e === "object" &&
        e !== null &&
        typeof (e as Record<string, unknown>)["method"] === "string" &&
        typeof (e as Record<string, unknown>)["path"] === "string",
    )
  );
}

async function refusalOf(response: Response): Promise<ApiResult<never>> {
  let text = "";
  try {
    text = (await response.text()).trim();
  } catch {
    // The status alone is the answer.
  }
  // A refusal that is a JSON object is kept whole for the caller that knows its shape,
  // and its `error` is the sentence where it carries one.
  if (text.startsWith("{")) {
    try {
      const body: unknown = JSON.parse(text);
      if (typeof body === "object" && body !== null) {
        const said = (body as Record<string, unknown>)["error"];
        const message = typeof said === "string" && said !== "" ? said : response.statusText;
        return { kind: "refused", status: response.status, message, body };
      }
    } catch {
      // Not JSON after all: it is read as text below.
    }
  }
  // One line at most: a refusal is a sentence, and anything longer is a page.
  const firstLine = text.split("\n", 1)[0] ?? "";
  const message = firstLine.length > 0 && firstLine.length <= 300 ? firstLine : response.statusText;
  return { kind: "refused", status: response.status, message };
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
