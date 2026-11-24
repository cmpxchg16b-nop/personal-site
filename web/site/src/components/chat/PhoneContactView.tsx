"use client";

import {
  Box,
  IconButton,
  Typography,
  type SxProps,
  type Theme,
} from "@mui/material";
import ArrowBackIcon from "@mui/icons-material/ArrowBack";
import CallIcon from "@mui/icons-material/Call";
import SmsIcon from "@mui/icons-material/Sms";
import VideocamIcon from "@mui/icons-material/Videocam";
import { useTranslation } from "react-i18next";
import type { ChatUser } from "@/api/ss/types";
import UserAvatar from "./UserAvatar";

type PhoneContactViewProps = {
  user: ChatUser;
  onBack: () => void;
  onVoiceCall: () => void;
  onVideoCall: () => void;
  onMessage: () => void;
  sx?: SxProps<Theme>;
};

// PhoneContactView is the contact's action pane. Every action enters the
// existing conversation; voice/video also start their call immediately.
export default function PhoneContactView({
  user,
  onBack,
  onVoiceCall,
  onVideoCall,
  onMessage,
  sx,
}: PhoneContactViewProps) {
  const { t } = useTranslation();
  const actions = [
    { key: "videoCall", Icon: VideocamIcon, onClick: onVideoCall },
    { key: "voiceCall", Icon: CallIcon, onClick: onVoiceCall },
    { key: "sms", Icon: SmsIcon, onClick: onMessage },
  ] as const;

  return (
    <Box
      sx={[
        {
          flexGrow: 1,
          minWidth: 0,
          minHeight: 0,
          display: "flex",
          flexDirection: "column",
        },
        ...(Array.isArray(sx) ? sx : [sx]),
      ]}
    >
      <Box
        sx={{
          display: { xs: "flex", sm: "none" },
          alignItems: "center",
          px: 1,
          py: 0.75,
          borderBottom: 1,
          borderColor: "divider",
        }}
      >
        <IconButton onClick={onBack} aria-label={t("chat.phone.backToHistory")}>
          <ArrowBackIcon />
        </IconButton>
      </Box>
      <Box
        sx={{
          flexGrow: 1,
          minHeight: 0,
          overflowY: "auto",
          px: 3,
          pt: { xs: 5, sm: 8 },
          display: "flex",
          flexDirection: "column",
          alignItems: "center",
        }}
      >
        <UserAvatar user={user} size={96} />
        <Typography
          variant="h5"
          component="h2"
          noWrap
          sx={{ maxWidth: "100%", mt: 2, fontWeight: 500 }}
        >
          {user.name}
        </Typography>
        <Box sx={{ display: "flex", gap: 2.5, mt: 4 }}>
          {actions.map(({ key, Icon, onClick }) => (
            <IconButton
              key={key}
              size="large"
              aria-label={t(`chat.phone.${key}`)}
              title={t(`chat.phone.${key}`)}
              onClick={onClick}
              sx={{
                width: 56,
                height: 56,
                border: 1,
                borderColor: "divider",
                color: "primary.main",
              }}
            >
              <Icon />
            </IconButton>
          ))}
        </Box>
      </Box>
    </Box>
  );
}
