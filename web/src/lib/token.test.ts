import { describe, expect, it, vi } from "vitest";

import { createTokenHolder } from "./token";

describe("token holder", () => {
  it("starts with no token, as a reload does", () => {
    expect(createTokenHolder().get()).toBe("");
  });

  it("holds what it was given, trimmed, and forgets it", () => {
    const holder = createTokenHolder();
    holder.set("  s3cret\n");
    expect(holder.get()).toBe("s3cret");
    holder.forget();
    expect(holder.get()).toBe("");
  });

  it("tells a subscriber now and on every change, and stops when asked", () => {
    const holder = createTokenHolder();
    const seen: string[] = [];
    const stop = holder.subscribe((t) => seen.push(t));
    holder.set("a");
    holder.forget();
    stop();
    holder.set("b");
    expect(seen).toEqual(["", "a", ""]);
  });

  it("token: is never written to storage, a cookie or the address by the holder", () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    const cookie = vi.spyOn(Document.prototype, "cookie", "set");
    const before = location.href;
    const holder = createTokenHolder();
    holder.set("s3cret");
    holder.forget();
    expect(setItem).not.toHaveBeenCalled();
    expect(cookie).not.toHaveBeenCalled();
    expect(location.href).toBe(before);
  });

  it("token: does not travel with the holder when it is serialised, spread or printed", () => {
    const holder = createTokenHolder();
    holder.set("s3cret");
    expect(JSON.stringify(holder)).not.toContain("s3cret");
    expect(JSON.stringify({ ...holder })).not.toContain("s3cret");
    expect(String(holder)).not.toContain("s3cret");
    expect(Object.values(holder).every((v) => typeof v === "function")).toBe(true);
  });
});
