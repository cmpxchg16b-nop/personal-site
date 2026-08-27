"use client";

// TopBarActions is the bridge through which the active page contributes
// contextual controls to the TopBar (the chat page's call volume control
// while a call is live, so far): TopBar renders the always-present
// TopBarActionsHost, and a page wraps its controls in <TopBarActions> to
// portal them into it. The host element is held at module level (the
// SSProxy-style singleton pattern) and subscribed via
// useSyncExternalStore, so the portal attaches whatever the mount order
// and disappears with whatever condition the page gates it on — the
// TopBar itself never learns what it is showing.

import { useSyncExternalStore, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { Box } from "@mui/material";

// The mounted host element and the subscribers waiting for it.
let hostElement: HTMLElement | null = null;
const hostListeners = new Set<() => void>();

const subscribeHost = (listener: () => void) => {
  hostListeners.add(listener);
  return () => {
    hostListeners.delete(listener);
  };
};
const getHost = () => hostElement;
const getServerHost = () => null;

// TopBarActionsHost is the empty TopBar slot pages portal into.
export function TopBarActionsHost() {
  return (
    <Box
      ref={(el: HTMLElement | null) => {
        hostElement = el;
        hostListeners.forEach((listener) => listener());
      }}
      sx={{ display: "flex", alignItems: "center", gap: 0.5, pr: 1 }}
    />
  );
}

// TopBarActions portals its children into the TopBar's host. Nothing
// renders until the host has mounted.
export function TopBarActions({ children }: { children: ReactNode }) {
  const host = useSyncExternalStore(subscribeHost, getHost, getServerHost);
  return host === null ? null : createPortal(children, host);
}
