"use client";

import { useMemo, useState } from "react";
import {
  Box,
  InputAdornment,
  Tab,
  Tabs,
  TextField,
  Typography,
  type SxProps,
  type Theme,
} from "@mui/material";
import ChatBubbleOutlineIcon from "@mui/icons-material/ChatBubbleOutlineOutlined";
import PhoneOutlinedIcon from "@mui/icons-material/PhoneOutlined";
import SearchIcon from "@mui/icons-material/Search";
import SettingsOutlinedIcon from "@mui/icons-material/SettingsOutlined";
import { useTranslation } from "react-i18next";
import type { ChatChannel, ChatUser } from "@/api/ss/types";
import ConversationTree from "./ConversationTree";
import PhonePanel from "./PhonePanel";
import SettingsPanel from "./SettingsPanel";
import {
  conversationKey,
  type ActivePhoneCall,
  type ChatMessage,
  type ConversationRef,
} from "./types";

export type ChatSidebarTab = "chat" | "phone" | "settings";

type ChatSidebarProps = {
  channels: ChatChannel[];
  unread: Record<string, number>;
  // The open conversation, or null when none is selected.
  selected: ConversationRef | null;
  onSelect: (ref: ConversationRef) => void;
  // Live voice calls by conversation key (see usePhoneCalls); the member
  // entries display their ringing / in-call pills.
  calls: Record<string, ActivePhoneCall>;
  tab: ChatSidebarTab;
  onTabChange: (tab: ChatSidebarTab) => void;
  messages: Record<string, ChatMessage[]>;
  users: Record<string, ChatUser>;
  currentUserId: string;
  selectedCallId: string | null;
  onSelectContact: (ref: ConversationRef, messageId: string) => void;
  sipSelected: boolean;
  onOpenSip: () => void;
  // Responsive visibility is controlled by the parent (ChatApp) through sx.
  sx?: SxProps<Theme>;
};

// ChatSidebar combines the selected tab's content with a Telegram-style
// bottom navigation bar. Search and channel expansion remain local to the Chat
// content; ChatApp owns the active tab because Settings can replace the right pane.
export default function ChatSidebar({
  channels,
  unread,
  selected,
  onSelect,
  calls,
  tab,
  onTabChange,
  messages,
  users,
  currentUserId,
  selectedCallId,
  onSelectContact,
  sipSelected,
  onOpenSip,
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
          width: { xs: "100%", sm: 288, md: 320 },
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
      <Tabs
        variant="fullWidth"
        value={tab}
        onChange={(_, value) => onTabChange(value as ChatSidebarTab)}
        aria-label={t("chat.navigation")}
        sx={{
          order: 2,
          width: "100%",
          minHeight: 44,
          flexShrink: 0,
          borderTop: 1,
          borderColor: "divider",
          "& .MuiTabs-indicator": { display: "none" },
          "& .MuiTab-root": {
            minWidth: 0,
            minHeight: 44,
            px: 0.25,
            py: 0.5,
            gap: 0.25,
            color: "text.secondary",
            fontSize: 11,
            fontWeight: 500,
            textTransform: "none",
          },
          "& .MuiTab-root.Mui-selected": {
            color: "primary.main",
            bgcolor: "action.selected",
          },
        }}
      >
        <Tab
          value="chat"
          icon={<ChatBubbleOutlineIcon fontSize="small" />}
          aria-label={t("chat.tabs.chat")}
          aria-controls="chat-sidebar-chat"
        />
        <Tab
          value="phone"
          icon={<PhoneOutlinedIcon fontSize="small" />}
          aria-label={t("chat.tabs.phone")}
          aria-controls="chat-sidebar-phone"
        />
        <Tab
          value="settings"
          icon={<SettingsOutlinedIcon fontSize="small" />}
          aria-label={t("chat.tabs.settings")}
          aria-controls="chat-sidebar-settings"
        />
      </Tabs>
      <Box
        sx={{
          order: 1,
          flexGrow: 1,
          minWidth: 0,
          minHeight: 0,
          display: "flex",
          flexDirection: "column",
        }}
      >
        {tab === "chat" && (
          <Box
            id="chat-sidebar-chat"
            role="tabpanel"
            sx={{
              flexGrow: 1,
              minHeight: 0,
              display: "flex",
              flexDirection: "column",
            }}
          >
            <Box
              sx={{
                px: 2,
                pt: 2,
                pb: 1.5,
                display: "flex",
                alignItems: "center",
              }}
            >
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
                selectedKey={
                  selected === null ? null : conversationKey(selected)
                }
                onSelect={onSelect}
                unread={unread}
                calls={calls}
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
        )}
        {tab === "phone" && (
          <Box
            id="chat-sidebar-phone"
            role="tabpanel"
            sx={{ flexGrow: 1, minHeight: 0, display: "flex" }}
          >
            <PhonePanel
              messages={messages}
              users={users}
              currentUserId={currentUserId}
              selectedMessageId={selectedCallId}
              onSelectContact={onSelectContact}
            />
          </Box>
        )}
        {tab === "settings" && (
          <Box
            id="chat-sidebar-settings"
            role="tabpanel"
            sx={{ flexGrow: 1, minHeight: 0, overflowY: "auto" }}
          >
            <SettingsPanel
              user={users[currentUserId]}
              sipSelected={sipSelected}
              onOpenSip={onOpenSip}
            />
          </Box>
        )}
      </Box>
    </Box>
  );
}
