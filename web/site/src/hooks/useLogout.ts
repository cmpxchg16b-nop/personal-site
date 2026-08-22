"use client";

import { useMutation } from "@tanstack/react-query";

// logout calls POST /api/logout, which clears the JWT and nonce cookies. The
// handler answers with a redirect meant for browser navigations; an SPA fetch
// just follows it, so callers navigate themselves on success (with a
// full-page load, which also drops the query cache and other client state
// tied to the old session).
async function logout(): Promise<void> {
  const res = await fetch("/api/logout", { method: "POST" });
  if (!res.ok) throw new Error(`failed to log out: ${res.status}`);
}

// useLogout ends the current session server-side. onSuccess is where callers
// should redirect, e.g. window.location.assign("/").
export function useLogout() {
  return useMutation({ mutationFn: logout });
}
