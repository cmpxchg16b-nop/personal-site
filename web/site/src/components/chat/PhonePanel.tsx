"use client";

import { useMemo, useState, type FormEvent } from "react";
import {
  Alert,
  Avatar,
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
  MenuItem,
  Tab,
  Tabs,
  TextField,
  Typography,
} from "@mui/material";
import AddIcon from "@mui/icons-material/Add";
import CallIcon from "@mui/icons-material/Call";
import HistoryIcon from "@mui/icons-material/History";
import SearchIcon from "@mui/icons-material/Search";
import SmsIcon from "@mui/icons-material/Sms";
import VideocamIcon from "@mui/icons-material/Videocam";
import { formatDistanceToNow } from "date-fns";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { ChatUser } from "@/api/ss/types";
import type {
  ExternalSIPCall,
  ExternalSIPMessage,
  UseExternalSIPResult,
} from "@/api/ss/sip";
import { useSipCredentials } from "@/hooks/useSipCredentials";
import { dateFnsLocaleFor, localeTagFor } from "@/i18n";
import { collectPhoneHistory, filterPhoneHistory } from "./phoneHistory";
import {
  eligibleSipCredentials,
  reconcileSipCredentialSelection,
} from "./sipForm";
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

function externalCallName(call: ExternalSIPCall): string {
  return call.remoteUri ?? call.target ?? call.callId;
}

function externalMessageName(message: ExternalSIPMessage): string {
  return message.direction === "incoming"
    ? (message.remoteUri ?? message.target ?? message.localUri ?? "")
    : (message.target ?? message.remoteUri ?? "");
}

type PhonePanelProps = {
  messages: Record<string, ChatMessage[]>;
  users: Record<string, ChatUser>;
  currentUserId: string;
  selectedMessageId: string | null;
  onSelectContact: (ref: ConversationRef, messageId: string) => void;
  sip: UseExternalSIPResult;
};

