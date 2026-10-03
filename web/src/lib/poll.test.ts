import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { startPolling, type VisibilitySource } from "./poll";

function fakeDocument(state = "visible") {
  const listeners = new Set<() => void>();
  const doc: VisibilitySource & { show(): void; hide(): void; listeners: Set<() => void> } = {
    visibilityState: state,
    addEventListener: (_type, listener) => void listeners.add(listener),
    removeEventListener: (_type, listener) => void listeners.delete(listener),
    listeners,
    show() {
      doc.visibilityState = "visible";
      listeners.forEach((l) => l());
    },
    hide() {
      doc.visibilityState = "hidden";
      listeners.forEach((l) => l());
    },
  };
  return doc;
}

describe("polling", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("reads at once and then on the interval", async () => {
    const read = vi.fn(() => Promise.resolve());
    const poller = startPolling(read, 1000, fakeDocument());
    expect(read).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(3000);
    expect(read).toHaveBeenCalledTimes(4);
    poller.stop();
  });

  it("does not read on the interval while the page is hidden, and reads when it is shown again", async () => {
    const doc = fakeDocument();
    const read = vi.fn(() => Promise.resolve());
    const poller = startPolling(read, 1000, doc);
    await vi.advanceTimersByTimeAsync(0);
    doc.hide();
    await vi.advanceTimersByTimeAsync(5000);
    expect(read).toHaveBeenCalledTimes(1);
    doc.show();
    expect(read).toHaveBeenCalledTimes(2);
    poller.stop();
  });

  it("skips a tick while a read is still in flight", async () => {
    const read = vi.fn(() => new Promise<void>(() => {}));
    const poller = startPolling(read, 1000, fakeDocument());
    await vi.advanceTimersByTimeAsync(5000);
    expect(read).toHaveBeenCalledTimes(1);
    poller.stop();
  });

  it("a manual refresh abandons the read in flight and starts another", () => {
    const signals: AbortSignal[] = [];
    const read = vi.fn((signal: AbortSignal) => {
      signals.push(signal);
      return new Promise<void>(() => {});
    });
    const poller = startPolling(read, 1000, fakeDocument());
    poller.refresh();
    expect(read).toHaveBeenCalledTimes(2);
    expect(signals[0]?.aborted).toBe(true);
    expect(signals[1]?.aborted).toBe(false);
    poller.stop();
  });

  it("stopping aborts the read in flight, and nothing reads afterwards", async () => {
    const doc = fakeDocument();
    const signals: AbortSignal[] = [];
    const read = vi.fn((signal: AbortSignal) => {
      signals.push(signal);
      return new Promise<void>(() => {});
    });
    const poller = startPolling(read, 1000, doc);
    poller.stop();
    expect(signals[0]?.aborted).toBe(true);
    await vi.advanceTimersByTimeAsync(5000);
    poller.refresh();
    expect(read).toHaveBeenCalledTimes(1);
    expect(doc.listeners.size).toBe(0);
  });

  it("an interval of zero reads once and then only when asked", async () => {
    const read = vi.fn(() => Promise.resolve());
    const poller = startPolling(read, 0, fakeDocument());
    await vi.advanceTimersByTimeAsync(60000);
    expect(read).toHaveBeenCalledTimes(1);
    poller.refresh();
    expect(read).toHaveBeenCalledTimes(2);
    poller.stop();
  });

  it("a read that fails does not stop the next one", async () => {
    const read = vi.fn(() => Promise.reject(new Error("boom")));
    const poller = startPolling(read, 1000, fakeDocument());
    await vi.advanceTimersByTimeAsync(1000);
    expect(read).toHaveBeenCalledTimes(2);
    poller.stop();
  });
});
