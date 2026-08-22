"use client";

import { useQuery } from "@tanstack/react-query";

import { fetchProfile, ProfileApiError } from "@/api/profile";

// The wire client, Profile type, and ProfileApiError live in
// @/api/profile; re-exported here so existing imports keep working.
export { ProfileApiError };
export type { Profile } from "@/api/profile";

// useProfile fetches GET /api/profile. refetchInterval (milliseconds) lets
// callers poll, so the account area can pick up profile changes without a
// full page load. A 401 means the caller is anonymous — a definitive answer,
// not a transient failure — so it is NOT retried: react-query's default
// retry-with-backoff would otherwise keep the query pending for seconds
// before the error state (which drives the anonymous UI) becomes visible.
export function useProfile(refetchInterval?: number) {
  return useQuery({
    queryKey: ["profile"],
    queryFn: ({ signal }) => fetchProfile(signal),
    refetchInterval,
    retry: (failureCount, error) =>
      !(error instanceof ProfileApiError && error.status === 401) &&
      failureCount < 3,
  });
}
