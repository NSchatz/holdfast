// What each endpoint answers, as far as this UI reads it, and the one reader per
// endpoint. A reader keeps two rules:
//
//  - a body that is not the endpoint's shape is `unreachable`, never a partly filled view:
//    a table drawn from some other document would be a confident wrong answer;
//  - inside the right shape, a field that is absent, null or of another type is `null`,
//    never a default. `null` is shown as unavailable or not recorded, never as 0.
//
// The wire names are docs/api-reference.md's; nothing here renames a status or a token.

import { request, type ApiResult, type RequestOptions } from "./api";
import { bool, dicts, isDict, num, str, strings, text, type Dict } from "./shape";

type Options = Pick<RequestOptions, "token" | "fetch" | "signal">;

function misshapen(path: string, what: string): ApiResult<never> {
  return { kind: "unreachable", message: `${path} did not answer with ${what}` };
}

async function read<T>(
  path: string,
  what: string,
  parse: (body: unknown) => T | null,
  options: RequestOptions,
): Promise<ApiResult<T>> {
  const result = await request("GET", path, options);
  if (result.kind !== "ok") {
    return result;
  }
  const value = parse(result.value);
  return value === null ? misshapen(path, what) : { kind: "ok", value };
}

// --- the total behind a cap ----------------------------------------------------------

/** `queue_total`, `history_total`, `leases_total`: the count a capped list was cut from. */
export interface RowTotal {
  available: boolean;
  /** Matching rows in the ledger. null when it could not be read: never a zero. */
  count: number | null;
  cap: number | null;
  covers: string | null;
  unavailable: string | null;
  /**
   * How old the count was when the server answered, in seconds: it serves the count from
   * a cache, and the rows beside it are read fresh. null where it did not say.
   */
  ageSeconds: number | null;
}

function rowTotal(v: unknown): RowTotal | null {
  if (!isDict(v)) {
    return null;
  }
  const available = bool(v["available"]) === true;
  return {
    available,
    // A count beside `available: false` is not a count.
    count: available ? num(v["count"]) : null,
    cap: num(v["cap"]),
    covers: text(v["covers"]),
    unavailable: text(v["unavailable"]),
    ageSeconds: num(v["age_seconds"]),
  };
}

// --- job rows ------------------------------------------------------------------------

/** One job row, of the queue or of the history. */
export interface Job {
  path: string;
  status: string;
  worker: string | null;
  failCount: number | null;
  updatedAt: number | null;
  reason: string | null;
  encoder: string | null;
  profile: string | null;
  priority: number | null;
  sourceBytes: number | null;
  outputBytes: number | null;
  encodeMs: number | null;
  sourceCodec: string | null;
  sourceWidth: number | null;
  sourceHeight: number | null;
  outputWidth: number | null;
  outputHeight: number | null;
  libraryRoot: string | null;
  vmafMean: number | null;
  vmafMin: number | null;
  progressSeconds: number | null;
  progressDurationSeconds: number | null;
  progressFraction: number | null;
}

function job(v: Dict): Job | null {
  const path = str(v["path"]);
  const status = text(v["status"]);
  if (path === null || status === null) {
    return null;
  }
  return {
    path,
    status,
    worker: text(v["worker"]),
    failCount: num(v["fail_count"]),
    updatedAt: num(v["updated_at"]),
    reason: text(v["reason"]),
    encoder: text(v["encoder"]),
    profile: str(v["profile"]),
    priority: num(v["priority"]),
    sourceBytes: num(v["source_bytes"]),
    outputBytes: num(v["output_bytes"]),
    encodeMs: num(v["encode_ms"]),
    sourceCodec: text(v["source_codec"]),
    sourceWidth: num(v["source_width"]),
    sourceHeight: num(v["source_height"]),
    outputWidth: num(v["output_width"]),
    outputHeight: num(v["output_height"]),
    libraryRoot: text(v["library_root"]),
    vmafMean: num(v["vmaf_mean"]),
    vmafMin: num(v["vmaf_min"]),
    progressSeconds: num(v["progress_seconds"]),
    progressDurationSeconds: num(v["progress_duration_seconds"]),
    progressFraction: num(v["progress_fraction"]),
  };
}

/** Every row of a list, or null when the list or any row of it is not a job row. */
function jobs(v: unknown): Job[] | null {
  if (!Array.isArray(v)) {
    return null;
  }
  const out: Job[] = [];
  for (const row of v) {
    const parsed = isDict(row) ? job(row) : null;
    if (parsed === null) {
      return null;
    }
    out.push(parsed);
  }
  return out;
}

