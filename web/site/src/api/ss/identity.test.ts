import { afterEach, describe, expect, it, vi } from "vitest";

import {
  clearStoredIdentity,
  isEndpointIdentity,
  readIdentityHint,
  sameEndpointIdentity,
  SS_IDENTITY_STORAGE_KEY,
  writeStoredIdentity,
} from "./identity";

function stubStorage(initial?: string) {
  const values = new Map<string, string>();
  if (initial !== undefined) values.set(SS_IDENTITY_STORAGE_KEY, initial);
  const getItem = vi.fn((key: string) => values.get(key) ?? null);
  const setItem = vi.fn((key: string, value: string) => values.set(key, value));
  const removeItem = vi.fn((key: string) => values.delete(key));
  vi.stubGlobal("window", {
    localStorage: { getItem, setItem, removeItem },
  });
  return { values, getItem, setItem, removeItem };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("endpoint identity", () => {
  it("validates and compares the complete pair", () => {
    const identity = { userId: "user", userSessionId: "session" };
    expect(isEndpointIdentity(identity)).toBe(true);
    expect(isEndpointIdentity({ userId: "user" })).toBe(false);
    expect(isEndpointIdentity({ userSessionId: "session" })).toBe(false);
    expect(isEndpointIdentity({ userId: "", userSessionId: "session" })).toBe(
      false,
    );
    expect(
      isEndpointIdentity({ userId: "user", userSessionId: 42 }),
    ).toBe(false);
    expect(isEndpointIdentity(null)).toBe(false);
    expect(sameEndpointIdentity(identity, { ...identity })).toBe(true);
    expect(
      sameEndpointIdentity(identity, {
        userId: "user",
        userSessionId: "other-session",
      }),
    ).toBe(false);
  });

  it("reads a valid cached pair as a detached hint", () => {
    stubStorage(
      JSON.stringify({
        userId: "cached-user",
        userSessionId: "cached-session",
        serviceId: "ignored",
      }),
    );
    expect(readIdentityHint()).toEqual({
      userId: "cached-user",
      userSessionId: "cached-session",
    });
  });

  it.each([
    "not-json",
    "null",
    JSON.stringify({ userId: "half", userSessionId: "" }),
    JSON.stringify({ userId: "half" }),
  ])("discards a corrupt or half-cached value: %s", (raw) => {
    const storage = stubStorage(raw);
    expect(readIdentityHint()).toBeNull();
    expect(storage.removeItem).toHaveBeenCalledWith(SS_IDENTITY_STORAGE_KEY);
    expect(storage.values.has(SS_IDENTITY_STORAGE_KEY)).toBe(false);
  });

  it("replaces the pair with one JSON write and clears the same key", () => {
    const storage = stubStorage();
    writeStoredIdentity({ userId: "first", userSessionId: "one" });
    expect(storage.setItem).toHaveBeenCalledTimes(1);
    expect(storage.setItem).toHaveBeenLastCalledWith(
      SS_IDENTITY_STORAGE_KEY,
      JSON.stringify({ userId: "first", userSessionId: "one" }),
    );

    writeStoredIdentity({ userId: "second", userSessionId: "two" });
    expect(storage.values.size).toBe(1);
    expect(readIdentityHint()).toEqual({
      userId: "second",
      userSessionId: "two",
    });
    clearStoredIdentity();
    expect(storage.removeItem).toHaveBeenCalledWith(SS_IDENTITY_STORAGE_KEY);
    expect(readIdentityHint()).toBeNull();
  });

  it("contains storage read, write, and cleanup failures", () => {
    vi.stubGlobal("window", {
      localStorage: {
        getItem: () => {
          throw new Error("read denied");
        },
        setItem: () => {
          throw new Error("write denied");
        },
        removeItem: () => {
          throw new Error("remove denied");
        },
      },
    });
    expect(readIdentityHint()).toBeNull();
    expect(() =>
      writeStoredIdentity({ userId: "user", userSessionId: "session" }),
    ).not.toThrow();
    expect(clearStoredIdentity).not.toThrow();

    const corrupt = stubStorage("not-json");
    corrupt.removeItem.mockImplementation(() => {
      throw new Error("remove denied");
    });
    expect(readIdentityHint()).toBeNull();
  });

  it("handles an inaccessible localStorage property", () => {
    vi.stubGlobal("window", {});
    Object.defineProperty(window, "localStorage", {
      configurable: true,
      get: () => {
        throw new Error("storage denied");
      },
    });
    expect(readIdentityHint()).toBeNull();
    expect(clearStoredIdentity).not.toThrow();
  });
});
