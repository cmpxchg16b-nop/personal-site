import { useMemo } from "react";
import type { DCMsgs } from "@/api/ss/datachannel";
import type { ChatChannel, ChatUser } from "@/api/ss/types";

// useChatUsers derives the user directory of the chat page: all known
// users by id, including the current user — the live channel members,
// plus ourselves once registered, plus every data-channel sender, so a
// message keeps rendering even after its author drops out of the listing
// (MessageList skips messages by unknown authors).
export function useChatUsers(
  me: ChatUser | null,
  channels: ChatChannel[],
  dcMsgs: DCMsgs,
): Record<string, ChatUser> {
  return useMemo(() => {
    const byId: Record<string, ChatUser> = {};
    if (me !== null) {
      byId[me.id] = me;
      // Messages name their author by subscriber id
      // (DCMsg.fromSubscriberId), which is not me's app-level user id:
      // index ourselves under it too, so our own echoed messages resolve
      // to our username (and to us for isOwn).
      if (me.subscriberId !== undefined) {
        byId[me.subscriberId] = me;
      }
    }
    for (const channel of channels) {
      for (const member of channel.members) {
        byId[member.id] = member;
      }
    }
    for (const bySender of Object.values(dcMsgs)) {
      for (const [senderId, msgs] of Object.entries(bySender)) {
        if (byId[senderId] === undefined && msgs.length > 0) {
          // The sender is no longer in the listing; the id doubles as
          // the display name (it is all that is known).
          byId[senderId] = {
            id: senderId,
            name: senderId,
            online: false,
            channelId: msgs[0].channelId,
            subscriberId: senderId,
          };
        }
      }
    }
    return byId;
  }, [me, channels, dcMsgs]);
}