// --- GET /api/summary ----------------------------------------------------------------

export const SUMMARY_PATH = "/api/summary";

/** The statuses in the order a job passes through them; any other the server names follows. */
export const STATUS_ORDER = [
  "pending",
  "probing",
  "encoding",
  "verifying",
  "done",
  "skipped",
  "failed",
  "would-transcode",
  "indeterminate",
  "applied-despite-error",
] as const;

export interface RootFigures {
  /** The configured root, or null on the row for files under no configured root. */
  root: string | null;
  candidateFiles: number | null;
  candidateExcluded: number | null;
  candidateBytes: number | null;
  projectionBasisFiles: number | null;
  projectedSavingsBytes: number | null;
  heldByUndoWindow: number | null;
  freeBytes: number | null;
}

export interface Spread {
  available: boolean;
  unavailable: string | null;
  covers: string | null;
  window: string | null;
  /** How old the figure was when the server answered, in seconds. null where it did not say. */
  ageSeconds: number | null;
  counted: number | null;
  excluded: number | null;
  min: number | null;
  mean: number | null;
  max: number | null;
}

export interface Breakdown {
  available: boolean;
  unavailable: string | null;
  covers: string | null;
  window: string | null;
  /** How old the figure was when the server answered, in seconds. null where it did not say. */
  ageSeconds: number | null;
  counted: number | null;
  excluded: number | null;
  buckets: { key: string; count: number | null }[];
}

export interface Aggregates {
  outcomes: Breakdown | null;
  skipsByGuard: Breakdown | null;
  sizeRatio: Spread | null;
  encodeMs: Spread | null;
  vmafMean: Spread | null;
  vmafMin: Spread | null;
}

export interface Summary {
  counts: { status: string; count: number | null }[];
  paused: boolean | null;
  scanning: boolean | null;
  reclaimedLifetime: number | null;
  reclaimedSession: number | null;
  heldByUndoWindow: number | null;
  /** null where the server answered without the per-root table. */
  roots: RootFigures[] | null;
  unattributed: RootFigures | null;
  aggregates: Aggregates | null;
}

function rootFigures(v: Dict, named: boolean): RootFigures {
  return {
    root: named ? str(v["root"]) : null,
    candidateFiles: num(v["candidate_files"]),
    candidateExcluded: num(v["candidate_excluded"]),
    candidateBytes: num(v["candidate_bytes"]),
    projectionBasisFiles: num(v["projection_basis_files"]),
    projectedSavingsBytes: num(v["projected_savings_bytes"]),
    heldByUndoWindow: num(v["bytes_held_by_undo_window"]),
    freeBytes: num(v["free_bytes"]),
  };
}

function spread(v: unknown): Spread | null {
  if (!isDict(v)) {
    return null;
  }
  const available = bool(v["available"]) === true;
  return {
    available,
    unavailable: text(v["unavailable"]),
    covers: text(v["covers"]),
    window: text(v["window"]),
    ageSeconds: num(v["age_seconds"]),
    counted: num(v["counted"]),
    excluded: num(v["excluded"]),
    min: available ? num(v["min"]) : null,
    mean: available ? num(v["mean"]) : null,
    max: available ? num(v["max"]) : null,
  };
}

function breakdown(v: unknown): Breakdown | null {
  if (!isDict(v)) {
    return null;
  }
  return {
    available: bool(v["available"]) === true,
    unavailable: text(v["unavailable"]),
    covers: text(v["covers"]),
    window: text(v["window"]),
    ageSeconds: num(v["age_seconds"]),
    counted: num(v["counted"]),
    excluded: num(v["excluded"]),
    buckets: (dicts(v["buckets"]) ?? []).flatMap((b) => {
      const key = str(b["key"]);
      return key === null ? [] : [{ key, count: num(b["count"]) }];
    }),
  };
}

function aggregates(v: unknown): Aggregates | null {
  if (!isDict(v)) {
    return null;
  }
  return {
    outcomes: breakdown(v["outcomes"]),
    skipsByGuard: breakdown(v["skips_by_guard"]),
    sizeRatio: spread(v["size_ratio"]),
    encodeMs: spread(v["encode_ms"]),
    vmafMean: spread(v["vmaf_mean"]),
    vmafMin: spread(v["vmaf_min"]),
  };
}

