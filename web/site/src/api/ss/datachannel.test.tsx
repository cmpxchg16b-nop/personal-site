import { beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  runtime: { current: null as unknown },
}));

vi.mock("react", () => ({
  useCallback: <T,>(fn: T) => fn,
  useEffect: (effect: () => void) => {
    (mocks.runtime.current as HookRuntime).effects.push(effect);
  },
  useState: <T,>(initial: T) =>
    (mocks.runtime.current as HookRuntime).state(initial),
}));

import {
  DC_MSG_MIME_PLAINTEXT,
  newChatControlDCMsg,
  newFileTransferDCMsg,
  newSipDCMsg,
  useDataChannel,
  type DCMsg,
} from "./datachannel";
import type { PeerSessions } from "./peersessions";
import {
  WELLKNOWN_CH_ID_MAIN,
  type ChatUser,
  type ClientToClientCallEv,
  type SignallingEvent,
} from "./types";

const channelId = WELLKNOWN_CH_ID_MAIN;
const me: ChatUser = {
  id: "1000",
  name: "browser",
  online: true,
  visible: true,
  channelId,
  subscriberId: "1000",
};
const peer = "2000";

interface StateSlot {
  value: unknown;
}

class HookRuntime {
  private slots: StateSlot[] = [];
  private index = 0;
  effects: Array<() => void> = [];

  state<T>(initial: T): [T, (next: T | ((prev: T) => T)) => void] {
    const index = this.index++;
    if (this.slots[index] === undefined) {
      this.slots[index] = { value: initial };
    }
    const slot = this.slots[index];
    return [
      slot.value as T,
      (next) => {
        slot.value =
          typeof next === "function"
            ? (next as (prev: T) => T)(slot.value as T)
            : next;
      },
    ];
  }

  render(sessions: PeerSessions, runEffects: boolean) {
    this.index = 0;
    this.effects = [];
    // Deliberate direct invocation under the local hook runtime.
    // eslint-disable-next-line react-hooks/rules-of-hooks
    const result = useDataChannel(me, sessions);
    if (runEffects) {
      for (const effect of this.effects) effect();
    }
    return result;
  }
}

class MockDataChannel {
  readonly readyState = "open";
  onmessage: ((event: { data: unknown }) => void) | null = null;
  readonly send = vi.fn();
}

class MockSessions {
  readonly dc = new MockDataChannel();
  readonly sendCall = vi.fn((): Promise<void> => Promise.resolve());
  private callHandler: ((event: SignallingEvent) => void) | null = null;

  readonly object: PeerSessions = {
    getChannel: () => this.dc as unknown as RTCDataChannel,
    whenOpenChannel: () =>
      Promise.resolve(this.dc as unknown as RTCDataChannel),
    subscribeChannel: (_label, cb) => {
      cb(channelId, peer, this.dc as unknown as RTCDataChannel);
      return () => {};
    },
    subscribeCall: (cb) => {
      this.callHandler = cb;
      return () => {};
    },
    sendCall: this.sendCall,
    subscribeReset: () => () => {},
    addTrack: () => null,
    removeTrack: () => {},
    resolveSender: () => null,
    subscribeTracks: () => () => {},
  };

  receiveCall(
    call: ClientToClientCallEv,
    outerFrom = call.fromSubscriberId,
  ): void {
    this.callHandler?.({
      from: {},
      to: {},
      msgId: crypto.randomUUID(),
      c2CEv: {
        channelId: call.channelId,
        fromSubscriber: outerFrom,
        toSubscriber: call.toSubscriberId,
        call,
      },
    });
  }
}

function invite(from = peer, to = me.subscriberId!): DCMsg {
  return newSipDCMsg(channelId, from, to, {
    callId: "call-1",
    method: "INVITE",
    "X-Media": "voice",
    "X-Call-Status": "inviting",
  });
}

function asCall(msg: DCMsg): ClientToClientCallEv {
  return msg;
}

async function flushMicrotasks() {
  for (let i = 0; i < 10; i++) await Promise.resolve();
}

let runtime: HookRuntime;
let sessions: MockSessions;

beforeEach(() => {
  runtime = new HookRuntime();
  mocks.runtime.current = runtime;
  sessions = new MockSessions();
});

