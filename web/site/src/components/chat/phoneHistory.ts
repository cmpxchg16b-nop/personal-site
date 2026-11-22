import type { ChatUser } from "@/api/ss/types";
import {
  parseConversationKey,
  type ChatMessage,
  type ConversationRef,
  type PhoneCallMessage,
} from "./types";

export type PhoneHistoryEntry = {
  ref: ConversationRef;
  message: PhoneCallMessage;
  user: ChatUser;
  outgoing: boolean;
};

// collectPhoneHistory reuses the conversation's call-log messages rather than
// creating a second, potentially divergent history for the Phone tab.
export function collectPhoneHistory(
  messages: Record<string, ChatMessage[]>,
  users: Record<string, ChatUser>,
  currentUserId: string,
): PhoneHistoryEntry[] {
  const self = users[currentUserId];
  const entries: PhoneHistoryEntry[] = [];

  for (const [key, conversationMessages] of Object.entries(messages)) {
    const ref = parseConversationKey(key);
    const user = ref === null ? undefined : users[ref.userId];
    if (ref === null || user === undefined) continue;

    for (const message of conversationMessages) {
      if (message.type !== "phone-call") continue;
      entries.push({
        ref,
        message,
        user,
        outgoing:
          message.authorId === currentUserId ||
          (self?.subscriberId !== undefined &&
            message.authorId === self.subscriberId),
      });
    }
  }

  return entries.sort(
    (a, b) =>
      b.message.timestamp - a.message.timestamp ||
      b.message.id.localeCompare(a.message.id),
  );
}

// The current signalling model has no separate PSTN/SIP number field. Until
// one exists, the peer's subscriber and user ids are the searchable numbers.
export function filterPhoneHistory(
  entries: PhoneHistoryEntry[],
  query: string,
): PhoneHistoryEntry[] {
  const normalized = query.trim().toLowerCase();
  if (normalized === "") return entries;
  return entries.filter(({ user }) =>
    [user.name, user.id, user.subscriberId].some(
      (value) => value?.toLowerCase().includes(normalized) ?? false,
    ),
  );
}
