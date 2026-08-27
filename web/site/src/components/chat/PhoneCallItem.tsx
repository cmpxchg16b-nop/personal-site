"use client";

// PhoneCallItem renders a phone-call message: the call log entry of one
// voice call in the conversation history. Its status text moves with the
// call ("inviting" → "accepted" → "ended", …) as the parties' amends
// land — the message is the invitation, amended in place. The audio
// itself never travels in the message.

import { Box, Typography } from "@mui/material";
import CallEndIcon from "@mui/icons-material/CallEnd";
import PhoneDisabledIcon from "@mui/icons-material/PhoneDisabled";
import PhoneInTalkIcon from "@mui/icons-material/PhoneInTalk";
import PhoneMissedIcon from "@mui/icons-material/PhoneMissed";
import RingVolumeIcon from "@mui/icons-material/RingVolume";
import { formatDistanceToNow } from "date-fns";
import { useTranslation } from "react-i18next";
import type { ChatUser } from "@/api/ss/types";
import { dateFnsLocaleFor, localeTagFor } from "@/i18n";
import UserAvatar from "./UserAvatar";
import type { PhoneCallMessage, PhoneSessionStatus } from "./types";

type PhoneCallItemProps = {
  message: PhoneCallMessage;
  // The caller (the invitation's author).
  author: ChatUser;
  // isOwn marks a call we placed: the author name picks up the primary
  // color, mirroring MessageItem, and a cancelled call we placed reads
  // "cancelled" rather than "missed".
  isOwn: boolean;
};

const STATUS_ICON = {
  inviting: RingVolumeIcon,
  accepted: PhoneInTalkIcon,
  ended: CallEndIcon,
  rejected: PhoneDisabledIcon,
  cancelled: PhoneMissedIcon,
} as const;

export function PhoneCallItem({ message, author, isOwn }: PhoneCallItemProps) {
  const { t, i18n } = useTranslation();
  const sentAt = new Date(message.timestamp * 1000);
  const relative = formatDistanceToNow(sentAt, {
    addSuffix: true,
    locale: dateFnsLocaleFor(i18n.language),
  });
  // Full timestamp for the hover tooltip on the relative time.
  const absolute = sentAt.toLocaleString(localeTagFor(i18n.language));

  const statusText = (status: PhoneSessionStatus): string => {
    switch (status) {
      case "inviting":
        return isOwn ? t("chat.call.outgoing") : t("chat.call.incoming");
      case "accepted":
        return t("chat.call.inCall");
      case "ended":
        return t("chat.call.ended");
      case "rejected":
        return t("chat.call.declined");
      case "cancelled":
        return isOwn ? t("chat.call.cancelled") : t("chat.call.missed");
    }
  };
  const StatusIcon = STATUS_ICON[message.phoneStatus];
  // Ringing and in-call entries are live states, tinted; the rest are
  // settled history.
  const live =
    message.phoneStatus === "inviting" || message.phoneStatus === "accepted";

  return (
    <Box sx={{ display: "flex", gap: 1.5, px: 2, py: 0.75, borderRadius: 2 }}>
      <UserAvatar user={author} size={36} />
      <Box sx={{ minWidth: 0, flexGrow: 1 }}>
        <Box sx={{ display: "flex", alignItems: "baseline", gap: 1 }}>
          <Typography
            variant="subtitle2"
            component="span"
            color={isOwn ? "primary.main" : "text.primary"}
          >
            {author.name}
          </Typography>
          <Typography
            variant="caption"
            color="text.secondary"
            component="span"
            title={absolute}
          >
            {relative}
          </Typography>
        </Box>
        <Box
          sx={{
            mt: 0.5,
            display: "flex",
            alignItems: "center",
            gap: 0.75,
            color: live ? "primary.main" : "text.secondary",
          }}
        >
          <StatusIcon fontSize="small" />
          <Typography variant="body2" component="span">
            {statusText(message.phoneStatus)}
          </Typography>
        </Box>
      </Box>
    </Box>
  );
}
