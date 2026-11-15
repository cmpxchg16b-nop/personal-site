"use client";

import type { EPAddr } from "./types";

// The complete user/session pair identifying one browser connection. Neither
// field is authoritative until the SS confirms it in the connection's hello.
export type EndpointIdentity = Readonly<
  Required<Pick<EPAddr, "userId" | "userSessionId">>
>;

export const SS_IDENTITY_STORAGE_KEY = "personal-site-ss-identity";

export function isEndpointIdentity(value: unknown): value is EndpointIdentity {
  if (value === null || typeof value !== "object") return false;
  const candidate = value as Partial<EPAddr>;
  return (
    typeof candidate.userId === "string" &&
    candidate.userId.length > 0 &&
    typeof candidate.userSessionId === "string" &&
    candidate.userSessionId.length > 0
  );
}

export function sameEndpointIdentity(
  a: EndpointIdentity,
  b: EndpointIdentity,
): boolean {
  return a.userId === b.userId && a.userSessionId === b.userSessionId;
}

function storage(): Storage | null {
  try {
    return typeof window === "undefined" ? null : window.localStorage;
  } catch {
    return null;
  }
}

// This is only a pre-dial hint. In particular, callers must not use it to
// accept flooded events: the connection's validated hello is the sole source
// of its authoritative identity.
export function readIdentityHint(): EndpointIdentity | null {
  const store = storage();
  if (store === null) return null;
  let raw: string | null;
  try {
    raw = store.getItem(SS_IDENTITY_STORAGE_KEY);
  } catch {
    return null;
  }
  if (raw === null) return null;
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!isEndpointIdentity(parsed)) throw new Error("incomplete identity");
    return {
      userId: parsed.userId,
      userSessionId: parsed.userSessionId,
    };
  } catch {
    try {
      store.removeItem(SS_IDENTITY_STORAGE_KEY);
    } catch {
      // Storage may be unavailable even when a read succeeded.
    }
    return null;
  }
}

// A single localStorage write replaces the whole pair atomically. Storage
// failures must never prevent an otherwise valid signalling connection.
export function writeStoredIdentity(identity: EndpointIdentity): void {
  const store = storage();
  if (store === null) return;
  try {
    store.setItem(
      SS_IDENTITY_STORAGE_KEY,
      JSON.stringify({
        userId: identity.userId,
        userSessionId: identity.userSessionId,
      }),
    );
  } catch {
    // Private browsing and quota/security errors are non-fatal.
  }
}

export function clearStoredIdentity(): void {
  const store = storage();
  if (store === null) return;
  try {
    store.removeItem(SS_IDENTITY_STORAGE_KEY);
  } catch {
    // Logout/401 cleanup remains best-effort when storage is unavailable.
  }
}
