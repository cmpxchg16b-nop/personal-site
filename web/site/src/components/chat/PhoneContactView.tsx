"use client";

import { useState } from "react";
import {
  Alert,
  Box,
  IconButton,
  Snackbar,
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
  sx?: SxProps<Theme>;
};

// PhoneContactView is the Linphone-style contact action pane. The actions are
// visual scaffolding only until their SIP/SMS implementations are available.
export default function PhoneContactView({
  user,
  onBack,
  sx,
}: PhoneContactViewProps) {
  const { t } = useTranslation();
  const [noticeOpen, setNoticeOpen] = useState(false);
  const actions = [
    { key: "videoCall", Icon: VideocamIcon },
    { key: "voiceCall", Icon: CallIcon },
    { key: "sms", Icon: SmsIcon },
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
          {actions.map(({ key, Icon }) => (
            <IconButton
              key={key}
              size="large"
              aria-label={t(`chat.phone.${key}`)}
              title={t(`chat.phone.${key}`)}
              onClick={() => setNoticeOpen(true)}
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
      <Snackbar
        open={noticeOpen}
        autoHideDuration={3500}
        onClose={(_, reason) => {
          if (reason !== "clickaway") setNoticeOpen(false);
        }}
        anchorOrigin={{ vertical: "bottom", horizontal: "center" }}
      >
        <Alert
          severity="info"
          variant="filled"
          onClose={() => setNoticeOpen(false)}
        >
          {t("chat.phone.unavailable")}
        </Alert>
      </Snackbar>
    </Box>
  );
}
