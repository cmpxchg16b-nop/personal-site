import { describe, expect, it } from "vitest";

import type { Credential } from "@/hooks/useSipCredentials";
import {
  eligibleSipCredentials,
  reconcileSipCredentialSelection,
} from "./sipForm";

const NOW = Date.parse("2026-11-23T12:00:00.000Z");
const FUTURE = "2026-11-23T12:01:00.000Z";

function credential(
  id: string,
  enabled: boolean,
  state: Credential["registration"]["state"],
  expiresAt: string | null = FUTURE,
  registeredAt?: string,
): Credential {
  return {
    id,
    sip_uri: `sip:${id}@example.com`,
    server: "example.com",
    username: id,
    transport: "tls",
    enabled,
    registration: {
      state,
      attempts: 1,
      expires_at: expiresAt ?? undefined,
      registered_at: registeredAt,
    },
  };
}

describe("external SIP caller eligibility", () => {
  it("allows enabled identities with valid leases throughout refresh states", () => {
    const credentials = [
      credential("registered", true, "registered"),
      credential("registering", true, "registering"),
      credential("failed-refresh", true, "failed"),
      credential("disabled", false, "registered"),
      credential("off", false, "disabled", null),
      credential("registering-without-lease", true, "registering", null),
    ];

    expect(
      eligibleSipCredentials(credentials, NOW).map(({ id }) => id),
    ).toEqual(["registered", "registering", "failed-refresh"]);
    expect(eligibleSipCredentials(undefined, NOW)).toEqual([]);
  });

  it("requires a valid future expires_at and never treats registered_at as a lease", () => {
    const credentials = [
      credential(
        "missing",
        true,
        "registered",
        null,
        "2026-11-23T12:02:00.000Z",
      ),
      credential(
        "invalid",
        true,
        "failed",
        "not-a-date",
        "2026-11-23T12:02:00.000Z",
      ),
      credential("expired", true, "registered", "2026-11-23T11:59:59.999Z"),
      credential("at-expiry", true, "registered", "2026-11-23T12:00:00.000Z"),
      credential("future", true, "registered", "2026-11-23T12:00:00.001Z"),
    ];

    expect(
      eligibleSipCredentials(credentials, NOW).map(({ id }) => id),
    ).toEqual(["future"]);
  });

  it("preserves a selected caller through refresh and reconciles stale selections", () => {
    const first = credential("first", true, "registered");
    const refreshing = credential("second", true, "registering");
    const eligible = eligibleSipCredentials([first, refreshing], NOW);

    expect(reconcileSipCredentialSelection("second", eligible)).toBe("second");
    expect(reconcileSipCredentialSelection("stale", eligible)).toBe("first");

    const expired = credential(
      "second",
      true,
      "failed",
      "2026-11-23T11:59:59Z",
    );
    const withoutSecond = eligibleSipCredentials([first, expired], NOW);
    expect(reconcileSipCredentialSelection("second", withoutSecond)).toBe(
      "first",
    );
    expect(reconcileSipCredentialSelection("first", [])).toBe("");
  });
});
