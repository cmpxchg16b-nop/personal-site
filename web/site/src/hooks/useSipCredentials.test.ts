import { afterEach, describe, expect, it, vi } from "vitest";

import {
  createSipCredential,
  deleteSipCredential,
  fetchSipCredentials,
  SipCredentialsApiError,
  updateSipCredential,
  type Credential,
  type CredentialInput,
} from "./useSipCredentials";

const credential: Credential = {
  id: "credential-1",
  sip_uri: "sip:alice@example.com",
  server: "example.com",
  username: "alice",
  transport: "tls",
  enabled: true,
  registration: { state: "registered", attempts: 1 },
};

const input: CredentialInput = {
  server: "example.com",
  username: "alice",
  password: "secret",
  transport: "tls",
  enabled: true,
};

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("SIP credentials API", () => {
  it("reads the credential list without exposing a password field", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(jsonResponse(200, { credentials: [credential] }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(fetchSipCredentials()).resolves.toEqual([credential]);
    expect(fetchMock).toHaveBeenCalledWith("/api/sip/credentials", {
      signal: undefined,
    });
    expect(credential).not.toHaveProperty("password");
  });

  it("posts and puts the exact credential input, including an empty update password", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(201, credential))
      .mockResolvedValueOnce(jsonResponse(200, credential));
    vi.stubGlobal("fetch", fetchMock);

    await expect(createSipCredential(input)).resolves.toEqual(credential);
    const updateInput = { ...input, password: "" };
    await expect(
      updateSipCredential("account/primary", updateInput),
    ).resolves.toEqual(credential);

    expect(fetchMock).toHaveBeenNthCalledWith(1, "/api/sip/credentials", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(input),
    });
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/api/sip/credentials/account%2Fprimary",
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(updateInput),
      },
    );
  });

  it("deletes an encoded credential id and accepts a 204 response", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(deleteSipCredential("account/primary")).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/sip/credentials/account%2Fprimary",
      { method: "DELETE" },
    );
  });

  it("surfaces the server err field and the HTTP status", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(jsonResponse(409, { err: "already exists" })),
    );

    const request = createSipCredential(input);
    await expect(request).rejects.toBeInstanceOf(SipCredentialsApiError);
    await expect(request).rejects.toMatchObject({
      status: 409,
      message: "already exists",
    });
  });

  it("uses an HTTP fallback for non-JSON errors", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response("unavailable", { status: 503 })),
    );

    await expect(fetchSipCredentials()).rejects.toMatchObject({
      status: 503,
      message: "GET /api/sip/credentials failed: 503",
    });
  });
});