// PhonePanel owns only form chrome. External call/message state remains in the
// SIP client, so switching tabs never clears either history.
export default function PhonePanel({
  messages,
  users,
  currentUserId,
  selectedMessageId,
  onSelectContact,
  sip,
}: PhonePanelProps) {
  const { t, i18n } = useTranslation();
  const credentialsQuery = useSipCredentials();
  const [section, setSection] = useState<"calls" | "messages">("calls");
  const [dialog, setDialog] = useState<"call" | "message" | null>(null);
  const [query, setQuery] = useState("");
  const [credentialChoice, setCredentialChoice] = useState("");
  const [dialTarget, setDialTarget] = useState("");
  const [dialKind, setDialKind] = useState<"voice" | "video">("voice");
  const [messageTarget, setMessageTarget] = useState("");
  const [messageText, setMessageText] = useState("");

  const eligible = useMemo(
    () => eligibleSipCredentials(credentialsQuery.data),
    [credentialsQuery.data],
  );
  const credentialsLoading = credentialsQuery.isLoading;
  const credentialId = reconcileSipCredentialSelection(
    credentialChoice,
    eligible,
  );

  const history = useMemo(
    () => collectPhoneHistory(messages, users, currentUserId),
    [messages, users, currentUserId],
  );
  const visibleHistory = useMemo(
    () => filterPhoneHistory(history, query),
    [history, query],
  );
  const externalCalls = useMemo(() => {
    const normalized = query.trim().toLowerCase();
    return Object.values(sip.calls)
      .filter((call) =>
        normalized === ""
          ? true
          : [call.target, call.remoteUri, call.localUri].some((value) =>
              value?.toLowerCase().includes(normalized),
            ),
      )
      .sort((a, b) => b.updatedAt - a.updatedAt);
  }, [sip.calls, query]);
  const externalMessages = useMemo(
    () => [...sip.messages].sort((a, b) => b.createdAt - a.createdAt),
    [sip.messages],
  );

  const closeDialog = () => {
    setDialog(null);
    setDialTarget("");
    setMessageTarget("");
    setMessageText("");
  };
  const openMessageDialog = (message?: ExternalSIPMessage) => {
    if (message !== undefined) {
      setMessageTarget(externalMessageName(message));
      if (message.credentialId !== undefined) {
        setCredentialChoice(
          reconcileSipCredentialSelection(message.credentialId, eligible),
        );
      }
    }
    setDialog("message");
  };
  const submitDial = (event: FormEvent) => {
    event.preventDefault();
    if (credentialsLoading || credentialId === "" || dialTarget.trim() === "") {
      return;
    }
    void sip.dial({
      credentialId,
      target: dialTarget.trim(),
      kind: dialKind,
    });
    closeDialog();
  };
  const submitMessage = (event: FormEvent) => {
    event.preventDefault();
    if (
      credentialsLoading ||
      credentialId === "" ||
      messageTarget.trim() === "" ||
      messageText.trim() === ""
    ) {
      return;
    }
    void sip.sendMessage({
      credentialId,
      target: messageTarget.trim(),
      text: messageText.trim(),
    });
    closeDialog();
  };

  const credentialOptions = eligible.map((credential) => (
    <MenuItem key={credential.id} value={credential.id}>
      {credential.sip_uri}
    </MenuItem>
  ));
  const noEligibleIdentity = !credentialsLoading && eligible.length === 0;

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
      <Tabs
        value={section}
        onChange={(_, value) => setSection(value as "calls" | "messages")}
        variant="fullWidth"
        aria-label={t("chat.externalSip.historyNavigation")}
        sx={{ minHeight: 40, borderBottom: 1, borderColor: "divider" }}
      >
        <Tab
          value="calls"
          label={t("chat.externalSip.calls")}
          sx={{ minHeight: 40, textTransform: "none" }}
        />
        <Tab
          value="messages"
          label={t("chat.externalSip.messages")}
          sx={{ minHeight: 40, textTransform: "none" }}
        />
      </Tabs>

      {section === "calls" ? (
        <>
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
            {visibleHistory.length === 0 && externalCalls.length === 0 ? (
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
                {externalCalls.map((call) => (
                  <ListItem key={`external:${call.callId}`} divider>
                    <ListItemAvatar sx={{ minWidth: 44 }}>
                      <Avatar sx={{ width: 36, height: 36 }}>
                        {call.kind === "video" ? (
                          <VideocamIcon fontSize="small" />
                        ) : (
                          <CallIcon fontSize="small" />
                        )}
                      </Avatar>
                    </ListItemAvatar>
                    <ListItemText
                      primary={externalCallName(call)}
                      secondary={
                        <>
                          {t(`chat.externalSip.direction.${call.direction}`)}
                          {" · "}
                          {t(`chat.externalSip.states.${call.state}`)}
                          {call.reason !== undefined && call.reason !== ""
                            ? ` · ${call.reason}`
                            : ""}
                        </>
                      }
                      slotProps={{
                        primary: { variant: "body2", noWrap: true },
                        secondary: { variant: "caption" },
                      }}
                    />
                  </ListItem>
                ))}
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
                          "&.Mui-selected:hover": {
                            bgcolor: "action.hover",
                          },
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
            onClick={() => setDialog("call")}
            sx={{ position: "absolute", right: 16, bottom: 16 }}
          >
            <AddIcon />
          </Fab>
        </>
      ) : (
        <>
          <Box sx={{ flexGrow: 1, minHeight: 0, overflowY: "auto" }}>
            {externalMessages.length === 0 ? (
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
                <SmsIcon sx={{ fontSize: 40, opacity: 0.6 }} />
                <Typography variant="body2">
                  {t("chat.externalSip.noMessages")}
                </Typography>
              </Box>
            ) : (
              <List disablePadding>
                {externalMessages.map((message) => {
                  const createdAt = new Date(message.createdAt * 1000);
                  return (
                    <ListItem
                      key={message.messageId}
                      divider
                      alignItems="flex-start"
                      secondaryAction={
                        <Button
                          size="small"
                          onClick={() => openMessageDialog(message)}
                        >
                          {t("chat.externalSip.reply")}
                        </Button>
                      }
                      sx={{ pr: 10 }}
                    >
                      <ListItemText
                        primary={message.text}
                        secondary={
                          <>
                            <Typography component="span" variant="caption">
                              {externalMessageName(message)}
                              {" · "}
                              {t(
                                `chat.externalSip.messageStates.${message.state}`,
                              )}
                              {" · "}
                              {formatDistanceToNow(createdAt, {
                                addSuffix: true,
                                locale: dateFnsLocaleFor(i18n.language),
                              })}
                            </Typography>
                            {message.reason !== undefined &&
                              message.reason !== "" && (
                                <Typography
                                  component="span"
                                  variant="caption"
                                  color="error"
                                  sx={{ display: "block" }}
                                >
                                  {message.reason}
                                </Typography>
                              )}
                          </>
                        }
                        slotProps={{
                          primary: {
                            variant: "body2",
                            sx: { whiteSpace: "pre-wrap", mt: 0.5 },
                          },
                          secondary: { component: "div", sx: { mt: 0.5 } },
                        }}
                      />
                    </ListItem>
                  );
                })}
              </List>
            )}
          </Box>
          <Fab
            color="primary"
            size="medium"
            aria-label={t("chat.externalSip.newMessage")}
            onClick={() => openMessageDialog()}
            sx={{ position: "absolute", right: 16, bottom: 16 }}
          >
            <SmsIcon />
          </Fab>
        </>
      )}

      <Dialog
        open={dialog === "call"}
        onClose={closeDialog}
        fullWidth
        maxWidth="xs"
      >
        <Box component="form" onSubmit={submitDial}>
          <DialogTitle>{t("chat.externalSip.dialTitle")}</DialogTitle>
          <DialogContent>
            {credentialsQuery.isError && (
              <Alert severity="error" sx={{ mb: 1 }}>
                {t("chat.sip.loadFailed")}
              </Alert>
            )}
            {noEligibleIdentity && (
              <Alert severity="info" sx={{ mb: 1 }}>
                {t("chat.externalSip.noEligibleIdentity")}
              </Alert>
            )}
            <TextField
              select
              fullWidth
              margin="dense"
              label={t("chat.externalSip.callerIdentity")}
              value={credentialId}
              onChange={(event) => setCredentialChoice(event.target.value)}
              disabled={credentialsLoading || noEligibleIdentity}
            >
              {credentialOptions}
            </TextField>
            <TextField
              autoFocus
              fullWidth
              margin="dense"
              label={t("chat.externalSip.target")}
              value={dialTarget}
              onChange={(event) => setDialTarget(event.target.value)}
              slotProps={{ htmlInput: { inputMode: "tel" } }}
            />
            <TextField
              select
              fullWidth
              margin="dense"
              label={t("chat.externalSip.callKind")}
              value={dialKind}
              onChange={(event) =>
                setDialKind(event.target.value as "voice" | "video")
              }
            >
              <MenuItem value="voice">{t("chat.externalSip.voice")}</MenuItem>
              <MenuItem value="video">
                {t("chat.externalSip.audioVideo")}
              </MenuItem>
            </TextField>
          </DialogContent>
          <DialogActions sx={{ px: 3, pb: 2 }}>
            <Button type="button" onClick={closeDialog}>
              {t("chat.phone.cancel")}
            </Button>
            <Button
              type="submit"
              variant="contained"
              disabled={
                credentialsLoading ||
                credentialId === "" ||
                dialTarget.trim() === ""
              }
            >
              {t("chat.phone.call")}
            </Button>
          </DialogActions>
        </Box>
      </Dialog>

      <Dialog
        open={dialog === "message"}
        onClose={closeDialog}
        fullWidth
        maxWidth="xs"
      >
        <Box component="form" onSubmit={submitMessage}>
          <DialogTitle>{t("chat.externalSip.messageTitle")}</DialogTitle>
          <DialogContent>
            {credentialsQuery.isError && (
              <Alert severity="error" sx={{ mb: 1 }}>
                {t("chat.sip.loadFailed")}
              </Alert>
            )}
            {noEligibleIdentity && (
              <Alert severity="info" sx={{ mb: 1 }}>
                {t("chat.externalSip.noEligibleIdentity")}
              </Alert>
            )}
            <TextField
              select
              fullWidth
              margin="dense"
              label={t("chat.externalSip.callerIdentity")}
              value={credentialId}
              onChange={(event) => setCredentialChoice(event.target.value)}
              disabled={credentialsLoading || noEligibleIdentity}
            >
              {credentialOptions}
            </TextField>
            <TextField
              autoFocus
              fullWidth
              margin="dense"
              label={t("chat.externalSip.target")}
              value={messageTarget}
              onChange={(event) => setMessageTarget(event.target.value)}
              slotProps={{ htmlInput: { inputMode: "tel" } }}
            />
            <TextField
              fullWidth
              multiline
              minRows={3}
              margin="dense"
              label={t("chat.externalSip.messageText")}
              value={messageText}
              onChange={(event) => setMessageText(event.target.value)}
            />
          </DialogContent>
          <DialogActions sx={{ px: 3, pb: 2 }}>
            <Button type="button" onClick={closeDialog}>
              {t("chat.phone.cancel")}
            </Button>
            <Button
              type="submit"
              variant="contained"
              disabled={
                credentialsLoading ||
                credentialId === "" ||
                messageTarget.trim() === "" ||
                messageText.trim() === ""
              }
            >
              {t("chat.externalSip.sendMessage")}
            </Button>
          </DialogActions>
        </Box>
      </Dialog>
    </Box>
  );
}
