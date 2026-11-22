import { afterEach, describe, expect, it, vi } from "vitest";

import {
  readStoredVisibility,
  SS_VISIBILITY_STORAGE_KEY,
  writeStoredVisibility,
} from "./visibility";

function stubStorage(initial?: string) {
  const values = new Map<string, string>();
  if (initial !== undefined) values.set(SS_VISIBILITY_STORAGE_KEY, initial);
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

describe("stored signalling visibility", () => {
  it("defaults to visible without a stored preference", () => {
    stubStorage();
    expect(readStoredVisibility()).toBe(true);
  });

  it("restores both explicit states and persists replacements", () => {
    const storage = stubStorage("false");
    expect(readStoredVisibility()).toBe(false);
    writeStoredVisibility(true);
    expect(readStoredVisibility()).toBe(true);
    writeStoredVisibility(false);
    expect(readStoredVisibility()).toBe(false);
    expect(storage.setItem).toHaveBeenLastCalledWith(
      SS_VISIBILITY_STORAGE_KEY,
      "false",
    );
  });

  it("discards a corrupt value and falls back to visible", () => {
    const storage = stubStorage("yes");
    expect(readStoredVisibility()).toBe(true);
    expect(storage.removeItem).toHaveBeenCalledWith(SS_VISIBILITY_STORAGE_KEY);
  });

  it("contains storage failures", () => {
    vi.stubGlobal("window", {
      localStorage: {
        getItem: () => {
          throw new Error("read denied");
        },
        setItem: () => {
          throw new Error("write denied");
        },
      },
    });
    expect(readStoredVisibility()).toBe(true);
    expect(() => writeStoredVisibility(false)).not.toThrow();
  });
});
