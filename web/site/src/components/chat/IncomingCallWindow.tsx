"use client";

// IncomingCallWindow is the global incoming-call popup: a small floating,
// draggable window shown anywhere in the chat page while a call is
// ringing for us — the one place calls are accepted or declined (the
// sidebar and the conversation only display call state). Drag it by its
// header strip; the buttons are never drag starts.

import { useRef, useState } from "react";
import type { PointerEvent as ReactPointerEvent } from "react";
import { Avatar, Box, IconButton, Paper, Typography } from "@mui/material";
import CallIcon from "@mui/icons-material/Call";
import CallEndIcon from "@mui/icons-material/CallEnd";
import DragIndicatorIcon from "@mui/icons-material/DragIndicator";
import { useTranslation } from "react-i18next";
import type { ChatUser } from "@/api/ss/types";
import UserAvatar from "./UserAvatar";
import type { ActivePhoneCall } from "./types";

type IncomingCallWindowProps = {
  // The ringing call to answer, or null when none is (the window hides).
  call: ActivePhoneCall | null;
  // The caller, when known — for the avatar and the name.
  caller: ChatUser | undefined;
  onAccept: (call: ActivePhoneCall) => void;
  onReject: (call: ActivePhoneCall) => void;
};

export function IncomingCallWindow({
  call,
  caller,
  onAccept,
  onReject,
}: IncomingCallWindowProps) {
  const { t } = useTranslation();
  // The drag offset from the window's home position (top-right, below
  // the top bar), in pixels.
  const [offset, setOffset] = useState({ x: 0, y: 0 });
  const dragRef = useRef<
    { pointerId: number; fromX: number; fromY: number } | undefined
  >(undefined);

  const onDragStart = (e: ReactPointerEvent<HTMLElement>) => {
    dragRef.current = {
      pointerId: e.pointerId,
      fromX: e.clientX - offset.x,
      fromY: e.clientY - offset.y,
    };
    e.currentTarget.setPointerCapture(e.pointerId);
  };
  const onDragMove = (e: ReactPointerEvent<HTMLElement>) => {
    const drag = dragRef.current;
    if (drag === undefined || drag.pointerId !== e.pointerId) return;
    setOffset({ x: e.clientX - drag.fromX, y: e.clientY - drag.fromY });
  };
  const onDragEnd = (e: ReactPointerEvent<HTMLElement>) => {
    if (dragRef.current?.pointerId !== e.pointerId) return;
    dragRef.current = undefined;
    e.currentTarget.releasePointerCapture(e.pointerId);
  };

  if (call === null) return null;

  return (
    <Paper
      elevation={8}
      sx={{
        position: "fixed",
        top: 72,
        right: 24,
        zIndex: (theme) => theme.zIndex.modal,
        width: 264,
        borderRadius: 2,
        overflow: "hidden",
        transform: `translate(${offset.x}px, ${offset.y}px)`,
      }}
    >
      {/* The drag handle strip. */}
      <Box
        onPointerDown={onDragStart}
        onPointerMove={onDragMove}
        onPointerUp={onDragEnd}
        onPointerCancel={onDragEnd}
        sx={{
          display: "flex",
          alignItems: "center",
          justifyContent: "center",
          py: 0.25,
          bgcolor: "action.hover",
          cursor: "grab",
          touchAction: "none",
          color: "text.secondary",
        }}
      >
        <DragIndicatorIcon fontSize="small" />
      </Box>
      <Box
        sx={{
          p: 2,
          display: "flex",
          flexDirection: "column",
          alignItems: "center",
          gap: 1,
        }}
      >
        {caller !== undefined ? (
          <UserAvatar user={caller} size={48} />
        ) : (
          <Avatar sx={{ width: 48, height: 48 }} />
        )}
        <Typography variant="subtitle2" noWrap>
          {caller?.name ?? call.ref.userId}
        </Typography>
        <Typography variant="caption" color="text.secondary">
          {t("chat.call.incoming")}
        </Typography>
        <Box sx={{ display: "flex", gap: 3, mt: 0.5 }}>
          <IconButton
            onClick={() => onReject(call)}
            aria-label={t("chat.call.decline")}
            sx={{
              bgcolor: "error.main",
              color: "error.contrastText",
              "&:hover": { bgcolor: "error.dark" },
            }}
          >
            <CallEndIcon />
          </IconButton>
          <IconButton
            onClick={() => onAccept(call)}
            aria-label={t("chat.call.accept")}
            sx={{
              bgcolor: "success.main",
              color: "success.contrastText",
              "&:hover": { bgcolor: "success.dark" },
            }}
          >
            <CallIcon />
          </IconButton>
        </Box>
      </Box>
    </Paper>
  );
}
