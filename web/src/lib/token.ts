// The one place the token lives: a variable of the running page. It is written to no
// storage, no cookie, no URL and no log; a reload starts with none, and `forget` drops
// it. docs/design/web-ui.md#token carries the reasoning.
//
// The holder keeps the value in a closure and not on a property, so serialising the
// holder, logging it or spreading it carries no token.

export interface TokenHolder {
  /** The token, or "" when none is held. */
  get(): string;
  /** Hold `value`, with surrounding whitespace removed. An empty value holds none. */
  set(value: string): void;
  /** Drop the token. */
  forget(): void;
  /** Call `listener` now and after every change. Returns the function that stops it. */
  subscribe(listener: (token: string) => void): () => void;
}

export function createTokenHolder(): TokenHolder {
  let token = "";
  const listeners = new Set<(token: string) => void>();
  const tell = () => {
    for (const listener of listeners) {
      listener(token);
    }
  };
  return Object.freeze({
    get: () => token,
    set: (value: string) => {
      token = value.trim();
      tell();
    },
    forget: () => {
      token = "";
      tell();
    },
    subscribe: (listener: (token: string) => void) => {
      listeners.add(listener);
      listener(token);
      return () => {
        listeners.delete(listener);
      };
    },
  });
}
