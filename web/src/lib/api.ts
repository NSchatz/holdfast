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
  const token = options.token ?? "";
  // Before anything is sent: a token a header cannot carry makes `fetch` throw, and what
  // an engine writes in that error is not this page's to choose.
  if (token !== "" && !TOKEN_SHAPE.test(token)) {
    return { kind: "unreachable", message: TOKEN_NOT_SENDABLE };
  }
  return withheld(await send(method, path, token, options), token);
}

/** What a bearer token may be made of: printable ASCII, no space. */
const TOKEN_SHAPE = /^[\x21-\x7e]+$/;

export const TOKEN_NOT_SENDABLE =
  "not sent: the token held is not one a request can carry - a token is printable ASCII with no space. Forget it and enter it again.";

export const MESSAGE_WITHHELD =
  "the reason is not shown: it repeated the token, and the token is written nowhere on this page.";

/**
 * The same answer with nothing in it that repeats the token: a message that carries it is
 * replaced by a fixed sentence, and a refusal's body that carries it is dropped.
 */
function withheld(result: ApiResult<unknown>, token: string): ApiResult<unknown> {
  if (token === "" || result.kind === "ok") {
    return result;
  }
  const message = result.message.includes(token) ? MESSAGE_WITHHELD : result.message;
  if (result.kind === "unreachable") {
    return { kind: "unreachable", message };
  }
  if (result.body !== undefined && JSON.stringify(result.body).includes(token)) {
    return { kind: "refused", status: result.status, message };
  }
  return { ...result, message };
}

async function send(
  method: "GET" | "POST" | "DELETE",
  path: string,
  token: string,
  options: RequestOptions & { body?: unknown },
): Promise<ApiResult<unknown>> {
  const doFetch = options.fetch ?? globalThis.fetch;
  const headers: Record<string, string> = { Accept: "application/json" };
  if (token !== "") {
    headers["Authorization"] = `Bearer ${token}`;
  }
  // Same origin only, no cookie, and no redirect followed: a redirect would have the
  // browser send these headers again to wherever it pointed, which need not be under
  // /api/. Refusing it makes `fetch` fail, and that is reported as no answer.
  const init: RequestInit = {
    headers,
    signal: options.signal,
    cache: "no-store",
    redirect: "error",
    mode: "same-origin",
    credentials: "omit",
  };
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
    return { kind: "unreachable", message: `${describe(err)} (${NO_ANSWER})` };
  }

  // An engine that hands back a redirect instead of failing is held to the same rule.
  if (response.redirected || response.type === "opaqueredirect" || (response.status >= 300 && response.status < 400)) {
    return { kind: "unreachable", message: REDIRECT_NOT_FOLLOWED };
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

/** What follows the engine's own words where a request failed before any answer was read. */
export const NO_ANSWER =
  "no answer was read: the server could not be reached, or it answered with a redirect, which this page does not follow";

export const REDIRECT_NOT_FOLLOWED =
  "the server answered with a redirect, which this page does not follow: a request goes to a path under /api/ on this server and nowhere else.";

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
        const message = typeof said === "string" && said !== "" ? sentence(said) : response.statusText;
        return { kind: "refused", status: response.status, message, body };
      }
    } catch {
      // Not JSON after all: it is read as text below.
    }
  }
  // One line at most, and the status text only where the body said nothing.
  const firstLine = text.split("\n", 1)[0] ?? "";
  const message = firstLine.length > 0 ? sentence(firstLine) : response.statusText;
  return { kind: "refused", status: response.status, message };
}

/** The longest refusal sentence shown whole. */
export const SENTENCE_LIMIT = 300;

/**
 * A refusal's sentence, cut where it runs long. It is cut and not dropped: the status text
 * that would stand in for it is empty under HTTP/2, and a long refusal still says more in
 * its first part than nothing does.
 */
function sentence(said: string): string {
  return said.length > SENTENCE_LIMIT ? `${said.slice(0, SENTENCE_LIMIT)}...` : said;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
