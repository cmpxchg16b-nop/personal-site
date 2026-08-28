"use client";

// VideoWindow is one floating video view of a video call — the peer's
// camera, or our own — scoped to the chat app (not to the open
// conversation), like the incoming-call window: it stays visible while
// the user wanders the chat. A borderless card: the video fills it,
// captioned with the peer's name (the peer view) or "me" (our own
// preview, mirrored like a mirror). Drag it by its caption strip.

import { useEffect, useRef, useState } from "react";
import type { PointerEvent as ReactPointerEvent } from "react";
import { Box, Paper, Typography } from "@mui/material";
import DragIndicatorIcon from "@mui/icons-material/DragIndicator";

type VideoWindowProps = {
  // The caption strip's text: the peer's name, or "me" for our own.
  title: string;
  // The camera stream to show. It holds a video track only — the call's
  // audio flows through the audio graph (its volume controls apply), so
  // the element is muted to never double it.
  stream: MediaStream;
  // Mirror the video horizontally — the self-view convention.
  mirrored?: boolean;
  // The window's home position (fixed positioning) before any drag.
  home: { top?: number; right?: number; bottom?: number; left?: number };
  // The card's width in pixels; the video's height follows the stream's
  // aspect ratio.
  width?: number;
};

export function VideoWindow({
  title,
  stream,
  mirrored = false,
  home,
  width = 280,
}: VideoWindowProps) {
  // The drag offset from the window's home position, in pixels.
  const [offset, setOffset] = useState({ x: 0, y: 0 });
  const dragRef = useRef<
    { pointerId: number; fromX: number; fromY: number } | undefined
  >(undefined);
  const videoRef = useRef<HTMLVideoElement>(null);

  useEffect(() => {
    const video = videoRef.current;
    if (video === null) return;
    video.srcObject = stream;
    // Muted autoplay is gesture-free, so playing straight from this
    // network-triggered path is allowed.
    void video
      .play()
      .catch((err) => console.error("videowindow: play() failed", err));
    return () => {
      video.srcObject = null;
    };
  }, [stream]);

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

  return (
    <Paper
      elevation={8}
      sx={{
        position: "fixed",
        ...home,
        zIndex: (theme) => theme.zIndex.modal,
        width,
        borderRadius: 2,
        overflow: "hidden",
        transform: `translate(${offset.x}px, ${offset.y}px)`,
      }}
    >
      {/* The caption strip doubles as the drag handle. */}
      <Box
        onPointerDown={onDragStart}
        onPointerMove={onDragMove}
        onPointerUp={onDragEnd}
        onPointerCancel={onDragEnd}
        sx={{
          display: "flex",
          alignItems: "center",
          gap: 0.5,
          px: 1,
          py: 0.25,
          bgcolor: "action.hover",
          cursor: "grab",
          touchAction: "none",
          color: "text.secondary",
        }}
      >
        <DragIndicatorIcon fontSize="small" />
        <Typography variant="caption" noWrap sx={{ fontWeight: 600 }}>
          {title}
        </Typography>
      </Box>
      <Box
        component="video"
        ref={videoRef}
        autoPlay
        playsInline
        muted
        sx={{
          display: "block",
          width: "100%",
          bgcolor: "black",
          transform: mirrored ? "scaleX(-1)" : undefined,
        }}
      />
    </Paper>
  );
}
