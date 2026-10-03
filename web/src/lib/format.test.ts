import { describe, expect, it } from "vitest";

import {
  exactBytes,
  formatBytes,
  formatCount,
  formatDimensions,
  formatDuration,
  formatFraction,
  formatMillis,
  formatNumber,
  formatTime,
  formatTimeAt,
  saving,
} from "./format";

describe("format: bytes are binary (IEC) units with the exact count beside them", () => {
  it.each([
    [0, "0 B"],
    [1023, "1023 B"],
    [1024, "1.00 KiB"],
    [1536, "1.50 KiB"],
    [1024 ** 2, "1.00 MiB"],
    [5 * 1024 ** 3, "5.00 GiB"],
    [1.5 * 1024 ** 4, "1.50 TiB"],
    [1024 ** 5, "1.00 PiB"],
    [4096 * 1024 ** 5, "4096.00 PiB"],
    [-2048, "-2.00 KiB"],
  ])("writes %d as %s", (bytes, want) => {
    expect(formatBytes(bytes)).toBe(want);
  });

  it("writes a null as words, never as 0 B", () => {
    expect(formatBytes(null)).toBe("unavailable");
    expect(formatBytes(null, "not recorded")).toBe("not recorded");
    expect(formatBytes(NaN)).toBe("unavailable");
    expect(exactBytes(null)).toBe("");
  });

  it("gives the exact count for a title", () => {
    expect(exactBytes(1610612736)).toBe("1610612736 bytes");
    expect(exactBytes(0)).toBe("0 bytes");
  });
});

describe("format: durations", () => {
  it.each([
    [0, "0 s"],
    [59.9, "59 s"],
    [60, "1 min 00 s"],
    [185, "3 min 05 s"],
    [3600, "1 h 00 min"],
    [7620, "2 h 07 min"],
    [97200, "1 d 03 h"],
  ])("writes %d seconds as %s", (seconds, want) => {
    expect(formatDuration(seconds)).toBe(want);
  });

  it("writes milliseconds under a second as milliseconds", () => {
    expect(formatMillis(450)).toBe("450 ms");
    expect(formatMillis(185000)).toBe("3 min 05 s");
  });

  it("writes a null or a negative length as words", () => {
    expect(formatDuration(null)).toBe("unavailable");
    expect(formatDuration(-1)).toBe("unavailable");
    expect(formatMillis(null, "not recorded")).toBe("not recorded");
  });
});

describe("format: times carry a numeric offset", () => {
  it("writes a unix time in a zone east of UTC", () => {
    expect(formatTimeAt(1700000000, 120)).toBe("2023-11-15 00:13:20 +02:00");
  });

  it("writes it in UTC and in a zone west of UTC with a half hour", () => {
    expect(formatTimeAt(1700000000, 0)).toBe("2023-11-14 22:13:20 +00:00");
    expect(formatTimeAt(1700000000, -210)).toBe("2023-11-14 18:43:20 -03:30");
  });

  it("writes the browser's own zone with its offset in digits", () => {
    expect(formatTime(1700000000)).toMatch(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} [+-]\d{2}:\d{2}$/);
    const offset = -new Date(1700000000 * 1000).getTimezoneOffset();
    expect(formatTime(1700000000)).toBe(formatTimeAt(1700000000, offset));
  });

  it("writes a null as words, never as 1970", () => {
    expect(formatTime(null)).toBe("unavailable");
    expect(formatTimeAt(null, 0, "not recorded")).toBe("not recorded");
  });
});

describe("format: counts, fractions, scores and dimensions", () => {
  it("writes a null as words, never as 0", () => {
    expect(formatCount(null)).toBe("unavailable");
    expect(formatCount(0)).toBe("0");
    expect(formatFraction(null)).toBe("unavailable");
    expect(formatFraction(0.425)).toBe("42.5%");
    expect(formatNumber(null, 2, "not recorded")).toBe("not recorded");
    expect(formatNumber(95.128, 2)).toBe("95.13");
  });

  it("writes dimensions only where both sides were recorded", () => {
    expect(formatDimensions(1920, 1080)).toBe("1920x1080");
    expect(formatDimensions(1920, null)).toBe("not recorded");
    expect(formatDimensions(null, null)).toBe("not recorded");
  });

  it("computes a saving only from two recorded sizes", () => {
    expect(saving(1000, 400)).toEqual({ bytes: 600, fraction: 0.6 });
    expect(saving(null, 400)).toBeNull();
    expect(saving(1000, null)).toBeNull();
    expect(saving(0, 0)).toBeNull();
  });
});
