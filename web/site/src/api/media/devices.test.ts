import { describe, expect, it, vi } from "vitest";
import { enumerateDevicesWithMicrophoneLabels } from "./devices";

function device(kind: MediaDeviceKind, label: string): MediaDeviceInfo {
  return { kind, label } as MediaDeviceInfo;
}

function mediaDevices(
  enumerateDevices: ReturnType<typeof vi.fn>,
  getUserMedia: ReturnType<typeof vi.fn>,
): MediaDevices {
  return { enumerateDevices, getUserMedia } as unknown as MediaDevices;
}

function capture() {
  const stop = vi.fn();
  const track = { stop } as unknown as MediaStreamTrack;
  const stream = {
    getTracks: () => [track],
  } as unknown as MediaStream;
  return { stream, stop };
}

describe("media device labels", () => {
  it("does not capture audio when microphone labels are already available", async () => {
    const devices = [device("audioinput", "USB Microphone")];
    const enumerateDevices = vi.fn().mockResolvedValue(devices);
    const getUserMedia = vi.fn();

    await expect(
      enumerateDevicesWithMicrophoneLabels(
        mediaDevices(enumerateDevices, getUserMedia),
      ),
    ).resolves.toBe(devices);
    expect(getUserMedia).not.toHaveBeenCalled();
  });

  it("does not capture audio for a camera-only list with hidden labels", async () => {
    const devices = [device("videoinput", "")];
    const enumerateDevices = vi.fn().mockResolvedValue(devices);
    const getUserMedia = vi.fn();

    await expect(
      enumerateDevicesWithMicrophoneLabels(
        mediaDevices(enumerateDevices, getUserMedia),
      ),
    ).resolves.toBe(devices);
    expect(getUserMedia).not.toHaveBeenCalled();
  });

  it("re-enumerates hidden microphone labels during capture and stops the temporary track", async () => {
    const hidden = [device("audioinput", "")];
    const revealed = [device("audioinput", "USB Microphone")];
    const enumerateDevices = vi
      .fn()
      .mockResolvedValueOnce(hidden)
      .mockResolvedValueOnce(revealed);
    const { stream, stop } = capture();
    const getUserMedia = vi.fn().mockResolvedValue(stream);

    await expect(
      enumerateDevicesWithMicrophoneLabels(
        mediaDevices(enumerateDevices, getUserMedia),
      ),
    ).resolves.toBe(revealed);
    expect(getUserMedia).toHaveBeenCalledWith({ audio: true });
    expect(enumerateDevices).toHaveBeenCalledTimes(2);
    expect(stop).toHaveBeenCalledOnce();
    expect(stop.mock.invocationCallOrder[0]).toBeGreaterThan(
      enumerateDevices.mock.invocationCallOrder[1],
    );
  });

  it("returns the original list when microphone permission is unavailable", async () => {
    const hidden = [device("audioinput", "")];
    const enumerateDevices = vi.fn().mockResolvedValue(hidden);
    const getUserMedia = vi
      .fn()
      .mockRejectedValue(new DOMException("Denied", "NotAllowedError"));

    await expect(
      enumerateDevicesWithMicrophoneLabels(
        mediaDevices(enumerateDevices, getUserMedia),
      ),
    ).resolves.toBe(hidden);
    expect(enumerateDevices).toHaveBeenCalledOnce();
  });

  it("stops the temporary capture when the second enumeration fails", async () => {
    const enumerateDevices = vi
      .fn()
      .mockResolvedValueOnce([device("audioinput", "")])
      .mockRejectedValueOnce(new Error("Enumeration failed"));
    const { stream, stop } = capture();
    const getUserMedia = vi.fn().mockResolvedValue(stream);

    await expect(
      enumerateDevicesWithMicrophoneLabels(
        mediaDevices(enumerateDevices, getUserMedia),
      ),
    ).rejects.toThrow("Enumeration failed");
    expect(stop).toHaveBeenCalledOnce();
  });

  it("does not request capture when the initial enumeration fails", async () => {
    const enumerateDevices = vi
      .fn()
      .mockRejectedValue(new Error("Enumeration failed"));
    const getUserMedia = vi.fn();

    await expect(
      enumerateDevicesWithMicrophoneLabels(
        mediaDevices(enumerateDevices, getUserMedia),
      ),
    ).rejects.toThrow("Enumeration failed");
    expect(getUserMedia).not.toHaveBeenCalled();
  });
});
