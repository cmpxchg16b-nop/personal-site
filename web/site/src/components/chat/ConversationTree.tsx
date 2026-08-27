"use client";

import { Fragment } from "react";
import {
  Box,
  Collapse,
  List,
  ListItem,
  ListItemButton,
  ListItemText,
  Typography,
} from "@mui/material";
import TagIcon from "@mui/icons-material/Tag";
import ExpandMoreIcon from "@mui/icons-material/ExpandMore";
import ChevronRightIcon from "@mui/icons-material/ChevronRight";
import PhoneInTalkIcon from "@mui/icons-material/PhoneInTalk";
import RingVolumeIcon from "@mui/icons-material/RingVolume";
import { useTranslation } from "react-i18next";
import type { ChatChannel } from "@/api/ss/types";
import UserAvatar from "./UserAvatar";
import {
  conversationKey,
  type ActivePhoneCall,
  type ConversationRef,
} from "./types";

type ConversationTreeProps = {
  channels: ChatChannel[];
  // Which channel rows are expanded, keyed by channel id. Rows missing from
  // the record count as expanded.
  openChannels: Record<string, boolean>;
  onToggleChannel: (channelId: string) => void;
  // Key of the selected conversation (see conversationKey), or null when
  // none; compared by key, so the open DM highlights in the channel it
  // was opened from.
  selectedKey: string | null;
  onSelect: (ref: ConversationRef) => void;
  // Unread counts by conversation key; zero/undefined renders nothing.
  unread: Record<string, number>;
  // Live voice calls by conversation key (see usePhoneCalls): a member
  // entry shows a ringing / in-call pill for them. Purely informative —
  // calls are answered from the global popup, not here.
  calls: Record<string, ActivePhoneCall>;
};

// UnreadBadge is the small pill showing a conversation's unseen count.
function UnreadBadge({ count }: { count: number }) {
  return (
    <Box
      component="span"
      sx={{
        ml: 1,
        px: 0.75,
        height: 18,
        minWidth: 18,
        borderRadius: "9999px",
        bgcolor: "primary.main",
        color: "primary.contrastText",
        fontSize: 11,
        fontWeight: 600,
        display: "inline-flex",
        alignItems: "center",
        justifyContent: "center",
        flexShrink: 0,
      }}
    >
      {count}
    </Box>
  );
}

// CallStatusPill is the member row's voice-call state: a ringing pill
// (pulsing) while the call is inviting either way, an in-call pill once
// accepted.
function CallStatusPill({ call }: { call: ActivePhoneCall }) {
  const { t } = useTranslation();
  const ringing = call.status === "inviting";
  const Icon = ringing ? RingVolumeIcon : PhoneInTalkIcon;
  const label = ringing
    ? call.incoming
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

// ConversationTree renders the two-level navigation: channel rooms first,
// each expandable to reveal its members as direct-message entries. It is
// purely presentational — expansion state and selection live with the
// parent.
export default function ConversationTree({
  channels,
  openChannels,
  onToggleChannel,
  selectedKey,
  onSelect,
  unread,
  calls,
}: ConversationTreeProps) {
  return (
    // Full-bleed rows: no horizontal padding on the list, no rounding on
    // the buttons — selection/hover paint edge to edge.
    <List disablePadding sx={{ pb: 1 }}>
      {channels.map((channel) => {
        const open = openChannels[channel.id] ?? true;
        return (
          <Fragment key={channel.id}>
            <ListItem disableGutters disablePadding>
              {/* Channels only group people — there is no channel-level chat
                  at this moment, so the row's only job is toggling the
                  member list. The chevron is a passive state indicator. */}
              <ListItemButton
                onClick={() => onToggleChannel(channel.id)}
                aria-expanded={open}
                sx={{ px: 2, py: 0.75 }}
              >
                <TagIcon
                  fontSize="small"
                  sx={{ color: "text.secondary", mr: 1, flexShrink: 0 }}
                />
                <ListItemText
                  primary={channel.name}
                  slotProps={{
                    primary: {
                      variant: "body2",
                      noWrap: true,
                      sx: { fontWeight: 500 },
                    },
                  }}
                />
                {open ? (
                  <ExpandMoreIcon
                    fontSize="small"
                    sx={{ ml: 1, color: "text.secondary", flexShrink: 0 }}
                  />
                ) : (
                  <ChevronRightIcon
                    fontSize="small"
                    sx={{ ml: 1, color: "text.secondary", flexShrink: 0 }}
                  />
                )}
              </ListItemButton>
            </ListItem>
            <Collapse in={open} timeout="auto">
              <List disablePadding>
                {channel.members.map((member) => {
                  const dmRef: ConversationRef = {
                    kind: "dm",
                    channelId: channel.id,
                    userId: member.id,
                  };
                  const dmUnread = unread[conversationKey(dmRef)] ?? 0;
                  const call = calls[conversationKey(dmRef)];
                  return (
                    <ListItem key={member.id} disableGutters disablePadding>
                      <ListItemButton
                        selected={conversationKey(dmRef) === selectedKey}
                        onClick={() => onSelect(dmRef)}
                        sx={{ py: 0.5, pl: 3, pr: 2 }}
                      >
                        <UserAvatar user={member} size={22} showPresence />
                        <ListItemText
                          primary={member.name}
                          sx={{ ml: 1.25 }}
                          slotProps={{
                            primary: { variant: "body2", noWrap: true },
                          }}
                        />
                        {call !== undefined && <CallStatusPill call={call} />}
                        {dmUnread > 0 && <UnreadBadge count={dmUnread} />}
                      </ListItemButton>
                    </ListItem>
                  );
                })}
              </List>
            </Collapse>
          </Fragment>
        );
      })}
    </List>
  );
}