export function parseSummary(body: unknown): Summary | null {
  if (!isDict(body) || !isDict(body["summary"])) {
    return null;
  }
  const map = body["summary"];
  const known: readonly string[] = STATUS_ORDER;
  const names = [
    ...STATUS_ORDER.filter((s) => s in map),
    ...Object.keys(map)
      .filter((s) => !known.includes(s))
      .sort(),
  ];
  const rootRows = dicts(body["roots"]);
  const unattributed = body["roots_unattributed"];
  return {
    counts: names.map((status) => ({ status, count: num(map[status]) })),
    paused: bool(body["paused"]),
    scanning: bool(body["scanning"]),
    reclaimedLifetime: num(body["bytes_reclaimed_lifetime"]),
    reclaimedSession: num(body["bytes_reclaimed_session"]),
    heldByUndoWindow: num(body["bytes_held_by_undo_window"]),
    roots: rootRows === null ? null : rootRows.map((r) => rootFigures(r, true)),
    unattributed: isDict(unattributed) ? rootFigures(unattributed, false) : null,
    aggregates: aggregates(body["aggregates"]),
  };
}

export function loadSummary(options: Options = {}): Promise<ApiResult<Summary>> {
  return read(SUMMARY_PATH, "a summary", parseSummary, options);
}

// --- GET /api/queue ------------------------------------------------------------------

export const QUEUE_PATH = "/api/queue";

export interface Queue {
  /** The server's clock when it answered, unix seconds. */
  now: number | null;
  rows: Job[];
  total: RowTotal | null;
}

export function parseQueue(body: unknown): Queue | null {
  if (!isDict(body)) {
    return null;
  }
  const rows = jobs(body["queue"]);
  if (rows === null) {
    return null;
  }
  return { now: num(body["now"]), rows, total: rowTotal(body["queue_total"]) };
}

export function loadQueue(options: Options = {}): Promise<ApiResult<Queue>> {
  return read(QUEUE_PATH, "a queue", parseQueue, options);
}

// --- GET /api/history ----------------------------------------------------------------

export const HISTORY_PATH = "/api/history";

/** The six terminal statuses, which are the whole vocabulary of the `status` filter. */
export const TERMINAL_STATUSES = [
  "done",
  "skipped",
  "failed",
  "would-transcode",
  "indeterminate",
  "applied-despite-error",
] as const;

export interface History {
  rows: Job[];
  total: RowTotal | null;
  /** The cursor of the next page, or null on the last one. */
  nextCursor: string | null;
  /**
   * Whether the answer carried `next_cursor` at all. The API always sends it, a string
   * or null; a server that predates paging answers without it, and ignores a filter: the
   * view says so.
   */
  pages: boolean;
}

export function parseHistory(body: unknown): History | null {
  if (!isDict(body)) {
    return null;
  }
  const rows = jobs(body["history"]);
  if (rows === null) {
    return null;
  }
  return {
    rows,
    total: rowTotal(body["history_total"]),
    nextCursor: text(body["next_cursor"]),
    pages: "next_cursor" in body,
  };
}

export interface HistoryQuery {
  limit: number;
  /** Terminal statuses to keep; none means all of them. */
  statuses: readonly string[];
  /** The cursor a previous answer gave, or null for the first page. */
  cursor: string | null;
}

export function loadHistory(query: HistoryQuery, options: Options = {}): Promise<ApiResult<History>> {
  const params: [string, string][] = [["limit", String(query.limit)]];
  for (const status of query.statuses) {
    params.push(["status", status]);
  }
  if (query.cursor !== null) {
    params.push(["cursor", query.cursor]);
  }
  return read(HISTORY_PATH, "a history", parseHistory, { ...options, query: params });
}

/** The 400 envelope: the server's sentence, and one per parameter it refused. */
export interface Refusal {
  error: string | null;
  parameters: { parameter: string | null; error: string | null }[];
}

export function parseRefusal(body: unknown): Refusal | null {
  if (!isDict(body)) {
    return null;
  }
  return {
    error: text(body["error"]),
    parameters: (dicts(body["parameters"]) ?? []).map((p) => ({
      parameter: text(p["parameter"]),
      error: text(p["error"]),
    })),
  };
}

// --- GET /api/health -----------------------------------------------------------------

export const HEALTH_PATH = "/api/health";

export interface HealthProblem {
  path: string | null;
  result: string | null;
  reason: string | null;
  checkedAt: number | null;
  size: number | null;
}

export interface Sweep {
  id: number | null;
  startedAt: number | null;
  finishedAt: number | null;
  checked: number | null;
  ok: number | null;
  corrupt: number | null;
  unreadable: number | null;
  problems: HealthProblem[];
  /** null where the answer did not say whether the list was cut. */
  problemsTruncated: boolean | null;
}

