"use client";

import type { ReactNode } from "react";

import { AudioGraphProvider } from "@/api/audio/audiograph";
import { SSProxyProvider } from "@/api/ss/react";

// The /chat segment owns the signalling server connection: mounting it
// connects (retrying until the handshake succeeds) and provides the
// SSProxy singleton to every page under /chat. The singleton itself lives
// at module level, so it survives leaving the segment and is reused on
// return while still connected.
//
// The segment likewise provides the audio graph singleton the voice
// calls run through (see api/audio/audiograph.tsx); unlike the proxy it
// is fully released on unmount, so leaving /chat never leaves the
// microphone open.
export default function ChatLayout({ children }: { children: ReactNode }) {
  return (
    <SSProxyProvider>
      <AudioGraphProvider>{children}</AudioGraphProvider>
    </SSProxyProvider>
  );
}
