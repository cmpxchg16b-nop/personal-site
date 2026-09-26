"use client";

// FloatingCard is the chat app's draggable card shell — the video
// windows' and the dial pad's shared chrome: a fixed-position Paper at a
// parameterized home position, draggable anywhere with the pointer (the
// drag offset is from home), captioned by a strip holding the drag
// affordance, the title, and optional action buttons (the dial pad's
// close). A drag never starts on a button, so a card carrying
// interactive content (the pad's keys) stays clickable while the rest
// of the card still drags.

import { useRef, useState } from "react";
import type { PointerEvent as ReactPointerEvent, ReactNode } from "react";
import { Box, Paper, Typography } from "@mui/material";
import DragIndicatorIcon from "@mui/icons-material/DragIndicator";

type FloatingCardProps = {
  // The caption strip's text.
  title: string;
  // The card's home position (fixed positioning) before any drag.
  home: { top?: number; right?: number; bottom?: number; left?: number };
  // The card's width in pixels.
  width?: number;
  // Caption-strip actions (e.g. the dial pad's close button).
  actions?: ReactNode;
  children: ReactNode;
};

export function FloatingCard({
  title,
  home,
  width = 280,
  actions,
  children,
}: FloatingCardProps) {
  // The drag offset from the window's home position, in pixels.
  const [offset, setOffset] = useState({ x: 0, y: 0 });
  const dragRef = useRef<
    { pointerId: number; fromX: number; fromY: number } | undefined
  >(undefined);

  const onDragStart = (e: ReactPointerEvent<HTMLElement>) => {
    // A press on a button (the pad's keys, the close) is not a drag.
    if ((e.target as HTMLElement).closest("button") !== null) return;
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

  return (
    <Paper
      elevation={8}
      onPointerDown={onDragStart}
      onPointerMove={onDragMove}
      onPointerUp={onDragEnd}
      onPointerCancel={onDragEnd}
      sx={{
        position: "fixed",
        ...home,
        zIndex: (theme) => theme.zIndex.modal,
        width,
        borderRadius: 2,
        overflow: "hidden",
        transform: `translate(${offset.x}px, ${offset.y}px)`,
        cursor: "grab",
        touchAction: "none",
      }}
    >
      {/* The caption strip: the drag affordance, the title, the card's
          action buttons. */}
      <Box
        sx={{
          display: "flex",
          alignItems: "center",
          gap: 0.5,
          px: 1,
          py: 0.25,
          bgcolor: "action.hover",
          color: "text.secondary",
        }}
      >
        <DragIndicatorIcon fontSize="small" />
        <Typography
          variant="caption"
          noWrap
          sx={{ fontWeight: 600, flexGrow: 1 }}
        >
          {title}
        </Typography>
        {actions}
      </Box>
      {children}
    </Paper>
  );
}
