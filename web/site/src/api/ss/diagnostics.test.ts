import { afterEach, describe, expect, it, vi } from "vitest";

import {
  createPeerDiagnostics,
  sessionDescriptionDtlsFingerprints,
  summarizeRtcStats,
  summarizeSessionDescription,
} from "./diagnostics";

const identity = {
  sessionId: "session-1",
  channelId: "main",
  self: "alice",
  peer: "bob",
  polite: true,
};

function mockPeerConnection(getStats = vi.fn()) {
  return {
    connectionState: "disconnected",
    iceConnectionState: "connected",
    signalingState: "stable",
    iceGatheringState: "complete",
    getStats,
  } as unknown as RTCPeerConnection;
}

function statsReport(): RTCStatsReport {
  return new Map([
    [
      "local",
      {
        id: "local",
        type: "local-candidate",
        candidateType: "host",
        protocol: "udp",
        address: "192.0.2.10",
        port: 5000,
      },
    ],
    [
      "remote",
      {
        id: "remote",
        type: "remote-candidate",
        candidateType: "srflx",
        protocol: "udp",
        address: "198.51.100.20",
        port: 6000,
      },
    ],
    [
      "pair",
      {
        id: "pair",
        type: "candidate-pair",
        state: "succeeded",
        nominated: true,
        selected: true,
        bytesSent: 1200,
        bytesReceived: 3400,
        currentRoundTripTime: 0.025,
        localCandidateId: "local",
        remoteCandidateId: "remote",
      },
    ],
    [
      "transport",
      {
        id: "transport",
        type: "transport",
        dtlsState: "failed",
        iceState: "connected",
        selectedCandidatePairId: "pair",
        selectedCandidatePairChanges: 8,
      },
    ],
    [
      "inbound",
      {
        id: "inbound",
        type: "inbound-rtp",
        kind: "audio",
        ssrc: 42,
        packetsReceived: 100,
        bytesReceived: 12_000,
        packetsLost: 3,
        jitter: 0.004,
      },
    ],
    [
      "outbound",
      {
        id: "outbound",
        type: "outbound-rtp",
        kind: "audio",
        ssrc: 43,
        packetsSent: 90,
        bytesSent: 10_000,
      },
    ],
  ]) as unknown as RTCStatsReport;
}

afterEach(() => vi.restoreAllMocks());

describe("RTC diagnostics", () => {
  it("summarizes SDP roles and identities without exposing wire secrets", () => {
    const description: RTCSessionDescriptionInit = {
      type: "offer",
      sdp: [
        "v=0",
        "a=ice-ufrag:session-ufrag-secret",
        "a=ice-pwd:session-pwd-secret",
        "m=audio 9 UDP/TLS/RTP/SAVPF 111",
        "a=mid:0",
        "a=setup:actpass",
        "a=fingerprint:sha-256 AA:BB:CC",
        "a=ice-ufrag:media-ufrag-secret",
      ].join("\r\n"),
    };
    const summary = summarizeSessionDescription(description);
    expect(summary).toMatchObject({
      type: "offer",
      sections: [{ mid: "?" }, { mid: "0", setup: "actpass" }],
    });
    expect(summary).toEqual(summarizeSessionDescription(description));
    expect(sessionDescriptionDtlsFingerprints(description)).toEqual([
      "sha-256 aa:bb:cc",
    ]);
    const serialized = JSON.stringify(summary);
    expect(serialized).not.toContain("session-ufrag-secret");
    expect(serialized).not.toContain("session-pwd-secret");
    expect(serialized).not.toContain("media-ufrag-secret");
    expect(serialized).not.toContain("AA:BB:CC");
    expect(serialized).toContain("iceUfragHash");
    expect(serialized).toContain("icePwdHash");
    expect(serialized).toContain("dtlsFingerprintHash");
  });

  it("summarizes selected pairs and RTP while excluding candidate addresses", () => {
    const summary = summarizeRtcStats(statsReport());
    expect(summary.candidatePairs).toEqual([
      expect.objectContaining({
        id: "pair",
        state: "succeeded",
        selected: true,
        bytesSent: 1200,
        bytesReceived: 3400,
        local: { candidateType: "host", protocol: "udp" },
        remote: { candidateType: "srflx", protocol: "udp" },
      }),
    ]);
    expect(summary.transports).toEqual([
      {
        id: "transport",
        dtlsState: "failed",
        iceState: "connected",
        selectedCandidatePairId: "pair",
        selectedCandidatePairChanges: 8,
      },
    ]);
    expect(summary.rtp).toEqual([
      expect.objectContaining({
        type: "inbound-rtp",
        packetsReceived: 100,
        bytesReceived: 12_000,
        packetsLost: 3,
      }),
      expect.objectContaining({
        type: "outbound-rtp",
        packetsSent: 90,
        bytesSent: 10_000,
      }),
    ]);
    expect(JSON.stringify(summary)).not.toContain("192.0.2.10");
    expect(JSON.stringify(summary)).not.toContain("198.51.100.20");
  });

  it("emits one structured stats event with session and transport context", async () => {
    const info = vi.spyOn(console, "info").mockImplementation(() => {});
    const getStats = vi.fn(async () => statsReport());
    const diagnostics = createPeerDiagnostics(
      mockPeerConnection(getStats),
      identity,
    );
    diagnostics.captureStats("test");
    await vi.waitFor(() => expect(info).toHaveBeenCalledTimes(1));
    expect(info).toHaveBeenCalledWith("[rtc alice→bob]", {
      at: expect.any(String),
      ...identity,
      event: "stats",
      connectionState: "disconnected",
      iceConnectionState: "connected",
      signalingState: "stable",
      iceGatheringState: "complete",
      reason: "test",
      sequence: 1,
      candidatePairs: [expect.objectContaining({ id: "pair" })],
      transports: [
        expect.objectContaining({
          dtlsState: "failed",
          iceState: "connected",
        }),
      ],
      rtp: [
        expect.objectContaining({ type: "inbound-rtp" }),
        expect.objectContaining({ type: "outbound-rtp" }),
      ],
    });
    diagnostics.close();
  });

  it("ignores a getStats result that arrives after session teardown", async () => {
    const info = vi.spyOn(console, "info").mockImplementation(() => {});
    let resolveStats!: (report: RTCStatsReport) => void;
    const getStats = vi.fn(
      () =>
        new Promise<RTCStatsReport>((resolve) => {
          resolveStats = resolve;
        }),
    );
    const diagnostics = createPeerDiagnostics(
      mockPeerConnection(getStats),
      identity,
    );
    diagnostics.captureStats("late");
    diagnostics.close();
    resolveStats(statsReport());
    await Promise.resolve();
    await Promise.resolve();
    expect(info).not.toHaveBeenCalled();
  });
});
