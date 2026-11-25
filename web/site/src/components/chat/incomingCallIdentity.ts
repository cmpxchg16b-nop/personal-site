import type { ExternalSIPCall } from "@/api/ss/sip";

export type IncomingCallerIdentity = {
  name: string;
  aor?: string;
};

function sipAor(remoteUri: string): { user: string; value: string } | null {
  const match = /^(?:sips?:)?([^@\s]+)@([^\s]+)$/i.exec(remoteUri);
  if (match === null) return null;
  return { user: match[1], value: `${match[1]}@${match[2]}` };
}

/** Resolves presentation only; remoteUri remains the canonical SIP identity. */
export function externalSIPCallerIdentity(
  call: Pick<
    ExternalSIPCall,
    "callId" | "remoteName" | "remoteUri" | "target"
  >,
): IncomingCallerIdentity {
  const remoteUri = call.remoteUri?.trim() ?? "";
  const parsedAor = sipAor(remoteUri);
  const displayName = call.remoteName?.trim();
  return {
    name:
      displayName ||
      parsedAor?.user ||
      remoteUri ||
      call.target?.trim() ||
      call.callId,
    ...(parsedAor === null ? {} : { aor: parsedAor.value }),
  };
}
