"use client";

import { useQuery } from "@tanstack/react-query";

// fetchICEServers calls GET /api/iceServers: the WebRTC ICE server URLs
// configured for the caller's origin (the <iceServer/> entries of
// serverConfig.xml, see pkg/api/iceservers). The endpoint sits on the
// JWT whitelist, so it answers anonymous callers too.
async function fetchICEServers(): Promise<string[]> {
  const res = await fetch("/api/iceServers");
  if (!res.ok) throw new Error(`failed to fetch ICE servers: ${res.status}`);
  const body = (await res.json()) as { urls?: string[] };
  return body.urls ?? [];
}

// useICEServers fetches and caches the ICE server URLs under the
// "iceServers" query key. The list only changes with the server
// configuration, so it is never considered stale.
export function useICEServers() {
  return useQuery({
    queryKey: ["iceServers"],
    queryFn: fetchICEServers,
    staleTime: Infinity,
  });
}
