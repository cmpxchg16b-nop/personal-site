import type { ChatChannel, ChatUser } from "@/api/ss/types";
import type { ChatMessage } from "./types";

// Stage 1 mock data: the entire chat "backend" lives in this file until a
// real API exists. Timestamps are computed relative to module load so the
// relative-time labels ("2 hours ago") always look fresh.

// CURRENT_USER_ID is the local chatter's identity. There is no login system
// yet, so this is a fixed placeholder; its display name comes from this
// record, shown as-is.
export const CURRENT_USER_ID = "me";

export const chatUsers: Record<string, ChatUser> = {
  [CURRENT_USER_ID]: { id: CURRENT_USER_ID, name: "visitor", online: true },
  qiuxin: { id: "qiuxin", name: "秋信", online: true },
  afu: { id: "afu", name: "阿福", online: true },
  mint: { id: "mint", name: "薄荷", online: true },
  laobai: { id: "laobai", name: "老白", online: false },
  qiyan: { id: "qiyan", name: "七言", online: false },
};

const user = (id: string): ChatUser => chatUsers[id];

export const chatChannels: ChatChannel[] = [
  {
    id: "general",
    name: "main",
    members: [user("qiuxin"), user("afu"), user("mint")],
  },
];

const now = Date.now();
const minutesAgo = (minutes: number): number =>
  Math.floor((now - minutes * 60_000) / 1000);

// mockMessages maps conversation key (see conversationKey in types.ts) to
// the conversation's messages, oldest first. Only DMs have messages —
// channels are grouping-only at this moment.
export const mockMessages: Record<string, ChatMessage[]> = {
  "dm:general:afu": [
    {
      id: "d1",
      authorId: "afu",
      content: "在吗？",
      timestamp: minutesAgo(21),
    },
    {
      id: "d2",
      authorId: "afu",
      content: "你博客的 RSS 好像打不开了",
      timestamp: minutesAgo(20),
    },
  ],
  "dm:general:mint": [
    {
      id: "d3",
      authorId: "mint",
      content: "下次线下聚会记得带上你的相机呀",
      timestamp: minutesAgo(26 * 60),
    },
  ],
};

// mockUnread maps conversation key to its unseen-message count. Opening a
// conversation clears its count; a mock reply landing while you look
// elsewhere bumps it again.
export const mockUnread: Record<string, number> = {
  "dm:general:afu": 1,
};

// mockReplies is the canned pool the fake chat partner answers with, a few
// seconds after you send something (see chat/page.tsx).
export const mockReplies: string[] = [
  "哈哈哈哈哈",
  "+1",
  "有道理",
  "让我看看 🤔",
  "确实",
  "可以的",
  "回头细说",
  "展开讲讲？",
];