export interface Health {
  enabled: boolean | null;
  intervalHours: number | null;
  state: string;
  waiting: string | null;
  nextDueAt: number | null;
  current: Sweep | null;
  lastCompleted: Sweep | null;
}

function sweep(v: unknown): Sweep | null {
  if (!isDict(v)) {
    return null;
  }
  return {
    id: num(v["id"]),
    startedAt: num(v["started_at"]),
    finishedAt: num(v["finished_at"]),
    checked: num(v["checked"]),
    ok: num(v["ok"]),
    corrupt: num(v["corrupt"]),
    unreadable: num(v["unreadable"]),
    problems: (dicts(v["problems"]) ?? []).map((p) => ({
      path: str(p["path"]),
      result: text(p["result"]),
      reason: text(p["reason"]),
      checkedAt: num(p["checked_at"]),
      size: num(p["size"]),
    })),
    problemsTruncated: bool(v["problems_truncated"]),
  };
}

export function parseHealth(body: unknown): Health | null {
  if (!isDict(body)) {
    return null;
  }
  const state = text(body["state"]);
  if (state === null) {
    return null;
  }
  return {
    enabled: bool(body["enabled"]),
    intervalHours: num(body["interval_hours"]),
    state,
    waiting: text(body["waiting"]),
    nextDueAt: num(body["next_due_at"]),
    current: sweep(body["current"]),
    lastCompleted: sweep(body["last_completed"]),
  };
}

export function loadHealth(options: Options = {}): Promise<ApiResult<Health>> {
  return read(HEALTH_PATH, "a health report", parseHealth, options);
}

// --- GET /api/nodes ------------------------------------------------------------------

export const NODES_PATH = "/api/nodes";

export interface WorkerNode {
  node: string | null;
  mode: string | null;
  encoders: string[] | null;
  waiting: boolean | null;
  coolingUntil: number | null;
  leasesActive: number | null;
}

export interface Lease {
  node: string | null;
  path: string | null;
  state: string | null;
  epoch: number | null;
  grantedAt: number | null;
  updatedAt: number | null;
  expiresAt: number | null;
  endedAt: number | null;
  reason: string | null;
  sourceBytes: number | null;
  outputBytes: number | null;
}

export interface Nodes {
  enabled: boolean | null;
  now: number | null;
  nodes: WorkerNode[];
  leases: Lease[];
  total: RowTotal | null;
}

export function parseNodes(body: unknown): Nodes | null {
  if (!isDict(body)) {
    return null;
  }
  const nodeRows = dicts(body["nodes"]);
  const leaseRows = dicts(body["leases"]);
  if (nodeRows === null || leaseRows === null) {
    return null;
  }
  return {
    enabled: bool(body["enabled"]),
    now: num(body["now"]),
    nodes: nodeRows.map((n) => ({
      node: str(n["node"]),
      mode: text(n["mode"]),
      encoders: strings(n["encoders"]),
      waiting: bool(n["waiting"]),
      coolingUntil: num(n["cooling_until"]),
      leasesActive: num(n["leases_active"]),
    })),
    leases: leaseRows.map((l) => ({
      node: str(l["node"]),
      path: str(l["path"]),
      state: text(l["state"]),
      epoch: num(l["epoch"]),
      grantedAt: num(l["granted_at"]),
      updatedAt: num(l["updated_at"]),
      expiresAt: num(l["expires_at"]),
      endedAt: num(l["ended_at"]),
      reason: text(l["reason"]),
      sourceBytes: num(l["source_bytes"]),
      outputBytes: num(l["output_bytes"]),
    })),
    total: rowTotal(body["leases_total"]),
  };
}

export function loadNodes(options: Options = {}): Promise<ApiResult<Nodes>> {
  return read(NODES_PATH, "a list of nodes and leases", parseNodes, options);
}

// --- the controls --------------------------------------------------------------------
//
// Exactly the mutating endpoints the API offers. Each answers with what the server said:
// a refusal keeps its status, its sentence and, where it is JSON, its body.

export const PAUSE_PATH = "/api/pause";
export const RESUME_PATH = "/api/resume";
export const RESCAN_PATH = "/api/rescan";
export const SCAN_PATH = "/api/scan";
export const EXCLUSIONS_PATH = "/api/exclusions";

export interface ControlState {
  paused: boolean | null;
  scanning: boolean | null;
}

