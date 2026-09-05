"use client";

import { useEffect, useRef, useState } from "react";
import { Box, Chip, Typography } from "@mui/material";
import { useTranslation } from "react-i18next";
import { WhepClient } from "@/api/whep";

// Pause between automatic reconnect attempts while the stream is offline,
// so a visit that outlives the broadcast keeps re-probing at a calm rate.
const RETRY_PAUSE_MS = 5000;

// How long the LIVE badge stays lit after the stream connects before it
// fades away; every (re)connection lights it again.
const BADGE_VISIBLE_MS = 3000;

type Status = "connecting" | "live" | "offline";

// WhepVideo plays one live stream read over WHEP (see src/api/whep.ts) in a
// 16:9 surface framed like the site's other bordered content. It owns the
// connection's whole lifecycle: connect on mount, tear down on unmount, and
// — because a live stream comes and goes — reconnect on its own after a
// pause while offline. The element
// starts muted so browsers allow autoplay; the native controls let the
// viewer unmute and go fullscreen.
export default function WhepVideo({ url }: { url: string }) {
  const { t } = useTranslation();
  const videoRef = useRef<HTMLVideoElement>(null);
  const [status, setStatus] = useState<Status>("connecting");
  // Bumped to (re)connect: the connection effect re-runs on every change.
  const [attempt, setAttempt] = useState(0);
  // The LIVE badge's lit state; the connection callback lights it, the
  // timer effect below fades it.
  const [badgeLit, setBadgeLit] = useState(false);

  useEffect(() => {
    const video = videoRef.current;
    if (video === null) return;
    let disposed = false;
    setStatus("connecting");

    const client = new WhepClient(url, {
      onStream: (stream) => {
        if (disposed) return;
        video.srcObject = stream;
        // Muted autoplay is gesture-free, so playing straight from this
        // network-triggered path is allowed.
        void video.play().catch((err) => {
          // Autoplay refused anyway: the viewer can press play (controls).
          console.debug("whepvideo: play() failed", err);
        });
      },
      onStateChange: (state) => {
        if (disposed) return;
        if (state === "connected") {
          setStatus("live");
          setBadgeLit(true);
        } else if (state === "failed" || state === "closed") {
          setStatus("offline");
          setBadgeLit(false);
        }
        // "disconnected" is left alone: ICE recovers on its own or slides
        // into "failed" soon after.
      },
    });
    client.start().catch((err) => {
      console.error("whepvideo: connection failed", err);
      client.close();
      if (!disposed) setStatus("offline");
    });

    return () => {
      disposed = true;
      client.close();
      video.srcObject = null;
    };
  }, [url, attempt]);

  // While offline, reconnect automatically after the pause.
  useEffect(() => {
    if (status !== "offline") return;
    const timer = setTimeout(() => setAttempt((a) => a + 1), RETRY_PAUSE_MS);
    return () => clearTimeout(timer);
  }, [status]);

  // The LIVE badge is a status cue, not permanent chrome: the connection
  // state callback lights it on every (re)connection; this timer fades it
  // out a few seconds later.
  useEffect(() => {
    if (!badgeLit) return;
    const timer = setTimeout(() => setBadgeLit(false), BADGE_VISIBLE_MS);
    return () => clearTimeout(timer);
  }, [badgeLit]);

  return (
    <Box
      sx={{
        position: "relative",
        // A calm cap on wide screens; full-bleed within the layout gutters
        // on narrow ones.
        maxWidth: 720,
        bgcolor: "black",
        border: 1,
        borderColor: "divider",
        borderRadius: 1,
        overflow: "hidden",
      }}
    >
      <Box
        component="video"
        ref={videoRef}
        autoPlay
        playsInline
        muted
        controls
        aria-label={t("live.videoLabel")}
        sx={{
          display: "block",
          width: "100%",
          // Fixed frame regardless of the stream's own aspect ratio or its
          // absence (nothing playing yet): objectFit letterboxes the video
          // inside it.
          aspectRatio: "16 / 9",
          objectFit: "contain",
        }}
      />
      {status === "live" ? (
        <Chip
          size="small"
          color="error"
          label={t("live.badge")}
          sx={{
            position: "absolute",
            top: 1,
            left: 1,
            // Never eat clicks meant for the video surface below.
            pointerEvents: "none",
            fontWeight: 600,
            // Lit for the first seconds of every (re)connection, then
            // faded out — see badgeLit.
            opacity: badgeLit ? 1 : 0,
            transition: "opacity 0.7s ease",
          }}
        />
      ) : (
        status === "offline" && (
          // The offline veil: the stream is unreachable right now. The
          // retry loop keeps probing in the background; a successful
          // reconnect lifts the veil by itself, so the veil carries no
          // controls of its own.
          <Box
            sx={{
              position: "absolute",
              inset: 0,
              display: "flex",
              alignItems: "center",
              justifyContent: "center",
              bgcolor: "rgba(0, 0, 0, 0.55)",
            }}
          >
            <Typography
              variant="body2"
              sx={{ color: "common.white", textAlign: "center", px: 2 }}
            >
              {t("live.offline")}
            </Typography>
          </Box>
        )
      )}
    </Box>
  );
}
