"use client";

// useCallVolumes exposes the audio graph's two user-facing volumes through a
// shared subscription: the top bar and Devices page always render the same
// gains, and the graph persists every change for later calls and page loads.

import { useCallback, useSyncExternalStore } from "react";
import type { AudioGraph } from "@/api/audio/audiograph";

export interface UseCallVolumesResult {
  /** The mic send volume, 0..1. */
  localVolume: number;
  /** The speaker volume of incoming voices, 0..1. */
  remoteVolume: number;
  setLocalVolume: (volume: number) => void;
  setRemoteVolume: (volume: number) => void;
}

export function useCallVolumes(audio: AudioGraph | null): UseCallVolumesResult {
  const subscribe = useCallback(
    (listener: () => void) => audio?.subscribeVolumes(listener) ?? (() => {}),
    [audio],
  );
  const getLocalVolume = useCallback(
    () => audio?.getLocalVolume() ?? 1,
    [audio],
  );
  const getRemoteVolume = useCallback(
    () => audio?.getRemoteVolume() ?? 1,
    [audio],
  );
  const localVolume = useSyncExternalStore(subscribe, getLocalVolume, () => 1);
  const remoteVolume = useSyncExternalStore(
    subscribe,
    getRemoteVolume,
    () => 1,
  );
  const setLocalVolume = useCallback(
    (volume: number) => audio?.setLocalVolume(volume),
    [audio],
  );
  const setRemoteVolume = useCallback(
    (volume: number) => audio?.setRemoteVolume(volume),
    [audio],
  );
  return { localVolume, remoteVolume, setLocalVolume, setRemoteVolume };
}