function controlState(body: unknown): ControlState | null {
  return isDict(body) ? { paused: bool(body["paused"]), scanning: bool(body["scanning"]) } : null;
}

async function send<T>(
  method: "POST" | "DELETE",
  path: string,
  what: string,
  parse: (body: unknown) => T | null,
  options: Options & { body?: unknown },
): Promise<ApiResult<T>> {
  const result = await request(method, path, options);
  if (result.kind !== "ok") {
    return result;
  }
  const value = parse(result.value);
  return value === null ? misshapen(path, what) : { kind: "ok", value };
}

export function pause(options: Options = {}): Promise<ApiResult<ControlState>> {
  return send("POST", PAUSE_PATH, "the pause state", controlState, options);
}

export function resume(options: Options = {}): Promise<ApiResult<ControlState>> {
  return send("POST", RESUME_PATH, "the pause state", controlState, options);
}

/** The body of `POST /api/rescan`, under 202 and under 409 alike. */
export interface ScanStart {
  started: boolean | null;
  reason: string | null;
  paused: boolean | null;
  scanning: boolean | null;
}

export function parseScanStart(body: unknown): ScanStart | null {
  if (!isDict(body)) {
    return null;
  }
  return {
    started: bool(body["started"]),
    reason: text(body["reason"]),
    paused: bool(body["paused"]),
    scanning: bool(body["scanning"]),
  };
}

export function startLibraryScan(options: Options = {}): Promise<ApiResult<ScanStart>> {
  return send("POST", RESCAN_PATH, "whether a scan started", parseScanStart, options);
}

/** The envelope of `POST /api/scan`, under every status past the token gate. */
export interface ScanReport {
  accepted: number | null;
  rejected: number | null;
  rule: string | null;
  error: string | null;
  retryable: boolean | null;
  results: {
    path: string | null;
    accepted: boolean | null;
    resolved: string | null;
    rule: string | null;
    detail: string | null;
    retryable: boolean | null;
  }[];
}

export function parseScanReport(body: unknown): ScanReport | null {
  if (!isDict(body)) {
    return null;
  }
  const results = dicts(body["results"]);
  if (results === null) {
    return null;
  }
  return {
    accepted: num(body["accepted"]),
    rejected: num(body["rejected"]),
    rule: text(body["rule"]),
    error: text(body["error"]),
    retryable: bool(body["retryable"]),
    results: results.map((r) => ({
      path: str(r["path"]),
      accepted: bool(r["accepted"]),
      resolved: text(r["resolved"]),
      rule: text(r["rule"]),
      detail: text(r["detail"]),
      retryable: bool(r["retryable"]),
    })),
  };
}

export function scanPaths(paths: string[], options: Options = {}): Promise<ApiResult<ScanReport>> {
  return send("POST", SCAN_PATH, "a scan report", parseScanReport, { ...options, body: { paths } });
}

export interface Exclusions {
  paths: { path: string; createdAt: number | null }[];
  runtimeState: string | null;
}

export function parseExclusions(body: unknown): Exclusions | null {
  if (!isDict(body)) {
    return null;
  }
  const rows = dicts(body["exclusions"]);
  if (rows === null) {
    return null;
  }
  const paths: Exclusions["paths"] = [];
  for (const row of rows) {
    const path = str(row["path"]);
    if (path === null) {
      return null;
    }
    paths.push({ path, createdAt: num(row["created_at"]) });
  }
  return { paths, runtimeState: text(body["runtime_state"]) };
}

export function loadExclusions(options: Options = {}): Promise<ApiResult<Exclusions>> {
  return read(EXCLUSIONS_PATH, "the withheld paths", parseExclusions, options);
}

/** The answer to withholding a path or ceasing to: what actually moved. */
export interface ExclusionChange {
  path: string | null;
  changed: boolean | null;
  runtimeState: string | null;
}

function exclusionChange(body: unknown): ExclusionChange | null {
  return isDict(body)
    ? { path: str(body["path"]), changed: bool(body["changed"]), runtimeState: text(body["runtime_state"]) }
    : null;
}

export function addExclusion(path: string, options: Options = {}): Promise<ApiResult<ExclusionChange>> {
  return send("POST", EXCLUSIONS_PATH, "what changed", exclusionChange, { ...options, body: { path } });
}

export function removeExclusion(path: string, options: Options = {}): Promise<ApiResult<ExclusionChange>> {
  return send("DELETE", EXCLUSIONS_PATH, "what changed", exclusionChange, { ...options, body: { path } });
}
