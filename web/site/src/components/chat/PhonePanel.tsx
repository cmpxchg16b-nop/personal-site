"use client";

import { useMemo, useState, type FormEvent } from "react";
import {
  Alert,
  Box,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Fab,
  InputAdornment,
  List,
  ListItem,
  ListItemAvatar,
  ListItemButton,
  ListItemText,
  Snackbar,
  TextField,
  Typography,
} from "@mui/material";
import AddIcon from "@mui/icons-material/Add";
import HistoryIcon from "@mui/icons-material/History";
import SearchIcon from "@mui/icons-material/Search";
import { formatDistanceToNow } from "date-fns";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { ChatUser } from "@/api/ss/types";
import { dateFnsLocaleFor, localeTagFor } from "@/i18n";
import { collectPhoneHistory, filterPhoneHistory } from "./phoneHistory";
import type { ChatMessage, ConversationRef, PhoneCallMessage } from "./types";
import UserAvatar from "./UserAvatar";

function statusText(
  message: PhoneCallMessage,
  outgoing: boolean,
  t: TFunction,
): string {
  switch (message.phoneStatus) {
    case "inviting":
      return outgoing ? t("chat.call.outgoing") : t("chat.call.incoming");
    case "accepted":
      return t("chat.call.inCall");
    case "ended":
      return t("chat.call.ended");
    case "rejected":
      return t("chat.call.declined");
    case "cancelled":
      return outgoing ? t("chat.call.cancelled") : t("chat.call.missed");
  }
}

type PhonePanelProps = {
  messages: Record<string, ChatMessage[]>;
  users: Record<string, ChatUser>;
  currentUserId: string;
  selectedMessageId: string | null;
  onSelectContact: (ref: ConversationRef, messageId: string) => void;
};

// PhonePanel is the left content pane for call history and new SIP calls.
// Selecting history opens contact actions; the number dialog is deliberately
// only a frontend placeholder until the SIP service lands.
export default function PhonePanel({
  messages,
  users,
  currentUserId,
  selectedMessageId,
  onSelectContact,
}: PhonePanelProps) {
  const { t, i18n } = useTranslation();
  const [dialogOpen, setDialogOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [number, setNumber] = useState("");
  const [noticeOpen, setNoticeOpen] = useState(false);
  const history = useMemo(
    () => collectPhoneHistory(messages, users, currentUserId),
    [messages, users, currentUserId],
  );
  const visibleHistory = useMemo(
    () => filterPhoneHistory(history, query),
    [history, query],
  );

  const closeDialog = () => {
    setDialogOpen(false);
    setNumber("");
  };
  const submitNumber = (event: FormEvent) => {
    event.preventDefault();
    if (number.trim() === "") return;
    closeDialog();
    setNoticeOpen(true);
  };

  return (
    <Box
      sx={{
        position: "relative",
        flexGrow: 1,
        minHeight: 0,
        display: "flex",
        flexDirection: "column",
      }}
    >
      <Box sx={{ px: 2, py: 2, borderBottom: 1, borderColor: "divider" }}>
        <TextField
          value={query}
          onChange={(event) => setQuery(event.target.value)}
          placeholder={t("chat.phone.search")}
          size="small"
          fullWidth
          sx={{
            "& .MuiOutlinedInput-root": { borderRadius: "9999px" },
          }}
          slotProps={{
            input: {
              startAdornment: (
                <InputAdornment position="start">
                  <SearchIcon fontSize="small" />
                </InputAdornment>
              ),
              sx: { pl: 1 },
            },
            htmlInput: { "aria-label": t("chat.phone.search") },
          }}
        />
      </Box>
      <Box sx={{ flexGrow: 1, minHeight: 0, overflowY: "auto" }}>
        {visibleHistory.length === 0 ? (
          <Box
            sx={{
              height: "100%",
              minHeight: 240,
              px: 3,
              display: "flex",
              flexDirection: "column",
              alignItems: "center",
              justifyContent: "center",
              gap: 1,
              textAlign: "center",
              color: "text.secondary",
            }}
          >
            <HistoryIcon sx={{ fontSize: 40, opacity: 0.6 }} />
            <Typography variant="body2">
              {history.length === 0
                ? t("chat.phone.empty")
                : t("chat.phone.noMatch")}
            </Typography>
          </Box>
        ) : (
          <List disablePadding sx={{ py: 0.5 }}>
            {visibleHistory.map(({ ref, message, user, outgoing }) => {
              const sentAt = new Date(message.timestamp * 1000);
              const relative = formatDistanceToNow(sentAt, {
                addSuffix: true,
                locale: dateFnsLocaleFor(i18n.language),
              });
              const absolute = sentAt.toLocaleString(
                localeTagFor(i18n.language),
              );
              return (
                <ListItem
                  key={`${ref.channelId}:${message.id}`}
                  disablePadding
                  divider
                  sx={{ "&:last-child": { borderBottom: 0 } }}
                >
                  <ListItemButton
                    selected={selectedMessageId === message.id}
                    onClick={() => onSelectContact(ref, message.id)}
                    aria-label={t("chat.phone.viewContact", {
                      name: user.name,
                    })}
                    title={absolute}
                    sx={{
                      px: 2,
                      py: 1.25,
                      bgcolor: "transparent",
                      "&:hover": { bgcolor: "action.hover" },
                      "&.Mui-selected": { bgcolor: "action.hover" },
                      "&.Mui-selected:hover": { bgcolor: "action.hover" },
                    }}
                  >
                    <ListItemAvatar sx={{ minWidth: 44 }}>
                      <UserAvatar user={user} size={36} />
                    </ListItemAvatar>
                    <ListItemText
                      primary={user.name}
                      secondary={`${statusText(message, outgoing, t)} · ${relative}`}
                      slotProps={{
                        primary: { variant: "body2", noWrap: true },
                        secondary: { variant: "caption", noWrap: true },
                      }}
                    />
                  </ListItemButton>
                </ListItem>
              );
            })}
          </List>
        )}
      </Box>
      <Fab
        color="primary"
        size="medium"
        aria-label={t("chat.phone.newSipCall")}
        onClick={() => setDialogOpen(true)}
        sx={{ position: "absolute", right: 16, bottom: 16 }}
      >
        <AddIcon />
      </Fab>

      <Dialog open={dialogOpen} onClose={closeDialog} fullWidth maxWidth="xs">
        <Box component="form" onSubmit={submitNumber}>
          <DialogTitle>{t("chat.phone.numberPrompt")}</DialogTitle>
          <DialogContent>
            <TextField
              autoFocus
              fullWidth
              type="tel"
              margin="dense"
              label={t("chat.phone.number")}
              value={number}
              onChange={(event) => setNumber(event.target.value)}
              slotProps={{ htmlInput: { inputMode: "tel" } }}
            />
          </DialogContent>
          <DialogActions sx={{ px: 3, pb: 2 }}>
            <Button type="button" onClick={closeDialog}>
              {t("chat.phone.cancel")}
            </Button>
            <Button
              type="submit"
              variant="contained"
              disabled={number.trim() === ""}
            >
              {t("chat.phone.call")}
            </Button>
          </DialogActions>
        </Box>
      </Dialog>
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
