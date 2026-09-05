"use client";

import { useQuery } from "@tanstack/react-query";

// fetchHomeLiveWHEPURL calls GET /api/homeLiveWHEPURL: the WHEP endpoint of
// the home page's live stream (the <homeLiveWHEPURL/> element of
// serverConfig.xml, see pkg/api/homelive). The endpoint sits on the JWT
// whitelist, so it answers anonymous callers too; an empty url means the
// document configures no live stream, and the home page shows no Live
// section.
async function fetchHomeLiveWHEPURL(): Promise<string> {
  const res = await fetch("/api/homeLiveWHEPURL");
  if (!res.ok) {
    throw new Error(`failed to fetch the live stream URL: ${res.status}`);
  }
  const body = (await res.json()) as { url?: string };
  return body.url ?? "";
}

// useHomeLiveWHEPURL fetches and caches the live stream's WHEP endpoint
// under the "homeLiveWHEPURL" query key. The URL only changes with the
// server configuration, so it is never considered stale.
export function useHomeLiveWHEPURL() {
  return useQuery({
    queryKey: ["homeLiveWHEPURL"],
    queryFn: fetchHomeLiveWHEPURL,
    staleTime: Infinity,
  });
}
