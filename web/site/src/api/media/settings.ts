export interface MediaSettings {
  microphoneId: string;
  cameraId: string;
  microphoneVolume: number;
  speakerVolume: number;
}

export const DEFAULT_MEDIA_SETTINGS: MediaSettings = {
  microphoneId: "",
  cameraId: "",
  microphoneVolume: 1,
  speakerVolume: 1,
};

export const MEDIA_SETTINGS_STORAGE_KEY = "personal-site.chat.media.v1";

function volume(value: unknown, fallback: number): number {
  return typeof value === "number" &&
    Number.isFinite(value) &&
    value >= 0 &&
    value <= 1
    ? value
    : fallback;
}

export function normalizeMediaSettings(value: unknown): MediaSettings {
  if (typeof value !== "object" || value === null) {
    return { ...DEFAULT_MEDIA_SETTINGS };
  }
  const settings = value as Record<string, unknown>;
  return {
    microphoneId:
      typeof settings.microphoneId === "string" ? settings.microphoneId : "",
    cameraId: typeof settings.cameraId === "string" ? settings.cameraId : "",
    microphoneVolume: volume(settings.microphoneVolume, 1),
    speakerVolume: volume(settings.speakerVolume, 1),
  };
}

export function loadMediaSettings(): MediaSettings {
  if (typeof window === "undefined") return { ...DEFAULT_MEDIA_SETTINGS };
  try {
    const stored = window.localStorage.getItem(MEDIA_SETTINGS_STORAGE_KEY);
    return stored === null
      ? { ...DEFAULT_MEDIA_SETTINGS }
      : normalizeMediaSettings(JSON.parse(stored));
  } catch {
    return { ...DEFAULT_MEDIA_SETTINGS };
  }
}

export function saveMediaSettings(patch: Partial<MediaSettings>): MediaSettings {
  const settings = normalizeMediaSettings({
    ...loadMediaSettings(),
    ...patch,
  });
  if (typeof window !== "undefined") {
    try {
      window.localStorage.setItem(
        MEDIA_SETTINGS_STORAGE_KEY,
        JSON.stringify(settings),
      );
    } catch {
      // Storage can be unavailable in private/embedded contexts. The live
      // controllers still retain the setting for the current page.
    }
  }
  return settings;
}
