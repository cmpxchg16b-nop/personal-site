import { describe, expect, it } from "vitest";
import type { ExternalSIPCall, ExternalSIPMessage } from "@/api/ss/sip";
import {
  externalSIPChatMessages,
  externalSIPConversationForCall,
  externalSIPConversationForMessage,
  externalSIPConversationKey,
  externalSIPConversationMessages,
  externalSIPMessageOnlyConversations,
} from "./externalSipConversation";

const ref = { credentialId: "credential-1", target: "sip:bob@example.com" };

function call(patch: Partial<ExternalSIPCall> = {}): ExternalSIPCall {
  return {
    callId: "call-1",
    credentialId: ref.credentialId,
    target: ref.target,
    kind: "voice",
    direction: "outgoing",
    state: "dialing",
    since: 1,
    updatedAt: 1,
    ...patch,
  };
}

function message(patch: Partial<ExternalSIPMessage> = {}): ExternalSIPMessage {
  return {
    messageId: "message-1",
    credentialId: ref.credentialId,
    target: ref.target,
    text: "hello",
    direction: "outgoing",
    state: "sent",
    createdAt: 10,
    ...patch,
  };
}

describe("external SIP conversations", () => {
  it("resolves dial and incoming-accept targets without requiring call progress", () => {
    expect(externalSIPConversationForCall(call())).toEqual(ref);
    expect(
      externalSIPConversationForCall(
        call({
          direction: "incoming",
          state: "ringing",
          target: undefined,
          remoteUri: ref.target,
        }),
      ),
    ).toEqual(ref);
    expect(
      externalSIPConversationForCall(call({ credentialId: undefined })),
    ).toBeNull();
  });

  it("keeps a raw send target stable when the result adds a normalized URI", () => {
    expect(
      externalSIPConversationForMessage(
        message({ target: "2002", remoteUri: "sip:2002@pbx.example.com" }),
      ),
    ).toEqual({ credentialId: ref.credentialId, target: "2002" });
    expect(
      externalSIPConversationKey({
        credentialId: "a:b",
        target: "sip:bob@example.com",
      }),
    ).toBe("external:a%3Ab:sip%3Abob%40example.com");
  });

  it("merges a raw dial target with its canonical incoming SIP URI", () => {
    const selected = { credentialId: ref.credentialId, target: "2002" };
    const history = [
      message({
        target: "2002",
        localUri: "sip:1001@127.0.0.1",
        createdAt: 1,
      }),
      message({
        messageId: "incoming-canonical",
        target: "sip:2002@127.0.0.1",
        direction: "incoming",
        state: "received",
        localUri: "sip:1001@127.0.0.1",
        createdAt: 2,
      }),
      message({
        messageId: "other-host",
        target: "sip:2002@other.example.com",
        direction: "incoming",
        state: "received",
        localUri: "sip:1001@127.0.0.1",
        createdAt: 3,
      }),
    ];
    expect(
      externalSIPChatMessages(history, selected, "self", "peer").map(
        ({ id }) => id,
      ),
    ).toEqual(["message-1", "incoming-canonical"]);
  });

  it("discovers MESSAGE-only peers once and omits peers already in call history", () => {
    const messageOnly = message({
      messageId: "new-peer",
      target: "sip:carol@example.com",
      direction: "incoming",
      state: "received",
      localUri: "sip:1001@example.com",
      createdAt: 20,
    });
    const olderDuplicate = {
      ...messageOnly,
      messageId: "older-duplicate",
      createdAt: 10,
    };
    const withCall = message({ createdAt: 30 });
    expect(
      externalSIPMessageOnlyConversations({
        calls: { "call-1": call() },
        messages: [olderDuplicate, withCall, messageOnly],
      }),
    ).toEqual([
      {
        ref: {
          credentialId: ref.credentialId,
          target: "sip:carol@example.com",
        },
        latestMessage: messageOnly,
      },
    ]);
  });

  it("links one call-log message to live state and interleaves SIP MESSAGEs", () => {
    const state = {
      calls: {
        "call-1": call({ state: "ringing", kind: "video", since: 5_000 }),
      },
      messages: [message({ createdAt: 6 })],
    };
    let rendered = externalSIPConversationMessages(state, ref, "self", "peer");
    expect(rendered.map(({ id }) => id)).toEqual([
      "external-call:call-1",
      "message-1",
    ]);
    expect(rendered[0]).toMatchObject({
      type: "phone-call",
      authorId: "self",
      kind: "video",
      phoneStatus: "inviting",
      timestamp: 5,
    });

    state.calls["call-1"] = call({
      state: "accepted",
      kind: "video",
      since: 5_000,
      updatedAt: 7_000,
    });
    rendered = externalSIPConversationMessages(state, ref, "self", "peer");
    expect(rendered[0]).toMatchObject({
      id: "external-call:call-1",
      phoneStatus: "accepted",
    });
    state.calls["call-1"] = call({
      state: "ended",
      kind: "video",
      since: 5_000,
      updatedAt: 8_000,
    });
    rendered = externalSIPConversationMessages(state, ref, "self", "peer");
    expect(rendered[0]).toMatchObject({
      id: "external-call:call-1",
      phoneStatus: "ended",
    });
    expect(rendered[1]).toMatchObject({ id: "message-1", content: "hello" });
  });

  it("attributes incoming call logs to the peer and preserves failures", () => {
    const state = {
      calls: {
        incoming: call({
          callId: "incoming",
          direction: "incoming",
          state: "accepted",
        }),
        failed: call({ callId: "failed", state: "failed" }),
      },
      messages: [],
    };
    const rendered = externalSIPConversationMessages(
      state,
      ref,
      "self",
      "peer",
    );
    expect(rendered).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ callId: "incoming", authorId: "peer" }),
        expect.objectContaining({ callId: "failed", phoneStatus: "failed" }),
      ]),
    );
  });

  it("isolates peers and credentials, sorts history, and maps delivery state", () => {
    const history = [
      message({ messageId: "later", text: "second", createdAt: 20 }),
      message({
        messageId: "failed",
        text: "try again",
        state: "failed",
        reason: "503 unavailable",
        createdAt: 15,
      }),
      message({
        messageId: "incoming",
        text: "reply",
        direction: "incoming",
        state: "received",
        createdAt: 25,
      }),
      message({ messageId: "other-peer", target: "sip:alice@example.com" }),
      message({ messageId: "other-credential", credentialId: "credential-2" }),
    ];

    const rendered = externalSIPChatMessages(history, ref, "self", "peer");
    expect(rendered.map(({ id }) => id)).toEqual([
      "failed",
      "later",
      "incoming",
    ]);
    expect(rendered[0]).toMatchObject({
      content: "try again",
      delivery: "failed",
      deliveryReason: "503 unavailable",
    });
    expect(rendered[1]).toMatchObject({
      authorId: "self",
      delivery: "sent",
    });
    expect(rendered[2]).toMatchObject({ authorId: "peer", content: "reply" });
    expect(rendered[2]).not.toHaveProperty("delivery");
  });
});
