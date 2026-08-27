"use client";

// useCallVolumes exposes the audio graph's two user-facing volumes as
// React state: the mic send volume (what peers hear) and the speaker
// volume of the incoming voices. The graph keeps the values, so an
// adjustment made before any call still applies once one starts.

import { useCallback, useState } from "react";
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
  const [localVolume, setLocalVolumeState] = useState(() =>
    audio === null ? 1 : audio.getLocalVolume(),
  );
  const [remoteVolume, setRemoteVolumeState] = useState(() =>
    audio === null ? 1 : audio.getRemoteVolume(),
  );
  const setLocalVolume = useCallback(
    (volume: number) => {
      setLocalVolumeState(volume);
      audio?.setLocalVolume(volume);
    },
    [audio],
  );
  const setRemoteVolume = useCallback(
    (volume: number) => {
      setRemoteVolumeState(volume);
      audio?.setRemoteVolume(volume);
    },
    [audio],
  );
  return { localVolume, remoteVolume, setLocalVolume, setRemoteVolume };
}
