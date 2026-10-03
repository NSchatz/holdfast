import { describe, expect, it } from "vitest";

import { bool, dicts, isDict, num, str, strings, text } from "./shape";

describe("shape: a field that cannot be read is null, never a default", () => {
  it("reads a number only from a finite number", () => {
    expect(num(0)).toBe(0);
    expect(num(12.5)).toBe(12.5);
    for (const v of [null, undefined, "12", true, NaN, Infinity, {}, []]) {
      expect(num(v)).toBeNull();
    }
  });

  it("keeps an empty string as a string, and tells it from a missing one", () => {
    expect(str("")).toBe("");
    expect(text("")).toBeNull();
    expect(text("x")).toBe("x");
    expect(str(null)).toBeNull();
    expect(str(7)).toBeNull();
  });

  it("assumes neither true nor false", () => {
    expect(bool(false)).toBe(false);
    expect(bool(true)).toBe(true);
    for (const v of [null, undefined, 0, 1, "true"]) {
      expect(bool(v)).toBeNull();
    }
  });

  it("tells a list that is absent from an empty one", () => {
    expect(dicts(null)).toBeNull();
    expect(dicts({})).toBeNull();
    expect(dicts([])).toEqual([]);
    expect(dicts([{ a: 1 }, 2, null])).toEqual([{ a: 1 }]);
    expect(strings(null)).toBeNull();
    expect(strings(["a", 1])).toEqual(["a"]);
  });

  it("does not take a list or null for an object", () => {
    expect(isDict({})).toBe(true);
    expect(isDict([])).toBe(false);
    expect(isDict(null)).toBe(false);
  });
});
