import { useCallback, useMemo } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import type { ChatChannel, ChatUser } from "@/api/ss/types";
import {
  conversationKey,
  parseConversationKey,
  type ConversationRef,
} from "./types";

// useConversationNavigation owns the open conversation. The selection
// lives in the ?conversation= query param (its conversationKey), so every
// selection change is a browser-history entry and the back button walks
// back through them. A missing or unknown value means nothing is
// selected, and the pane shows its placeholder.
export function useConversationNavigation(
  me: ChatUser | null,
  users: Record<string, ChatUser>,
  channels: ChatChannel[],
): {
  selected: ConversationRef | null;
  select: (ref: ConversationRef) => void;
} {
  const searchParams = useSearchParams();
  const router = useRouter();
  const pathname = usePathname();
  const conversationParam = searchParams.get("conversation");

  const selected: ConversationRef | null = useMemo(() => {
    const ref = parseConversationKey(conversationParam);
    // Only known users other than yourself, reached from a known
    // channel, are openable. While disconnected the listing is empty and
    // nothing is openable; the query param survives, so the conversation
    // reopens once the listing returns.
    if (
      ref === null ||
      me === null ||
      ref.userId === me.id ||
      users[ref.userId] === undefined ||
      !channels.some((c) => c.id === ref.channelId)
    ) {
      return null;
    }
    return ref;
  }, [conversationParam, users, channels, me]);

  const select = useCallback(
    (ref: ConversationRef) => {
      const params = new URLSearchParams(searchParams.toString());
      if (conversationKey(ref) === conversationParam) {
        // Selecting the open conversation again deselects it.
        params.delete("conversation");
      } else {
        params.set("conversation", conversationKey(ref));
      }
      // Push, not replace: each selection change is a history entry, so
      // the browser's back button walks back through them.
      const query = params.toString();
      router.push(query === "" ? pathname : `${pathname}?${query}`, {
        scroll: false,
      });
    },
    [conversationParam, pathname, router, searchParams],
  );

  return { selected, select };
}
