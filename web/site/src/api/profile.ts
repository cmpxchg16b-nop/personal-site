"use client";

// Client for the server's profile endpoint: GET /api/profile answers the
// caller's session identity. Behind the server's JWT middleware the
// endpoint answers 401 for anonymous callers, so a failed fetch doubles
// as the "not logged in" signal.

export type Profile = {
  session_id: string;
  subject_id: string;
  username: string;
  email: string;
};

// ProfileApiError is a non-2xx response from GET /api/profile. It carries
// the HTTP status so callers can tell a definitive "not logged in" (401)
// from a possibly transient failure.
export class ProfileApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ProfileApiError";
  }
}

// fetchProfile GETs /api/profile, throwing ProfileApiError on a non-2xx
// response. Pass an AbortSignal to cancel the request (e.g. on unmount —
// StrictMode's mount-unmount-mount would otherwise duplicate it); an
// aborted request rejects with an AbortError DOMException.
export async function fetchProfile(signal?: AbortSignal): Promise<Profile> {
  const res = await fetch("/api/profile", { signal });
  if (!res.ok) {
    throw new ProfileApiError(
      res.status,
      `GET /api/profile failed: ${res.status}`,
    );
  }
  return res.json();
}
