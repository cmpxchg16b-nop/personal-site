"use client";

import { useQuery } from "@tanstack/react-query";

// Profile is the account identity served by GET /api/profile. Until the site
// grows a login and account system, the server answers every request with the
// same hard-coded visitor identity.
export type Profile = {
  session_id: string;
  subject_id: string;
  username: string;
  email: string;
};

// useProfile fetches GET /api/profile. refetchInterval (milliseconds) lets
// callers poll, so the account area can pick up profile changes without a
// full page load.
export function useProfile(refetchInterval?: number) {
  return useQuery({
    queryKey: ["profile"],
    queryFn: async (): Promise<Profile> => {
      const res = await fetch("/api/profile");
      if (!res.ok) {
        throw new Error(`GET /api/profile failed: ${res.status}`);
      }
      return res.json();
    },
    refetchInterval,
  });
}
