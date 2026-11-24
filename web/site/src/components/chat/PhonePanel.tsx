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
  MenuItem,
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
import {
  isExternalSIPCallActive,
  type ExternalSIPCall,
  type UseExternalSIPResult,
} from "@/api/ss/sip";
import { useSipCredentials } from "@/hooks/useSipCredentials";
import { dateFnsLocaleFor, localeTagFor } from "@/i18n";
import { collectPhoneHistory, filterPhoneHistory } from "./phoneHistory";
import {
  externalSIPConversationForCall,
  externalSIPMessageOnlyConversations,
  sameExternalSIPConversation,
  type ExternalSIPConversationRef,
} from "./externalSipConversation";
import {
  eligibleSipCredentials,
  reconcileSipCredentialSelection,
} from "./sipForm";
import type { ChatMessage, ConversationRef, PhoneCallMessage } from "./types";
import UserAvatar from "./UserAvatar";
import { CallStatusPill } from "./CallStatusPill";

function statusText(
  status: PhoneCallMessage["phoneStatus"],
  outgoing: boolean,
  t: TFunction,
): string {
  switch (status) {
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
    case "failed":
      return t("chat.externalSip.states.failed");
  }
}

function externalCallName(call: ExternalSIPCall): string {
  return call.remoteUri ?? call.target ?? call.callId;
}

function externalTerminalStatus(call: ExternalSIPCall, t: TFunction): string {
  const outgoing = call.direction === "outgoing";
  switch (call.state) {
    case "ended":
    case "rejected":
    case "cancelled":
    case "failed":
      return statusText(call.state, outgoing, t);
    default:
      return "";
  }
}

const historyActionSx = {
  px: 2,
  py: 1.25,
  bgcolor: "transparent",
  "&:hover": { bgcolor: "action.hover" },
  "&.Mui-selected": { bgcolor: "action.hover" },
  "&.Mui-selected:hover": { bgcolor: "action.hover" },
} as const;

type PhonePanelProps = {
  messages: Record<string, ChatMessage[]>;
  users: Record<string, ChatUser>;
  currentUserId: string;
  selectedMessageId: string | null;
  onSelectContact: (ref: ConversationRef, messageId: string) => void;
  onOpenExternalConversation: (ref: ExternalSIPConversationRef) => void;
  selectedExternalConversation: ExternalSIPConversationRef | null;
  sip: UseExternalSIPResult;
};

