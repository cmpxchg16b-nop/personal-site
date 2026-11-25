"use client";

import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  type ReactNode,
} from "react";
import { loadMediaSettings, saveMediaSettings } from "./settings";

type CameraListener = (stream: MediaStream | null) => void;

/**
 * The one camera capture shared by channel WebRTC, external SIP and previews.
 * Consumers hold acquisitions; changing devices publishes a replacement stream
 * so each existing RTP sender can switch to its video track.
 */
export class CameraController {
  private stream: MediaStream | null = null;
  private capturePromise: Promise<MediaStream> | null = null;
  private readonly holds = new Set<number>();
  private nextHold = 0;
  private lifecycle = 0;
  private deviceVersion = 0;
  private deviceId = loadMediaSettings().cameraId;
  private switchTail: Promise<void> = Promise.resolve();
  private readonly listeners = new Set<CameraListener>();

  get selectedDeviceId(): string {
    return this.deviceId;
  }

  get localStream(): MediaStream | null {
    return this.stream;
  }

  subscribe(listener: CameraListener): () => void {
    this.listeners.add(listener);
    listener(this.stream);
    return () => this.listeners.delete(listener);
  }

  private publish(stream: MediaStream | null): void {
    for (const listener of this.listeners) listener(stream);
  }

  private openCamera(): Promise<MediaStream> {
    return navigator.mediaDevices.getUserMedia({
      video:
        this.deviceId === "" ? true : { deviceId: { exact: this.deviceId } },
    });
  }

  private async openInitialCapture(lifecycle: number): Promise<MediaStream> {
    for (;;) {
      const selection = this.deviceVersion;
      const stream = await this.openCamera();
      if (lifecycle !== this.lifecycle || this.holds.size === 0) {
        for (const track of stream.getTracks()) track.stop();
        throw new Error("camera: released while opening the camera");
      }
      if (selection === this.deviceVersion) {
        this.stream = stream;
        this.publish(stream);
        return stream;
      }
      // The selection changed while the permission prompt was open. Do not
      // expose the obsolete device; retry with the latest persisted choice.
      for (const track of stream.getTracks()) track.stop();
    }
  }

  async acquire(): Promise<MediaStream> {
    const hold = ++this.nextHold;
    this.holds.add(hold);
    let promise = this.capturePromise;
    if (promise === null) {
      promise = this.openInitialCapture(this.lifecycle);
      this.capturePromise = promise;
    }
    try {
      return await promise;
    } catch (error) {
      // A released, stale acquisition must never decrement the holds of a
      // newer capture. Set membership identifies this acquisition exactly.
      this.holds.delete(hold);
      if (this.capturePromise === promise && this.stream === null) {
        this.capturePromise = null;
      }
      throw error;
    }
  }

  release(): void {
    const hold = this.holds.values().next().value as number | undefined;
    if (hold !== undefined) this.holds.delete(hold);
    if (this.holds.size === 0) this.stopCapture();
  }

  private stopCapture(): void {
    this.lifecycle += 1;
    this.capturePromise = null;
    const stream = this.stream;
    this.stream = null;
    if (stream !== null) {
      this.publish(null);
      for (const track of stream.getTracks()) track.stop();
    }
  }

  setDevice(deviceId: string): Promise<void> {
    this.deviceId = deviceId;
    saveMediaSettings({ cameraId: deviceId });
    const selection = ++this.deviceVersion;
    this.switchTail = this.switchTail
      .catch(() => {})
      .then(async () => {
        // A pending initial capture observes deviceVersion and retries itself.
        if (
          selection !== this.deviceVersion ||
          this.holds.size === 0 ||
          this.stream === null
        ) {
          return;
        }
        const lifecycle = this.lifecycle;
        const previous = this.stream;
        const next = await this.openCamera();
        if (
          selection !== this.deviceVersion ||
          lifecycle !== this.lifecycle ||
          this.holds.size === 0 ||
          this.stream !== previous
        ) {
          for (const track of next.getTracks()) track.stop();
          return;
        }
        this.stream = next;
        this.capturePromise = Promise.resolve(next);
        this.publish(next);
        for (const track of previous.getTracks()) track.stop();
      });
    return this.switchTail;
  }

  dispose(): void {
    this.holds.clear();
    this.deviceVersion += 1;
    this.stopCapture();
  }
}

let theCamera: CameraController | null = null;
const CameraContext = createContext<CameraController | null>(null);

export function CameraProvider({ children }: { children: ReactNode }) {
  const camera = useMemo(() => {
    theCamera ??= new CameraController();
    return theCamera;
  }, []);
  useEffect(
    () => () => {
      camera.dispose();
      if (theCamera === camera) theCamera = null;
    },
    [camera],
  );
  return (
    <CameraContext.Provider value={camera}>{children}</CameraContext.Provider>
  );
}

export function useCameraController(): CameraController | null {
  return useContext(CameraContext);
}
