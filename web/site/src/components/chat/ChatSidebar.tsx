"use client";

import { useMemo, useState } from "react";
import {
  Box,
  InputAdornment,
  TextField,
  Typography,
  type SxProps,
  type Theme,
} from "@mui/material";
import SearchIcon from "@mui/icons-material/Search";
import { useTranslation } from "react-i18next";
import ConversationTree from "./ConversationTree";
import {
  conversationKey,
  type ChatChannel,
  type ConversationRef,
} from "./types";

type ChatSidebarProps = {
  channels: ChatChannel[];
  unread: Record<string, number>;
  selected: ConversationRef;
  onSelect: (ref: ConversationRef) => void;
  // Responsive visibility is controlled by the parent (ChatApp) through sx.
  sx?: SxProps<Theme>;
};

// ChatSidebar is the left-hand navigation panel: a header with the panel
// title and a filter box, then the channel/people tree. It owns two pieces
// of purely-local UI state — the search query and which channels are
// expanded; selection and unread counts are lifted up to ChatApp.
export default function ChatSidebar({
  channels,
  unread,
  selected,
  onSelect,
  sx,
}: ChatSidebarProps) {
  const { t } = useTranslation();
  const [query, setQuery] = useState("");
  // Rows absent from this record are treated as expanded.
  const [openChannels, setOpenChannels] = useState<Record<string, boolean>>({});

  const trimmedQuery = query.trim().toLowerCase();
  const searching = trimmedQuery !== "";

  // Filtering keeps a channel when its own name matches or any member's
  // does; in the latter case only the matching members are listed. While
  // searching, every surviving channel is forced open so matches stay
  // visible regardless of the collapsed state.
  const visibleChannels = useMemo(() => {
    if (!searching) return channels;
    return channels.flatMap((channel) => {
      if (channel.name.toLowerCase().includes(trimmedQuery)) {
        return [channel];
      }
      const members = channel.members.filter((member) =>
        member.name.toLowerCase().includes(trimmedQuery),
      );
      return members.length > 0 ? [{ ...channel, members }] : [];
    });
  }, [channels, searching, trimmedQuery]);

  const openForRender = useMemo(() => {
    if (!searching) return openChannels;
    return Object.fromEntries(visibleChannels.map((c) => [c.id, true]));
  }, [searching, openChannels, visibleChannels]);

  const toggleChannel = (channelId: string) =>
    setOpenChannels((prev) => ({
      ...prev,
      [channelId]: !(prev[channelId] ?? true),
    }));

  return (
    <Box
      component="nav"
      aria-label={t("chat.title")}
      sx={[
        {
          width: { xs: "100%", sm: 248, md: 288 },
          flexShrink: 0,
          borderRight: 1,
          borderColor: "divider",
          display: "flex",
          flexDirection: "column",
          minHeight: 0,
        },
        ...(Array.isArray(sx) ? sx : [sx]),
      ]}
    >
      <Box sx={{ px: 2, pt: 2, pb: 1.5 }}>
        <TextField
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder={t("chat.search")}
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
            htmlInput: { "aria-label": t("chat.search") },
          }}
        />
      </Box>
      <Box sx={{ flexGrow: 1, overflowY: "auto", minHeight: 0 }}>
        <ConversationTree
          channels={visibleChannels}
          openChannels={openForRender}
          onToggleChannel={toggleChannel}
          selectedKey={conversationKey(selected)}
          onSelect={onSelect}
          unread={unread}
        />
        {searching && visibleChannels.length === 0 && (
          <Typography
            variant="body2"
            color="text.secondary"
            sx={{ px: 2, py: 1 }}
          >
            {t("chat.noMatch")}
          </Typography>
        )}
      </Box>
    </Box>
  );
}
