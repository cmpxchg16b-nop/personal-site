import type {
  ExternalSIPCall,
  ExternalSIPMessage,
  ExternalSIPState,
} from "@/api/ss/sip";
import type { ChatMessage, PhoneCallMessage, TextChatMessage } from "./types";

export interface ExternalSIPConversationRef {
  credentialId: string;
  target: string;
}

function nonEmpty(...values: (string | undefined)[]): string {
  for (const value of values) {
    const trimmed = value?.trim() ?? "";
    if (trimmed !== "") return trimmed;
  }
  return "";
}

export function externalSIPConversationKey(
  ref: ExternalSIPConversationRef,
): string {
  return `external:${encodeURIComponent(ref.credentialId)}:${encodeURIComponent(ref.target)}`;
}

export function externalSIPConversationForCall(
  call: ExternalSIPCall,
): ExternalSIPConversationRef | null {
  const credentialId = call.credentialId?.trim() ?? "";
  const target = nonEmpty(call.target, call.remoteUri);
  return credentialId === "" || target === "" ? null : { credentialId, target };
}

export function externalSIPConversationForMessage(
  message: ExternalSIPMessage,
): ExternalSIPConversationRef | null {
  const credentialId = message.credentialId?.trim() ?? "";
  // Send results retain the entered target and add a normalized remoteUri.
  // Prefer target so the pending/sent/failed bubble stays in the same chat.
  const target = nonEmpty(message.target, message.remoteUri);
  return credentialId === "" || target === "" ? null : { credentialId, target };
}

function withoutPort(host: string): string {
  if (host.startsWith("[")) {
    const end = host.indexOf("]");
    return end < 0 ? host : host.slice(0, end + 1);
  }
  return host.split(":", 1)[0];
}

function hostFromURI(uri: string | undefined): string {
  if (uri === undefined) return "";
  const match = /^sips?:[^@]*@([^;?]+)/i.exec(uri.trim());
  return match === null ? "" : withoutPort(match[1]);
}

function comparableTarget(target: string, localUri?: string): string {
  const original = target.trim();
  let uri = original;
  if (!/^sips?:/i.test(uri)) {
    const colon = uri.indexOf(":");
    const at = uri.indexOf("@");
    if (colon >= 0 && (at < 0 || colon < at)) return original;
    uri = `sip:${uri}`;
  }
  const body = uri.slice(uri.indexOf(":") + 1);
  const at = body.lastIndexOf("@");
  const user = (at < 0 ? body : body.slice(0, at)).split(/[;?]/, 1)[0];
  const host = withoutPort(
    at < 0 ? hostFromURI(localUri) : body.slice(at + 1).split(/[;?]/, 1)[0],
  );
  return user === "" || host === ""
    ? original
    : `sip:${user}@${host.toLowerCase()}`;
}

export function sameExternalSIPConversation(
  a: ExternalSIPConversationRef,
  b: ExternalSIPConversationRef,
  localUri?: string,
): boolean {
  return (
    a.credentialId === b.credentialId &&
    comparableTarget(a.target, localUri) ===
      comparableTarget(b.target, localUri)
  );
}

function phoneStatus(call: ExternalSIPCall): PhoneCallMessage["phoneStatus"] {
  switch (call.state) {
    case "accepted":
      return "accepted";
    case "ended":
    case "rejected":
    case "cancelled":
    case "failed":
      return call.state;
    default:
      return "inviting";
  }
}

export interface ExternalSIPMessageConversation {
  ref: ExternalSIPConversationRef;
  latestMessage: ExternalSIPMessage;
}

export function externalSIPMessageOnlyConversations(
  state: ExternalSIPState,
): ExternalSIPMessageConversation[] {
  const out: ExternalSIPMessageConversation[] = [];
  const calls = Object.values(state.calls);
  const newest = [...state.messages].sort(
    (a, b) =>
      b.createdAt - a.createdAt || b.messageId.localeCompare(a.messageId),
  );
  for (const message of newest) {
    const ref = externalSIPConversationForMessage(message);
    if (ref === null) continue;
    if (
      out.some((entry) =>
        sameExternalSIPConversation(entry.ref, ref, message.localUri),
      )
    ) {
      continue;
    }
    const hasCall = calls.some((call) => {
      const callRef = externalSIPConversationForCall(call);
      return (
        callRef !== null &&
        sameExternalSIPConversation(
          callRef,
          ref,
          call.localUri ?? message.localUri,
        )
      );
    });
    if (!hasCall) out.push({ ref, latestMessage: message });
  }
  return out;
}

export function externalSIPConversationMessages(
  state: ExternalSIPState,
  ref: ExternalSIPConversationRef,
  selfAuthorId: string,
  peerAuthorId: string,
): ChatMessage[] {
  const calls = Object.values(state.calls)
    .filter((call) => {
      const conversation = externalSIPConversationForCall(call);
      return (
        conversation !== null &&
        sameExternalSIPConversation(conversation, ref, call.localUri)
      );
    })
    .map((call): PhoneCallMessage => ({
      type: "phone-call",
      id: `external-call:${call.callId}`,
      authorId: call.direction === "outgoing" ? selfAuthorId : peerAuthorId,
      callId: call.callId,
      kind: call.kind,
      phoneStatus: phoneStatus(call),
      timestamp: call.since / 1000,
    }));
  return [
    ...calls,
    ...externalSIPChatMessages(state.messages, ref, selfAuthorId, peerAuthorId),
  ].sort((a, b) => a.timestamp - b.timestamp || a.id.localeCompare(b.id));
}

export function externalSIPChatMessages(
  messages: ExternalSIPMessage[],
  ref: ExternalSIPConversationRef,
  selfAuthorId: string,
  peerAuthorId: string,
): TextChatMessage[] {
  return messages
    .filter((message) => {
      const conversation = externalSIPConversationForMessage(message);
      return (
        conversation !== null &&
        sameExternalSIPConversation(conversation, ref, message.localUri)
      );
    })
    .sort(
      (a, b) =>
        a.createdAt - b.createdAt || a.messageId.localeCompare(b.messageId),
    )
    .map((message) => ({
      type: "text-chat",
      id: message.messageId,
      authorId: message.direction === "outgoing" ? selfAuthorId : peerAuthorId,
      content: message.text,
      timestamp: message.createdAt,
      ...(message.direction === "outgoing"
        ? { delivery: message.state, deliveryReason: message.reason }
        : {}),
    }));
}
