"use client";

// useEchoCancellation exposes the audio graph's capture-side voice
// processing switch (echo cancellation, noise suppression, auto gain) as
// React state, mirroring useCallVolumes: the graph keeps the value, so a
// toggle made during one call still applies to the next.

import { useCallback, useState } from "react";
import type { AudioGraph } from "@/api/audio/audiograph";

export interface UseEchoCancellationResult {
  /** Whether the capture-side voice processing is on. */
  echoCancellation: boolean;
  setEchoCancellation: (on: boolean) => void;
}

export function useEchoCancellation(
  audio: AudioGraph | null,
): UseEchoCancellationResult {
  const [echoCancellation, setEchoCancellationState] = useState(() =>
    audio === null ? false : audio.getEchoCancellation(),
  );
  const setEchoCancellation = useCallback(
    (on: boolean) => {
      setEchoCancellationState(on);
      audio?.setEchoCancellation(on);
    },
    [audio],
  );
  return { echoCancellation, setEchoCancellation };
}
