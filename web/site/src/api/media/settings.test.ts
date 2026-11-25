import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  DEFAULT_MEDIA_SETTINGS,
  MEDIA_SETTINGS_STORAGE_KEY,
  loadMediaSettings,
  normalizeMediaSettings,
  saveMediaSettings,
} from "./settings";

const values = new Map<string, string>();

beforeEach(() => {
  values.clear();
  vi.stubGlobal("window", {
    localStorage: {
      getItem: (key: string) => values.get(key) ?? null,
      setItem: (key: string, value: string) => values.set(key, value),
    },
  });
});

describe("media settings persistence", () => {
  it("round-trips choices and preserves fields changed by other controllers", () => {
    saveMediaSettings({ microphoneVolume: 0.3, microphoneId: "mic-a" });
    saveMediaSettings({ speakerVolume: 0.25, cameraId: "camera-b" });

    expect(loadMediaSettings()).toEqual({
      microphoneId: "mic-a",
      cameraId: "camera-b",
      microphoneVolume: 0.3,
      speakerVolume: 0.25,
    });
    expect(JSON.parse(values.get(MEDIA_SETTINGS_STORAGE_KEY) ?? "{}")).toEqual(
      loadMediaSettings(),
    );
  });

  it("validates malformed JSON, fields and volume ranges", () => {
    values.set(MEDIA_SETTINGS_STORAGE_KEY, "not-json");
    expect(loadMediaSettings()).toEqual(DEFAULT_MEDIA_SETTINGS);
    expect(
      normalizeMediaSettings({
        microphoneId: 42,
        cameraId: "camera",
        microphoneVolume: 2,
        speakerVolume: Number.NaN,
      }),
    ).toEqual({ ...DEFAULT_MEDIA_SETTINGS, cameraId: "camera" });
  });

  it("keeps live operation possible when browser storage throws", () => {
    vi.stubGlobal("window", {
      localStorage: {
        getItem: () => {
          throw new DOMException("denied", "SecurityError");
        },
        setItem: () => {
          throw new DOMException("denied", "SecurityError");
        },
      },
    });
    expect(() => saveMediaSettings({ speakerVolume: 0.4 })).not.toThrow();
    expect(loadMediaSettings()).toEqual(DEFAULT_MEDIA_SETTINGS);
  });
});
