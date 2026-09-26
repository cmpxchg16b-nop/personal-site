"use client";

// VideoWindow is one floating video view of a video call — the peer's
// camera, or our own — scoped to the chat app (not to the open
// conversation), like the incoming-call window: it stays visible while
// the user wanders the chat. A borderless FloatingCard: the video fills
// it, captioned with the peer's name (the peer view) or "me" (our own
// preview, mirrored like a mirror).

import { useEffect, useRef } from "react";
import { Box } from "@mui/material";
import { FloatingCard } from "./FloatingCard";

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

  return (
    <FloatingCard title={title} home={home} width={width}>
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
    </FloatingCard>
  );
}
