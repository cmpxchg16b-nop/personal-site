"use client";

import { Avatar, Box } from "@mui/material";
import type { ChatUser } from "@/api/ss/types";

// avatarHue hashes a user id to a stable hue (0–359), so each user gets a
// consistent avatar color without the server assigning one. Same hashing as
// the top bar's ProfileMenu.
function avatarHue(id: string): number {
  let hash = 0;
  for (let i = 0; i < id.length; i++) {
    hash = (hash * 31 + id.charCodeAt(i)) | 0;
  }
  return ((hash % 360) + 360) % 360;
}

type UserAvatarProps = {
  user: ChatUser;
  // Pixel size of the avatar; the initial letter scales with it.
  size?: number;
  // showPresence adds a presence dot on the bottom-right corner: green
  // while the user is online, gray otherwise. connectionState overrides
  // what the dot reports with the peer connection's real state — green
  // only while connected, amber while connecting, gray otherwise (the
  // listing's online flag alone is nearly always true).
  showPresence?: boolean;
  connectionState?: RTCPeerConnectionState | null;
};

// UserAvatar renders a user's initial on a stable per-user color, optionally
// with a presence dot. Purely presentational.
export default function UserAvatar({
  user,
  size = 32,
  showPresence = false,
  connectionState,
}: UserAvatarProps) {
  const dotSize = Math.max(8, Math.round(size * 0.32));
  const dotColor =
    connectionState === undefined
      ? user.online
        ? "success.main"
        : "text.disabled"
      : !user.online
        ? "text.disabled"
        : connectionState === "connected"
          ? "success.main"
          : connectionState === "connecting"
            ? "warning.main"
            : "text.disabled";
  return (
    <Box
      sx={{ position: "relative", flexShrink: 0, width: size, height: size }}
    >
      <Avatar
        sx={{
          width: size,
          height: size,
          fontSize: size * 0.52,
          color: "#fff",
          bgcolor: `hsl(${avatarHue(user.id)}, 65%, 45%)`,
        }}
      >
        {user.name.charAt(0).toUpperCase()}
      </Avatar>
      {showPresence && (
        <Box
          component="span"
          sx={{
            position: "absolute",
            right: -1,
            bottom: -1,
            width: dotSize,
            height: dotSize,
            borderRadius: "50%",
            bgcolor: dotColor,
            // The ring matches the surrounding surface so the dot reads as
            // punched out of the avatar.
            border: "2px solid",
            borderColor: "background.paper",
            boxSizing: "border-box",
          }}
        />
      )}
    </Box>
  );
}
