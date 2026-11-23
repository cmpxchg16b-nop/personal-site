import type { Credential } from "@/hooks/useSipCredentials";

/** Only enabled identities with a currently valid registration lease may dial. */
export function eligibleSipCredentials(
  credentials: readonly Credential[] | undefined,
  now = Date.now(),
): Credential[] {
  return (credentials ?? []).filter((credential) => {
    if (!credential.enabled) return false;
    const expiresAt = credential.registration.expires_at;
    if (expiresAt === undefined) return false;
    const expiry = Date.parse(expiresAt);
    return Number.isFinite(expiry) && expiry > now;
  });
}

/** Preserve a valid selection, otherwise choose the first eligible identity. */
export function reconcileSipCredentialSelection(
  selectedId: string,
  eligible: readonly Credential[],
): string {
  return eligible.some((credential) => credential.id === selectedId)
    ? selectedId
    : (eligible[0]?.id ?? "");
}