describe("useDataChannel call signalling", () => {
  it("sends SIP and call-status amendments only over WebSocket", async () => {
    let result = runtime.render(sessions.object, true);
    const outgoing = invite(me.subscriberId!, peer);
    result.sendTo(outgoing);
    await flushMicrotasks();

    expect(sessions.sendCall).toHaveBeenCalledWith(channelId, peer, outgoing);
    expect(sessions.dc.send).not.toHaveBeenCalled();
    result = runtime.render(sessions.object, false);
    expect(result.dcMsgs[channelId][me.subscriberId!]).toEqual([
      { ...outgoing, echo: true },
    ]);

    result.sendTo(
      newChatControlDCMsg(channelId, me.subscriberId!, peer, {
        subtype: "amend",
        targetMessageId: outgoing.msgId,
        sip: {
          callId: "call-1",
          method: "INVITE",
          "X-Media": "voice",
          "X-Call-Status": "accepted",
        },
      }),
    );
    await flushMicrotasks();

    result = runtime.render(sessions.object, false);
    expect(sessions.sendCall).toHaveBeenCalledTimes(2);
    expect(sessions.dc.send).not.toHaveBeenCalled();
    expect(result.dcMsgs[channelId][me.subscriberId!]).toHaveLength(1);
    expect(
      result.dcMsgs[channelId][me.subscriberId!][0].sip?.["X-Call-Status"],
    ).toBe("accepted");
  });

  it("accepts addressed WebSocket calls and rejects mismatched addressing", () => {
    let result = runtime.render(sessions.object, true);
    const incoming = invite();
    sessions.receiveCall(asCall(incoming));
    result = runtime.render(sessions.object, false);
    expect(result.dcMsgs[channelId][peer]).toEqual([incoming]);
    expect(sessions.dc.send).not.toHaveBeenCalled();

    const forged = {
      ...invite(),
      msgId: crypto.randomUUID(),
      fromSubscriberId: "someone-else",
    };
    sessions.receiveCall(asCall(forged), peer);
    result = runtime.render(sessions.object, false);
    expect(result.dcMsgs[channelId][peer]).toEqual([incoming]);
    expect(result.dcMsgs[channelId]["someone-else"]).toBeUndefined();
  });

  it("drops legacy dcmsg call frames but keeps chat echo semantics", () => {
    let result = runtime.render(sessions.object, true);
    const legacySip = invite();
    sessions.dc.onmessage?.({ data: JSON.stringify(legacySip) });
    const legacyAmend = newChatControlDCMsg(channelId, peer, me.subscriberId!, {
      subtype: "amend",
      targetMessageId: legacySip.msgId,
      sip: {
        callId: "call-1",
        method: "INVITE",
        "X-Call-Status": "ended",
      },
    });
    sessions.dc.onmessage?.({ data: JSON.stringify(legacyAmend) });
    result = runtime.render(sessions.object, false);
    expect(result.dcMsgs).toEqual({});
    expect(sessions.dc.send).not.toHaveBeenCalled();

    const chat: DCMsg = {
      ...invite(),
      msgId: crypto.randomUUID(),
      mimeType: DC_MSG_MIME_PLAINTEXT,
      plaintext: "hello",
      sip: undefined,
    };
    sessions.dc.onmessage?.({ data: JSON.stringify(chat) });
    result = runtime.render(sessions.object, false);
    expect(result.dcMsgs[channelId][peer]).toEqual([chat]);
    expect(sessions.dc.send).toHaveBeenCalledTimes(1);
    expect(JSON.parse(sessions.dc.send.mock.calls[0][0])).toMatchObject({
      msgId: chat.msgId,
      echo: true,
    });

    result.sendTo(
      newChatControlDCMsg(channelId, me.subscriberId!, peer, {
        subtype: "amend",
        targetMessageId: chat.msgId,
        text: "edited",
      }),
    );
    result.sendTo(
      newFileTransferDCMsg(channelId, me.subscriberId!, peer, {
        fileId: crypto.randomUUID(),
        kind: "file",
        filename: "data.bin",
        fileMIMEType: "application/octet-stream",
        fileSizeTotalBytes: 1,
        fileSizeTransferred: 0,
        fileTransferStatus: "pending",
      }),
    );
    expect(sessions.sendCall).not.toHaveBeenCalled();
    expect(sessions.dc.send).toHaveBeenCalledTimes(3);
  });
});
