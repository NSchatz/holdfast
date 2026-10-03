// The one place the UI talks to the holdfast JSON API. Every call returns a value
// that says which of three things happened, and never throws: a view decides what to
// show from the answer, not from a catch block that may never run.

/** The path of the surface document. It needs no credential in any configuration. */
export const SCHEMA_PATH = "/api/schema";

export type ApiResult<T> =
  | { kind: "ok"; value: T }
  /** The server answered, and the answer was not a success. */
  | { kind: "refused"; status: number; message: string }
  /** No usable answer: the request failed, or the body was not the JSON asked for. */
  | { kind: "unreachable"; message: string };

type Fetch = typeof globalThis.fetch;

/**
 * GET one JSON document. `token`, when given, is sent as a bearer credential; it is
 * never stored, logged or put in a URL by this function.
 */
export async function getJSON<T>(
  path: string,
  options: { token?: string; fetch?: Fetch; signal?: AbortSignal } = {},
): Promise<ApiResult<T>> {
  const doFetch = options.fetch ?? globalThis.fetch;
  const headers: Record<string, string> = { Accept: "application/json" };
  if (options.token !== undefined && options.token !== "") {
    headers["Authorization"] = `Bearer ${options.token}`;
  }

  let response: Response;
  try {
    response = await doFetch(path, { headers, signal: options.signal, cache: "no-store" });
  } catch (err) {
    return { kind: "unreachable", message: describe(err) };
  }

  if (!response.ok) {
    return { kind: "refused", status: response.status, message: await refusalOf(response) };
  }

  try {
    return { kind: "ok", value: (await response.json()) as T };
  } catch (err) {
    return { kind: "unreachable", message: `the answer was not JSON: ${describe(err)}` };
  }
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

async function refusalOf(response: Response): Promise<string> {
  let text = "";
  try {
    text = (await response.text()).trim();
  } catch {
    // The status alone is the answer.
  }
  // One line at most: a refusal is a sentence, and anything longer is a page.
  const firstLine = text.split("\n", 1)[0] ?? "";
  return firstLine.length > 0 && firstLine.length <= 300 ? firstLine : response.statusText;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
