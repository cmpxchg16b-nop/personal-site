import { describe, expect, it } from "vitest";
import { externalSIPCallerIdentity } from "./incomingCallIdentity";

describe("external SIP caller identity", () => {
  it("keeps the complete display name and shows a long AOR without its scheme", () => {
    const displayName = "A very long caller display name that must remain visible";
    expect(
      externalSIPCallerIdentity({
        callId: "call-1",
        remoteName: displayName,
        remoteUri:
          "sip:hsiaofongwei123-99a5e3bd7677@a-very-long-hostname.sip.signalwire.example",
      }),
    ).toEqual({
      name: displayName,
      aor: "hsiaofongwei123-99a5e3bd7677@a-very-long-hostname.sip.signalwire.example",
    });
  });

  it("uses the AOR user as the display name when none was provided", () => {
    expect(
      externalSIPCallerIdentity({
        callId: "call-2",
        remoteName: "  ",
        remoteUri: "sips:alice@example.com",
      }),
    ).toEqual({ name: "alice", aor: "alice@example.com" });
  });

  it("accepts an AOR that already has no scheme", () => {
    expect(
      externalSIPCallerIdentity({
        callId: "call-3",
        remoteUri: "bob@example.com",
      }),
    ).toEqual({ name: "bob", aor: "bob@example.com" });
  });

  it("does not invent an AOR for a name-only caller", () => {
    expect(
      externalSIPCallerIdentity({
        callId: "call-4",
        remoteUri: "visitor-65447",
      }),
    ).toEqual({ name: "visitor-65447" });
    expect(
      externalSIPCallerIdentity({
        callId: "call-5",
        remoteName: "Visitor",
        target: "visitor-65447",
      }),
    ).toEqual({ name: "Visitor" });
  });
});
