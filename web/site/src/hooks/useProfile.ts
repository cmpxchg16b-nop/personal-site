"use client";

import { useQuery } from "@tanstack/react-query";

// Profile is the account identity served by GET /api/profile. Behind the
// server's JWT middleware the endpoint answers 401 for anonymous callers, so
// a failed fetch doubles as the "not logged in" signal (see ProfileMenu).
export type Profile = {
  session_id: string;
  subject_id: string;
  username: string;
  email: string;
};

// ProfileApiError is a non-2xx response from GET /api/profile. It carries
// the HTTP status so the query can tell a definitive "not logged in" (401 —
// useless to retry) from a possibly transient failure (retried as usual).
export class ProfileApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ProfileApiError";
  }
}

// useProfile fetches GET /api/profile. refetchInterval (milliseconds) lets
// callers poll, so the account area can pick up profile changes without a
// full page load. A 401 means the caller is anonymous — a definitive answer,
// not a transient failure — so it is NOT retried: react-query's default
// retry-with-backoff would otherwise keep the query pending for seconds
// before the error state (which drives the anonymous UI) becomes visible.
export function useProfile(refetchInterval?: number) {
  return useQuery({
    queryKey: ["profile"],
    queryFn: async (): Promise<Profile> => {
      const res = await fetch("/api/profile");
      if (!res.ok) {
        throw new ProfileApiError(
          res.status,
          `GET /api/profile failed: ${res.status}`,
        );
      }
      return res.json();
    },
    refetchInterval,
    retry: (failureCount, error) =>
      !(error instanceof ProfileApiError && error.status === 401) &&
      failureCount < 3,
  });
}
