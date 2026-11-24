"use client";

import { Box, Typography } from "@mui/material";
import PhoneInTalkIcon from "@mui/icons-material/PhoneInTalk";
import RingVolumeIcon from "@mui/icons-material/RingVolume";
import { useTranslation } from "react-i18next";

// The existing conversation-row treatment for a live call, shared by channel
// members and phone history.
export function CallStatusPill({
  status,
  incoming,
}: {
  status: "inviting" | "accepted";
  incoming: boolean;
}) {
  const { t } = useTranslation();
  const ringing = status === "inviting";
  const Icon = ringing ? RingVolumeIcon : PhoneInTalkIcon;
  const label = ringing
    ? incoming
      ? t("chat.call.incomingShort")
      : t("chat.call.callingShort")
    : t("chat.call.inCallShort");
  return (
    <Box
      component="span"
      sx={{
        ml: 1,
        px: 0.75,
        height: 18,
        borderRadius: "9999px",
        display: "inline-flex",
        alignItems: "center",
        gap: 0.5,
        flexShrink: 0,
        color: ringing ? "primary.main" : "success.main",
        border: 1,
        borderColor: ringing ? "primary.main" : "success.main",
        ...(ringing && {
          "@keyframes callPulse": {
            "0%, 100%": { opacity: 1 },
            "50%": { opacity: 0.4 },
          },
          animation: "callPulse 1.2s ease-in-out infinite",
        }),
      }}
    >
      <Icon sx={{ fontSize: 12 }} />
      <Typography variant="caption" component="span" sx={{ lineHeight: 1 }}>
        {label}
      </Typography>
    </Box>
  );
}
