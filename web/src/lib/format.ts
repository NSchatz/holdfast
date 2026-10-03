// How a figure is written on the page. One rule runs through all of it: a figure that is
// null was not read or not recorded, and it is written in words, never as a zero.

export const UNAVAILABLE = "unavailable";
export const NOT_RECORDED = "not recorded";

const UNITS = ["KiB", "MiB", "GiB", "TiB", "PiB"] as const;

/** A byte count in binary (IEC) units: 1536 is "1.50 KiB". */
export function formatBytes(bytes: number | null, missing: string = UNAVAILABLE): string {
  if (bytes === null || !Number.isFinite(bytes)) {
    return missing;
  }
  const sign = bytes < 0 ? "-" : "";
  let value = Math.abs(bytes);
  if (value < 1024) {
    return `${sign}${value} B`;
  }
  let unit = -1;
  while (value >= 1024 && unit < UNITS.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${sign}${value.toFixed(2)} ${UNITS[unit] ?? "B"}`;
}

/** The exact count behind a rounded one, for a `title`: "1536 bytes". Empty when there is none. */
export function exactBytes(bytes: number | null): string {
  return bytes === null || !Number.isFinite(bytes) ? "" : `${bytes} bytes`;
}

/** A whole count in plain digits. */
export function formatCount(n: number | null, missing: string = UNAVAILABLE): string {
  return n === null || !Number.isFinite(n) ? missing : String(n);
}

/** A length of time given in seconds: "45 s", "3 min 05 s", "2 h 07 min", "1 d 03 h". */
export function formatDuration(seconds: number | null, missing: string = UNAVAILABLE): string {
  if (seconds === null || !Number.isFinite(seconds) || seconds < 0) {
    return missing;
  }
  const s = Math.floor(seconds);
  const two = (n: number) => String(n).padStart(2, "0");
  if (s < 60) {
    return `${s} s`;
  }
  if (s < 3600) {
    return `${Math.floor(s / 60)} min ${two(s % 60)} s`;
  }
  if (s < 86400) {
    return `${Math.floor(s / 3600)} h ${two(Math.floor((s % 3600) / 60))} min`;
  }
  return `${Math.floor(s / 86400)} d ${two(Math.floor((s % 86400) / 3600))} h`;
}

/** A length of time given in milliseconds. Under a second it is written in milliseconds. */
export function formatMillis(ms: number | null, missing: string = UNAVAILABLE): string {
  if (ms === null || !Number.isFinite(ms) || ms < 0) {
    return missing;
  }
  return ms < 1000 ? `${Math.round(ms)} ms` : formatDuration(ms / 1000, missing);
}

/**
 * A unix time in seconds, written in the zone `offsetMinutes` east of UTC with that
 * offset in digits: "2026-10-03 17:33:44 +02:00".
 */
export function formatTimeAt(unixSeconds: number | null, offsetMinutes: number, missing: string = UNAVAILABLE): string {
  if (unixSeconds === null || !Number.isFinite(unixSeconds)) {
    return missing;
  }
  const shifted = new Date((unixSeconds + offsetMinutes * 60) * 1000);
  if (Number.isNaN(shifted.getTime())) {
    return missing;
  }
  const two = (n: number) => String(n).padStart(2, "0");
  const sign = offsetMinutes < 0 ? "-" : "+";
  const abs = Math.abs(offsetMinutes);
  return (
    `${shifted.getUTCFullYear()}-${two(shifted.getUTCMonth() + 1)}-${two(shifted.getUTCDate())} ` +
    `${two(shifted.getUTCHours())}:${two(shifted.getUTCMinutes())}:${two(shifted.getUTCSeconds())} ` +
    `${sign}${two(Math.floor(abs / 60))}:${two(abs % 60)}`
  );
}

/** A unix time in seconds, in the browser's own zone, with its numeric offset. */
export function formatTime(unixSeconds: number | null, missing: string = UNAVAILABLE): string {
  if (unixSeconds === null || !Number.isFinite(unixSeconds)) {
    return missing;
  }
  const offset = -new Date(unixSeconds * 1000).getTimezoneOffset();
  return formatTimeAt(unixSeconds, Number.isFinite(offset) ? offset : 0, missing);
}

/** A fraction in [0,1] as a percentage with one decimal: 0.425 is "42.5%". */
export function formatFraction(fraction: number | null, missing: string = UNAVAILABLE): string {
  return fraction === null || !Number.isFinite(fraction) ? missing : `${(fraction * 100).toFixed(1)}%`;
}

/** A number with a fixed count of decimals, for a score or a ratio. */
export function formatNumber(n: number | null, decimals: number, missing: string = UNAVAILABLE): string {
  return n === null || !Number.isFinite(n) ? missing : n.toFixed(decimals);
}

/** A picture's size, "1920x1080", only where both sides were recorded. */
export function formatDimensions(width: number | null, height: number | null, missing: string = NOT_RECORDED): string {
  return width === null || height === null ? missing : `${width}x${height}`;
}

/**
 * What a swap saved: the bytes and the share of the source. null where either size was
 * not recorded, or the source size is not a size a share can be taken of.
 */
export function saving(sourceBytes: number | null, outputBytes: number | null): { bytes: number; fraction: number } | null {
  if (sourceBytes === null || outputBytes === null || sourceBytes <= 0) {
    return null;
  }
  return { bytes: sourceBytes - outputBytes, fraction: (sourceBytes - outputBytes) / sourceBytes };
}

/**
 * How old a figure the server served from its cache is, in words: "as of 12 s before this
 * read". Empty where the figure is current or the server gave no age: nothing is claimed
 * about an age nobody stated.
 */
export function ageNote(ageSeconds: number | null): string {
  return ageSeconds !== null && Number.isFinite(ageSeconds) && ageSeconds > 0
    ? `as of ${formatDuration(ageSeconds)} before this read`
    : "";
}

/**
 * What to say where more rows are shown than the server's total counts. The rows are read
 * fresh and the total may be older, so "N of M" would read as a contradiction; this says
 * which of the two is behind, and never puts the two figures side by side as a share.
 */
export function totalBehind(count: number, ageSeconds: number | null): string {
  return ageNote(ageSeconds) === ""
    ? `The server's total of ${count} is lower than the rows shown: it was counted apart from them, and the rows are the newer of the two.`
    : `The server's total of ${count} was counted ${formatDuration(ageSeconds)} before this read, so it is older than the rows shown.`;
}
