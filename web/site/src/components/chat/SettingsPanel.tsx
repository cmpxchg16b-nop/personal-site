"use client";

import {
  Avatar,
  Box,
  List,
  ListItem,
  ListItemButton,
  ListItemIcon,
  ListItemText,
  Typography,
} from "@mui/material";
import ChevronRightIcon from "@mui/icons-material/ChevronRight";
import PersonOutlineIcon from "@mui/icons-material/AccountCircleOutlined";
import SettingsPhoneIcon from "@mui/icons-material/SettingsPhone";
import { useTranslation } from "react-i18next";
import type { ChatUser } from "@/api/ss/types";
import UserAvatar from "./UserAvatar";

type SettingsPanelProps = {
  user: ChatUser | undefined;
  sipSelected: boolean;
  onOpenSip: () => void;
};

// SettingsPanel intentionally has no profile editing controls: identity still
// belongs to the site's sign-in session, not to the WebRTC sidebar.
export default function SettingsPanel({
  user,
  sipSelected,
  onOpenSip,
}: SettingsPanelProps) {
  const { t } = useTranslation();

  return (
    <Box sx={{ flexGrow: 1, minHeight: 0, overflowY: "auto" }}>
      <Box
        sx={{
          px: 2,
          py: 3,
          display: "flex",
          flexDirection: "column",
          alignItems: "center",
          textAlign: "center",
        }}
      >
        {user === undefined ? (
          <Avatar sx={{ width: 72, height: 72, mb: 1.5 }}>
            <PersonOutlineIcon fontSize="large" />
          </Avatar>
        ) : (
          <Box sx={{ mb: 1.5 }}>
            <UserAvatar user={user} size={72} />
          </Box>
        )}
        <Typography variant="subtitle1" noWrap sx={{ maxWidth: "100%" }}>
          {user?.name ?? "—"}
        </Typography>
      </Box>
      <List disablePadding sx={{ py: 1 }}>
        <ListItem disablePadding>
          <ListItemButton
            selected={sipSelected}
            onClick={onOpenSip}
            aria-label={t("chat.sip.title")}
            aria-controls="sip-registration-settings"
            sx={{ px: 2, py: 1.5 }}
          >
            <ListItemIcon sx={{ minWidth: 40 }}>
              <SettingsPhoneIcon color="action" />
            </ListItemIcon>
            <ListItemText
              primary={t("chat.sip.title")}
              slotProps={{
                primary: { variant: "body2", sx: { fontWeight: 500 } },
              }}
            />
            <ChevronRightIcon color="action" sx={{ ml: 1 }} />
          </ListItemButton>
        </ListItem>
      </List>
    </Box>
  );
}
