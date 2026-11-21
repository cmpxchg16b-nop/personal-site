"use client";

import type { ChannelId, SubscriberId } from "./types";

export type RtcDiagnosticLevel = "debug" | "info" | "warn" | "error";

export interface PeerDiagnostics {
  log(
    event: string,
    details?: Record<string, unknown>,
    level?: RtcDiagnosticLevel,
  ): void;
  captureStats(reason: string): void;
  close(): void;
}

interface PeerDiagnosticIdentity {
  sessionId: string;
  channelId: ChannelId;
  self: SubscriberId;
  peer: SubscriberId;
  polite: boolean;
}

// A stable, non-cryptographic digest lets one console timeline correlate SDP
// identities without exposing the ICE credentials or the DTLS fingerprint.
function digest(value: string): string {
  let hash = 0x811c9dc5;
  for (let i = 0; i < value.length; i++) {
    hash = Math.imul(hash ^ value.charCodeAt(i), 0x01000193);
  }
  return (hash >>> 0).toString(16).padStart(8, "0");
}

/** Returns normalized DTLS identities for local stale-generation checks only. */
export function sessionDescriptionDtlsFingerprints(
  description: RTCSessionDescriptionInit | null | undefined,
): string[] {
  if (!description?.sdp) return [];
  const sessionLevel: string[] = [];
  const mediaLevel: string[] = [];
  let inMedia = false;
  for (const line of description.sdp.split(/\r?\n/)) {
    if (line.startsWith("m=")) inMedia = true;
    if (!line.startsWith("a=fingerprint:")) continue;
    const fields = line
      .slice("a=fingerprint:".length)
      .trim()
      .toLowerCase()
      .split(/\s+/);
    if (fields.length !== 2) continue;
    const fingerprint = fields.join(" ");
    (inMedia ? mediaLevel : sessionLevel).push(fingerprint);
  }
  return [...new Set(sessionLevel.length > 0 ? sessionLevel : mediaLevel)];
}

/** Returns only the negotiation-relevant shape, never the wire SDP itself. */
export function summarizeSessionDescription(
  description: RTCSessionDescriptionInit | null | undefined,
): Record<string, unknown> | null {
  if (!description) return null;
  const sections: Record<string, unknown>[] = [];
  let current: Record<string, unknown> = {};
  const section = () => {
    if (Object.keys(current).length === 0) current.mid = "?";
    return current;
  };
  for (const line of description.sdp?.split(/\r?\n/) ?? []) {
    if (line.startsWith("a=mid:")) {
      if (Object.keys(current).length > 0) sections.push(current);
      current = { mid: line.slice("a=mid:".length) };
    } else if (line.startsWith("a=setup:")) {
      section().setup = line.slice("a=setup:".length);
    } else if (line.startsWith("a=ice-ufrag:")) {
      section().iceUfragHash = digest(line.slice("a=ice-ufrag:".length));
    } else if (line.startsWith("a=ice-pwd:")) {
      section().icePwdHash = digest(line.slice("a=ice-pwd:".length));
    } else if (line.startsWith("a=fingerprint:")) {
      section().dtlsFingerprintHash = digest(
        line.slice("a=fingerprint:".length),
      );
    }
  }
  if (Object.keys(current).length > 0) sections.push(current);
  return { type: description.type, sections };
}

export function diagnosticError(err: unknown): Record<string, unknown> {
  if (err instanceof Error) return { name: err.name, message: err.message };
  return { message: String(err) };
}

type Stat = RTCStats & Record<string, unknown>;

function numberField(stat: Stat, key: string): number | undefined {
  return typeof stat[key] === "number" ? (stat[key] as number) : undefined;
}

function stringField(stat: Stat, key: string): string | undefined {
  return typeof stat[key] === "string" ? (stat[key] as string) : undefined;
}

function candidateSummary(stat: Stat | undefined): Record<string, unknown> {
  if (!stat) return {};
  // Addresses, ports, related addresses and raw candidate strings are
  // deliberately excluded. Types are enough to compare host/srflx/relay paths.
  return {
    candidateType: stringField(stat, "candidateType"),
    protocol: stringField(stat, "protocol"),
    relayProtocol: stringField(stat, "relayProtocol"),
    networkType: stringField(stat, "networkType"),
  };
}

