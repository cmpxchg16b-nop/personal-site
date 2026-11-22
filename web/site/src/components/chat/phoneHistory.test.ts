import { describe, expect, it } from "vitest";
import type { ChatUser } from "@/api/ss/types";
import { collectPhoneHistory, filterPhoneHistory } from "./phoneHistory";
import type { ChatMessage, PhoneCallMessage } from "./types";

const self: ChatUser = {
  id: "self",
  subscriberId: "self-subscription",
  name: "Me",
  online: true,
  visible: true,
};
const peer: ChatUser = {
  id: "peer",
  name: "Peer",
  online: true,
  visible: true,
};

function call(
  id: string,
  authorId: string,
  timestamp: number,
): PhoneCallMessage {
  return {
    type: "phone-call",
    id,
    authorId,
    callId: `call-${id}`,
    kind: "voice",
    phoneStatus: "ended",
    timestamp,
  };
}

describe("collectPhoneHistory", () => {
  it("collects only known conversations' calls, newest first", () => {
    const text: ChatMessage = {
      type: "text-chat",
      id: "text",
      authorId: peer.id,
      content: "hello",
      timestamp: 40,
    };
    const messages: Record<string, ChatMessage[]> = {
      "dm:main:peer": [
        call("old", peer.id, 10),
        text,
        call("new", self.id, 30),
      ],
      "dm:main:unknown": [call("unknown", "unknown", 50)],
      invalid: [call("invalid", peer.id, 60)],
    };

    const history = collectPhoneHistory(messages, { self, peer }, self.id);
    expect(history.map((entry) => entry.message.id)).toEqual(["new", "old"]);
    expect(history[0]).toMatchObject({
      ref: { kind: "dm", channelId: "main", userId: peer.id },
      user: peer,
      outgoing: true,
    });
    expect(history[1].outgoing).toBe(false);
  });

  it("recognizes an echoed own call by the current subscriber id", () => {
    const history = collectPhoneHistory(
      { "dm:main:peer": [call("echo", self.subscriberId!, 10)] },
      { self, peer },
      self.id,
    );
    expect(history).toHaveLength(1);
    expect(history[0].outgoing).toBe(true);
  });

  it("filters case-insensitively by display name, user id or subscriber id", () => {
    const entries = collectPhoneHistory(
      { "dm:main:peer": [call("one", peer.id, 10)] },
      { self, peer: { ...peer, subscriberId: "10086" } },
      self.id,
    );
    expect(filterPhoneHistory(entries, "pee")).toHaveLength(1);
    expect(filterPhoneHistory(entries, "PEER")).toHaveLength(1);
    expect(filterPhoneHistory(entries, "1008")).toHaveLength(1);
    expect(filterPhoneHistory(entries, "999")).toHaveLength(0);
    expect(filterPhoneHistory(entries, "  ")).toBe(entries);
  });

  it("uses message id as a deterministic tie breaker", () => {
    const history = collectPhoneHistory(
      {
        "dm:main:peer": [call("a", peer.id, 10), call("b", peer.id, 10)],
      },
      { self, peer },
      self.id,
    );
    expect(history.map((entry) => entry.message.id)).toEqual(["b", "a"]);
  });
});
