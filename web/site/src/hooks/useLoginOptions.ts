"use client";

import { useQuery } from "@tanstack/react-query";

// LoginOption is one entry of the login options list served by
// GET /api/login/loginoptions. Mirrors the Go LoginOption struct in
// pkg/api/loginoptions/loginoptions.go, which itself mirrors the
// <loginOption/> element of serverConfig.xml. kind identifies the IdP type
// and selects the login icon (see getLoginIcon in the login page); name is
// the option's unique key. Multiple IdPs of the same kind can coexist under
// different names and display names (e.g. "entra-public" and
// "entra-corporation", both with kind "entra"). Options with an empty
// loginURL are hidden by the login page.
export type LoginOption = {
  kind: string;
  name: string;
  displayName: string;
  label?: string;
  loginURL: string;
};

// LoginOptions is the resolved list of login options exposed by
// useLoginOptions.
export type LoginOptions = LoginOption[];

// fetchLoginOptions calls GET /api/login/loginoptions. The endpoint sits on
// the JWT whitelist (it feeds the login page itself), so it answers for
// logged-out callers too.
async function fetchLoginOptions(): Promise<LoginOptions> {
  const res = await fetch("/api/login/loginoptions");
  if (!res.ok) throw new Error(`failed to fetch login options: ${res.status}`);
  return (await res.json()) as LoginOptions;
}

// useLoginOptions fetches and caches the login page's IdP list under the
// "loginOptions" query key. The list only changes with the server
// configuration, so it is never considered stale.
export function useLoginOptions() {
  return useQuery({
    queryKey: ["loginOptions"],
    queryFn: fetchLoginOptions,
    staleTime: Infinity,
  });
}
