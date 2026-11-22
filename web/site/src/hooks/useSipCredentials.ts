"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

export const SIP_CREDENTIALS_QUERY_KEY = ["sip", "credentials"] as const;
const SIP_CREDENTIALS_PATH = "/api/sip/credentials";

export type SipTransport = "udp" | "tcp" | "tls" | "ws";
export type SipRegistrationState =
  "disabled" | "registering" | "registered" | "failed";

export type SipRegistration = {
  state: SipRegistrationState;
  last_error?: string;
  attempts: number;
  registered_at?: string;
  expires_at?: string;
  retry_at?: string;
};

export type Credential = {
  id: string;
  sip_uri: string;
  server: string;
  username: string;
  transport: SipTransport;
  enabled: boolean;
  registration: SipRegistration;
};

export type CredentialInput = {
  server: string;
  username: string;
  password: string;
  transport: SipTransport;
  /** Omitted on ordinary edits to preserve a concurrently changed server value. */
  enabled?: boolean;
};

export class SipCredentialsApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "SipCredentialsApiError";
  }
}

async function apiError(
  response: Response,
  method: string,
  path: string,
): Promise<SipCredentialsApiError> {
  let message = `${method} ${path} failed: ${response.status}`;
  try {
    const body = (await response.json()) as unknown;
    if (
      typeof body === "object" &&
      body !== null &&
      "err" in body &&
      typeof body.err === "string" &&
      body.err !== ""
    ) {
      message = body.err;
    }
  } catch {
    // Non-JSON error responses retain the HTTP fallback above.
  }
  return new SipCredentialsApiError(response.status, message);
}

export async function fetchSipCredentials(
  signal?: AbortSignal,
): Promise<Credential[]> {
  const response = await fetch(SIP_CREDENTIALS_PATH, { signal });
  if (!response.ok) {
    throw await apiError(response, "GET", SIP_CREDENTIALS_PATH);
  }
  const body = (await response.json()) as { credentials: Credential[] };
  return body.credentials;
}

export async function createSipCredential(
  input: CredentialInput,
): Promise<Credential> {
  const response = await fetch(SIP_CREDENTIALS_PATH, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  if (!response.ok) {
    throw await apiError(response, "POST", SIP_CREDENTIALS_PATH);
  }
  return (await response.json()) as Credential;
}

export async function updateSipCredential(
  id: string,
  input: CredentialInput,
): Promise<Credential> {
  const path = `${SIP_CREDENTIALS_PATH}/${encodeURIComponent(id)}`;
  const response = await fetch(path, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  if (!response.ok) {
    throw await apiError(response, "PUT", path);
  }
  return (await response.json()) as Credential;
}

export async function deleteSipCredential(id: string): Promise<void> {
  const path = `${SIP_CREDENTIALS_PATH}/${encodeURIComponent(id)}`;
  const response = await fetch(path, { method: "DELETE" });
  if (!response.ok) {
    throw await apiError(response, "DELETE", path);
  }
}

export function useSipCredentials() {
  return useQuery({
    queryKey: SIP_CREDENTIALS_QUERY_KEY,
    queryFn: ({ signal }) => fetchSipCredentials(signal),
    refetchInterval: 3000,
    refetchIntervalInBackground: false,
  });
}

function useInvalidateSipCredentials() {
  const queryClient = useQueryClient();
  return () =>
    queryClient.invalidateQueries({ queryKey: SIP_CREDENTIALS_QUERY_KEY });
}

export function useCreateSipCredential() {
  const invalidate = useInvalidateSipCredentials();
  return useMutation({
    mutationFn: (input: CredentialInput) => createSipCredential(input),
    onSuccess: invalidate,
  });
}

export function useUpdateSipCredential() {
  const invalidate = useInvalidateSipCredentials();
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: CredentialInput }) =>
      updateSipCredential(id, input),
    onSuccess: invalidate,
  });
}

export function useDeleteSipCredential() {
  const invalidate = useInvalidateSipCredentials();
  return useMutation({
    mutationFn: (id: string) => deleteSipCredential(id),
    onSuccess: invalidate,
  });
}
