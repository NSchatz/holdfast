// Reading a JSON value this page did not write. Every reader here answers `null` for a
// field that is absent, null or of another type, and never a default: a figure that could
// not be read is shown as unavailable, and a zero in its place would be a figure nobody
// measured (docs/design/ledger-totals.md#null-is-not-zero).

export type Dict = Record<string, unknown>;

export function isDict(v: unknown): v is Dict {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** A finite number, or null. */
export function num(v: unknown): number | null {
  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

/** A string, or null. An empty string is a value, and stays one. */
export function str(v: unknown): string | null {
  return typeof v === "string" ? v : null;
}

/** A string with something in it, or null. */
export function text(v: unknown): string | null {
  return typeof v === "string" && v !== "" ? v : null;
}

/** A boolean, or null: neither `true` nor `false` is assumed. */
export function bool(v: unknown): boolean | null {
  return typeof v === "boolean" ? v : null;
}

/** The dictionaries of a list, or null when the value is not a list at all. */
export function dicts(v: unknown): Dict[] | null {
  return Array.isArray(v) ? v.filter(isDict) : null;
}

/** The strings of a list, or null when the value is not a list at all. */
export function strings(v: unknown): string[] | null {
  return Array.isArray(v) ? v.filter((e): e is string => typeof e === "string") : null;
}
