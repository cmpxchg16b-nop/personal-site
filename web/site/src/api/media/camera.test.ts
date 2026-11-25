import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CameraController } from "./camera";
import { loadMediaSettings, saveMediaSettings } from "./settings";

function track(kind: "video" | "audio" = "video") {
  return { kind, stop: vi.fn() } as unknown as MediaStreamTrack;
}

function stream(video: MediaStreamTrack): MediaStream {
  return {
    getTracks: () => [video],
    getVideoTracks: () => [video],
  } as unknown as MediaStream;
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

const storage = new Map<string, string>();
const getUserMedia = vi.fn();

beforeEach(() => {
  storage.clear();
  getUserMedia.mockReset();
  vi.stubGlobal("window", {
    localStorage: {
      getItem: (key: string) => storage.get(key) ?? null,
      setItem: (key: string, value: string) => storage.set(key, value),
    },
  });
  vi.stubGlobal("navigator", { mediaDevices: { getUserMedia } });
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("CameraController", () => {
  it("shares one capture and stops it only after the last hold", async () => {
    const first = track();
    const captured = stream(first);
    getUserMedia.mockResolvedValue(captured);
    const controller = new CameraController();
    const published: (MediaStream | null)[] = [];
    controller.subscribe((value) => published.push(value));

    expect(await controller.acquire()).toBe(captured);
    expect(await controller.acquire()).toBe(captured);
    expect(getUserMedia).toHaveBeenCalledOnce();
    controller.release();
    expect(first.stop).not.toHaveBeenCalled();
    controller.release();
    expect(first.stop).toHaveBeenCalledOnce();
    expect(published).toEqual([null, captured, null]);
  });

  it("uses the saved device, hot-switches subscribers and persists the choice", async () => {
    saveMediaSettings({ cameraId: "camera-a", speakerVolume: 0.3 });
    const oldTrack = track();
    const newTrack = track();
    const oldStream = stream(oldTrack);
    const newStream = stream(newTrack);
    getUserMedia
      .mockResolvedValueOnce(oldStream)
      .mockResolvedValueOnce(newStream);
    const controller = new CameraController();
    await controller.acquire();
    const listener = vi.fn();
    controller.subscribe(listener);

    await controller.setDevice("camera-b");
    expect(getUserMedia).toHaveBeenNthCalledWith(1, {
      video: { deviceId: { exact: "camera-a" } },
    });
    expect(getUserMedia).toHaveBeenNthCalledWith(2, {
      video: { deviceId: { exact: "camera-b" } },
    });
    expect(listener).toHaveBeenLastCalledWith(newStream);
    expect(oldTrack.stop).toHaveBeenCalledOnce();
    expect(loadMediaSettings()).toMatchObject({
      cameraId: "camera-b",
      speakerVolume: 0.3,
    });
    controller.release();
    expect(newTrack.stop).toHaveBeenCalledOnce();
  });

  it("honours a selection made while the initial permission request is pending", async () => {
    const pending = deferred<MediaStream>();
    const obsoleteTrack = track();
    const selectedTrack = track();
    const selectedStream = stream(selectedTrack);
    getUserMedia
      .mockReturnValueOnce(pending.promise)
      .mockResolvedValueOnce(selectedStream);
    const controller = new CameraController();
    const acquisition = controller.acquire();
    await controller.setDevice("selected-camera");
    pending.resolve(stream(obsoleteTrack));

    expect(await acquisition).toBe(selectedStream);
    expect(obsoleteTrack.stop).toHaveBeenCalledOnce();
    expect(getUserMedia).toHaveBeenLastCalledWith({
      video: { deviceId: { exact: "selected-camera" } },
    });
    controller.release();
  });

  it("does not let a stale released acquisition corrupt a retry's holds", async () => {
    const obsolete = deferred<MediaStream>();
    const current = deferred<MediaStream>();
    const obsoleteTrack = track();
    const currentTrack = track();
    const currentStream = stream(currentTrack);
    getUserMedia
      .mockReturnValueOnce(obsolete.promise)
      .mockReturnValueOnce(current.promise);
    const controller = new CameraController();
    const obsoleteAcquisition = controller.acquire();
    controller.release();
    const currentAcquisition = controller.acquire();
    obsolete.resolve(stream(obsoleteTrack));

    await expect(obsoleteAcquisition).rejects.toThrow("released");
    current.resolve(currentStream);
    expect(await currentAcquisition).toBe(currentStream);
    expect(controller.localStream).toBe(currentStream);
    expect(currentTrack.stop).not.toHaveBeenCalled();
    controller.release();
    expect(currentTrack.stop).toHaveBeenCalledOnce();
  });

  it("keeps the working capture if a replacement cannot be opened", async () => {
    const currentTrack = track();
    const currentStream = stream(currentTrack);
    getUserMedia
      .mockResolvedValueOnce(currentStream)
      .mockRejectedValueOnce(new DOMException("denied", "NotAllowedError"));
    const controller = new CameraController();
    await controller.acquire();

    await expect(controller.setDevice("denied-camera")).rejects.toMatchObject({
      name: "NotAllowedError",
    });
    expect(controller.localStream).toBe(currentStream);
    expect(currentTrack.stop).not.toHaveBeenCalled();
    controller.release();
    expect(currentTrack.stop).toHaveBeenCalledOnce();
  });
});
