"use client";

import type { ReactNode } from "react";

import { SSProxyProvider } from "@/api/ss/react";

// The /chat segment owns the signalling server connection: mounting it
// connects (retrying until the handshake succeeds) and provides the
// SSProxy singleton to every page under /chat. The singleton itself lives
// at module level, so it survives leaving the segment and is reused on
// return while still connected.
export default function ChatLayout({ children }: { children: ReactNode }) {
  return <SSProxyProvider>{children}</SSProxyProvider>;
}