/** Summarizes transport progress without exposing network identities. */
export function summarizeRtcStats(report: RTCStatsReport): {
  candidatePairs: Record<string, unknown>[];
  transports: Record<string, unknown>[];
  rtp: Record<string, unknown>[];
} {
  const stats = new Map<string, Stat>();
  report.forEach((value, key) => stats.set(key, value as Stat));
  const candidatePairs: Record<string, unknown>[] = [];
  const transports: Record<string, unknown>[] = [];
  const rtp: Record<string, unknown>[] = [];
  for (const [id, stat] of stats) {
    if (stat.type === "candidate-pair") {
      const nominated = stat.nominated === true;
      const selected = stat.selected === true;
      if (!nominated && !selected) continue;
      candidatePairs.push({
        id,
        state: stringField(stat, "state"),
        nominated,
        selected,
        bytesSent: numberField(stat, "bytesSent"),
        bytesReceived: numberField(stat, "bytesReceived"),
        currentRoundTripTime: numberField(stat, "currentRoundTripTime"),
        local: candidateSummary(
          stats.get(stringField(stat, "localCandidateId") ?? ""),
        ),
        remote: candidateSummary(
          stats.get(stringField(stat, "remoteCandidateId") ?? ""),
        ),
      });
    } else if (stat.type === "transport") {
      transports.push({
        id,
        dtlsState: stringField(stat, "dtlsState"),
        iceState: stringField(stat, "iceState"),
        selectedCandidatePairId: stringField(stat, "selectedCandidatePairId"),
        selectedCandidatePairChanges: numberField(
          stat,
          "selectedCandidatePairChanges",
        ),
      });
    } else if (stat.type === "inbound-rtp" || stat.type === "outbound-rtp") {
      rtp.push({
        id,
        type: stat.type,
        kind: stringField(stat, "kind") ?? stringField(stat, "mediaType"),
        ssrc: numberField(stat, "ssrc"),
        packetsSent: numberField(stat, "packetsSent"),
        bytesSent: numberField(stat, "bytesSent"),
        packetsReceived: numberField(stat, "packetsReceived"),
        bytesReceived: numberField(stat, "bytesReceived"),
        packetsLost: numberField(stat, "packetsLost"),
        jitter: numberField(stat, "jitter"),
        framesDecoded: numberField(stat, "framesDecoded"),
        framesSent: numberField(stat, "framesSent"),
      });
    }
  }
  candidatePairs.sort(
    (a, b) => Number(b.selected === true) - Number(a.selected === true),
  );
  return {
    candidatePairs: candidatePairs.slice(0, 4),
    transports,
    rtp,
  };
}

export function createPeerDiagnostics(
  pc: RTCPeerConnection,
  identity: PeerDiagnosticIdentity,
): PeerDiagnostics {
  const tag = `[rtc ${identity.self}→${identity.peer}]`;
  let closed = false;
  let statsInFlight = false;
  let queuedStatsReason: string | null = null;
  let statsSequence = 0;

  const log: PeerDiagnostics["log"] = (event, details = {}, level = "info") => {
    if (closed) return;
    console[level](tag, {
      at: new Date().toISOString(),
      ...identity,
      event,
      connectionState: pc.connectionState,
      iceConnectionState: pc.iceConnectionState,
      signalingState: pc.signalingState,
      iceGatheringState: pc.iceGatheringState,
      ...details,
    });
  };

  const runStats = async (reason: string) => {
    const sequence = ++statsSequence;
    try {
      const report = await pc.getStats();
      if (closed) return;
      log("stats", { reason, sequence, ...summarizeRtcStats(report) });
    } catch (err) {
      log(
        "stats:error",
        { reason, sequence, error: diagnosticError(err) },
        "warn",
      );
    } finally {
      statsInFlight = false;
      const queued = queuedStatsReason;
      queuedStatsReason = null;
      if (!closed && queued !== null) captureStats(queued);
    }
  };

  const captureStats = (reason: string) => {
    if (closed) return;
    if (statsInFlight) {
      queuedStatsReason = reason;
      return;
    }
    statsInFlight = true;
    void runStats(reason);
  };

  return {
    log,
    captureStats,
    close() {
      closed = true;
      queuedStatsReason = null;
    },
  };
}