// Phone history is the entry point to both live and finished calls. SIP
// MESSAGEs deliberately have no separate inbox here: they live in the peer's
// chat, alongside the live call panel and media.
export default function PhonePanel({
  messages,
  users,
  currentUserId,
  selectedMessageId,
  onSelectContact,
  onOpenExternalConversation,
  selectedExternalConversation,
  sip,
}: PhonePanelProps) {
  const { t, i18n } = useTranslation();
  const credentialsQuery = useSipCredentials();
  const [dialOpen, setDialOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [credentialChoice, setCredentialChoice] = useState("");
  const [dialTarget, setDialTarget] = useState("");
  const [dialKind, setDialKind] = useState<"voice" | "video">("voice");

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
      .sort((a, b) => b.since - a.since);
  }, [sip.calls, query]);
  const externalMessageConversations = useMemo(() => {
    const normalized = query.trim().toLowerCase();
    return externalSIPMessageOnlyConversations({
      calls: sip.calls,
      messages: sip.messages,
    }).filter(({ ref, latestMessage }) => {
      if (normalized === "") return true;
      return [ref.target, latestMessage.remoteUri, latestMessage.localUri].some(
        (value) => value?.toLowerCase().includes(normalized),
      );
    });
  }, [sip.calls, sip.messages, query]);
  const selectedExternalCallId =
    selectedExternalConversation === null
      ? null
      : (externalCalls.find((call) => {
          const ref = externalSIPConversationForCall(call);
          return (
            ref !== null &&
            sameExternalSIPConversation(
              ref,
              selectedExternalConversation,
              call.localUri,
            )
          );
        })?.callId ?? null);

  const closeDialog = () => {
    setDialOpen(false);
    setDialTarget("");
  };
  const submitDial = (event: FormEvent) => {
    event.preventDefault();
    if (credentialsLoading || credentialId === "" || dialTarget.trim() === "") {
      return;
    }
    const target = dialTarget.trim();
    void sip.dial({ credentialId, target, kind: dialKind });
    // Dial confirmation opens chat immediately, before the peer answers.
    onOpenExternalConversation({ credentialId, target });
    closeDialog();
  };

  const noEligibleIdentity = !credentialsLoading && eligible.length === 0;

  return (
    <Box
      sx={{
        position: "relative",
        flexGrow: 1,
        minWidth: 0,
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
        {visibleHistory.length === 0 &&
        externalCalls.length === 0 &&
        externalMessageConversations.length === 0 ? (
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
            {externalCalls.map((call) => {
              const name = externalCallName(call);
              const user: ChatUser = {
                id: `external:${call.credentialId ?? ""}:${name}`,
                name,
                online: true,
                visible: true,
              };
              const startedAt = new Date(call.since);
              const relative = formatDistanceToNow(startedAt, {
                addSuffix: true,
                locale: dateFnsLocaleFor(i18n.language),
              });
              const ref = externalSIPConversationForCall(call);
              const live = isExternalSIPCallActive(call);
              const status = externalTerminalStatus(call, t);
              const secondary = live
                ? relative
                : `${status}${call.reason ? ` · ${call.reason}` : ""} · ${relative}`;
              return (
                <ListItem
                  key={`external:${call.callId}`}
                  divider
                  disablePadding
                >
                  <ListItemButton
                    selected={call.callId === selectedExternalCallId}
                    onClick={() => {
                      if (ref !== null) onOpenExternalConversation(ref);
                    }}
                    aria-label={t("chat.phone.viewContact", { name })}
                    title={startedAt.toLocaleString(
                      localeTagFor(i18n.language),
                    )}
                    sx={historyActionSx}
                  >
                    <ListItemAvatar sx={{ minWidth: 44 }}>
                      <UserAvatar user={user} size={36} />
                    </ListItemAvatar>
                    <ListItemText
                      primary={name}
                      secondary={secondary}
                      slotProps={{
                        primary: { variant: "body2", noWrap: true },
                        secondary: { variant: "caption", noWrap: true },
                      }}
                    />
                    {live && (
                      <CallStatusPill
                        status={
                          call.state === "accepted" ? "accepted" : "inviting"
                        }
                        incoming={call.direction === "incoming"}
                      />
                    )}
                  </ListItemButton>
                </ListItem>
              );
            })}
            {externalMessageConversations.map(({ ref, latestMessage }) => {
              const name = latestMessage.remoteUri ?? ref.target;
              const user: ChatUser = {
                id: `external:message:${ref.credentialId}:${name}`,
                name,
                online: true,
                visible: true,
              };
              const createdAt = new Date(latestMessage.createdAt * 1000);
              const relative = formatDistanceToNow(createdAt, {
                addSuffix: true,
                locale: dateFnsLocaleFor(i18n.language),
              });
              return (
                <ListItem
                  key={`external-message:${ref.credentialId}:${ref.target}`}
                  divider
                  disablePadding
                >
                  <ListItemButton
                    selected={
                      selectedExternalConversation !== null &&
                      sameExternalSIPConversation(
                        ref,
                        selectedExternalConversation,
                        latestMessage.localUri,
                      )
                    }
                    onClick={() => onOpenExternalConversation(ref)}
                    aria-label={t("chat.phone.viewContact", { name })}
                    title={createdAt.toLocaleString(
                      localeTagFor(i18n.language),
                    )}
                    sx={historyActionSx}
                  >
                    <ListItemAvatar sx={{ minWidth: 44 }}>
                      <UserAvatar user={user} size={36} />
                    </ListItemAvatar>
                    <ListItemText
                      primary={name}
                      secondary={`${latestMessage.text} · ${relative}`}
                      slotProps={{
                        primary: { variant: "body2", noWrap: true },
                        secondary: { variant: "caption", noWrap: true },
                      }}
                    />
                  </ListItemButton>
                </ListItem>
              );
            })}
            {visibleHistory.map(({ ref, message, user, outgoing }) => {
              const sentAt = new Date(message.timestamp * 1000);
              const relative = formatDistanceToNow(sentAt, {
                addSuffix: true,
                locale: dateFnsLocaleFor(i18n.language),
              });
              const absolute = sentAt.toLocaleString(
                localeTagFor(i18n.language),
              );
              const live =
                message.phoneStatus === "inviting" ||
                message.phoneStatus === "accepted";
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
                    sx={historyActionSx}
                  >
                    <ListItemAvatar sx={{ minWidth: 44 }}>
                      <UserAvatar user={user} size={36} />
                    </ListItemAvatar>
                    <ListItemText
                      primary={user.name}
                      secondary={
                        live
                          ? relative
                          : `${statusText(message.phoneStatus, outgoing, t)} · ${relative}`
                      }
                      slotProps={{
                        primary: { variant: "body2", noWrap: true },
                        secondary: { variant: "caption", noWrap: true },
                      }}
                    />
                    {live && (
                      <CallStatusPill
                        status={message.phoneStatus as "inviting" | "accepted"}
                        incoming={!outgoing}
                      />
                    )}
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
        onClick={() => setDialOpen(true)}
        sx={{ position: "absolute", right: 16, bottom: 16 }}
      >
        <AddIcon />
      </Fab>

      <Dialog open={dialOpen} onClose={closeDialog} fullWidth maxWidth="xs">
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
              {eligible.map((credential) => (
                <MenuItem key={credential.id} value={credential.id}>
                  {credential.sip_uri}
                </MenuItem>
              ))}
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
    </Box>
  );
}
