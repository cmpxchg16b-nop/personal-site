"use client";

// The browser's public-listing preference survives page reloads, but is not an
// identity credential: absent, corrupt, or unavailable storage falls back to
// the protocol default of visible=true.
export const SS_VISIBILITY_STORAGE_KEY = "personal-site-ss-visibility";

function storage(): Storage | null {
  try {
    return typeof window === "undefined"
      ? null
      : (window.localStorage ?? null);
  } catch {
    return null;
  }
}

export function readStoredVisibility(): boolean {
  const store = storage();
  if (store === null) return true;
  let raw: string | null;
  try {
    raw = store.getItem(SS_VISIBILITY_STORAGE_KEY);
  } catch {
    return true;
  }
  if (raw === null || raw === "true") return true;
  if (raw === "false") return false;
  try {
    store.removeItem(SS_VISIBILITY_STORAGE_KEY);
  } catch {
    // Storage cleanup is best-effort when the browser denies access.
  }
  return true;
}

export function writeStoredVisibility(visible: boolean): void {
  const store = storage();
  if (store === null) return;
  try {
    store.setItem(SS_VISIBILITY_STORAGE_KEY, visible ? "true" : "false");
  } catch {
    // Private browsing and quota/security errors do not block live switching.
  }
}
