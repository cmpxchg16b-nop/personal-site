"use client";

import { useEffect, useRef, useState } from "react";
import { Box, Typography } from "@mui/material";
import { useTranslation } from "react-i18next";
import Hls from "hls.js";

// Pause between full retry attempts after a fatal failure — the same calm
// rate as WhepVideo's reconnect loop, since a live stream's playlist 404s
// while nobody is publishing.
const RETRY_PAUSE_MS = 5000;

// "connecting": the first attempt has yet to start playback; "playing":
// playback is (or was) running — hls.js recovers its own non-fatal stalls;
// "retrying": a fatal error tore the attempt down and a full retry is
// scheduled; "unsupported": the browser can play HLS no way at all, so no
// retry is ever scheduled.
type Status = "connecting" | "playing" | "retrying" | "unsupported";

// HLSPlayer plays one m3u8 playlist in the same framed 16:9 surface
// WhepVideo renders: black, bordered, capped on wide screens, native
// controls, muted so autoplay is allowed. Two playback paths, chosen at
// mount: where Media Source Extensions exist, hls.js drives the video
// element; where they don't but the element speaks HLS natively (Safari),
// the playlist URL goes straight onto the element. It owns the playback's
// whole lifecycle: attach on mount, tear down on unmount, and — because a
// live stream comes and goes — retry on its own after a pause when a fatal
// error ends an attempt.
export default function HLSPlayer({ url }: { url: string }) {
  const { t } = useTranslation();
  const videoRef = useRef<HTMLVideoElement>(null);
  const [status, setStatus] = useState<Status>("connecting");
  // Bumped to retry from scratch: the playback effect re-runs on every
  // change.
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const video = videoRef.current;
    if (video === null) return;
    let disposed = false;
    setStatus("connecting");

    // The element's own "playing" event is the single source of the lit
    // state, so it covers the MSE path, the native path, and a manual start
    // through the native controls alike.
    const onPlaying = () => {
      if (!disposed) setStatus("playing");
    };
    video.addEventListener("playing", onPlaying);

    // playOnce starts muted autoplay (gesture-free, so allowed); a refusal
    // leaves the native controls to start playback by hand.
    const playOnce = () => {
      void video.play().catch((err) => {
        console.debug("hlsplayer: play() failed", err);
      });
    };
    // Set only on the native path, for cleanup.
    let onLoadedMetadata: (() => void) | null = null;

    let hls: Hls | null = null;
    if (Hls.isSupported()) {
      const h = new Hls();
      hls = h;
      h.on(Hls.Events.MANIFEST_PARSED, playOnce);
      h.on(Hls.Events.ERROR, (_event, data) => {
        if (disposed || !data.fatal) return;
        if (data.type === Hls.ErrorTypes.MEDIA_ERROR) {
          // A decoder-level failure can be transient (a level switch, a
          // source-buffer hiccup): try recovering in place; a repeated
          // failure surfaces as another fatal MEDIA_ERROR and each one
          // re-arms this same recovery.
          try {
            h.recoverMediaError();
            return;
          } catch {
            // Recovery itself failed — fall through to the full retry.
          }
        }
        // Network (or otherwise unrecoverable) failure: tear the instance
        // down and let the retry effect re-attempt after the pause.
        h.destroy();
        hls = null;
        setStatus("retrying");
      });
      h.loadSource(url);
      h.attachMedia(video);
    } else if (video.canPlayType("application/vnd.apple.mpegurl")) {
      // Native HLS (Safari): hand the playlist straight to the element.
      video.src = url;
      onLoadedMetadata = playOnce;
      video.addEventListener("loadedmetadata", onLoadedMetadata);
    } else {
      setStatus("unsupported");
    }

    return () => {
      disposed = true;
      video.removeEventListener("playing", onPlaying);
      if (onLoadedMetadata !== null) {
        video.removeEventListener("loadedmetadata", onLoadedMetadata);
      }
      hls?.destroy();
      // Reset the element itself so no stale native source survives a
      // retry or an unmount.
      video.removeAttribute("src");
      video.load();
    };
  }, [url, attempt]);

  // While in the retrying state, re-attempt after the pause.
  useEffect(() => {
    if (status !== "retrying") return;
    const timer = setTimeout(() => setAttempt((a) => a + 1), RETRY_PAUSE_MS);
    return () => clearTimeout(timer);
  }, [status]);

  return (
    <Box
      sx={{
        position: "relative",
        // A calm cap on wide screens; full-bleed within the layout gutters
        // on narrow ones — the same frame as WhepVideo.
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
        aria-label={t("play.videoLabel")}
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
      {(status === "retrying" || status === "unsupported") && (
        // The failure veil: playback is down. While retrying, the retry
        // loop keeps probing in the background and a successful attempt
        // lifts the veil by itself; while unsupported, the veil is
        // terminal. Neither carries controls of its own.
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
            {status === "retrying" ? t("play.retrying") : t("play.unsupported")}
          </Typography>
        </Box>
      )}
    </Box>
  );
}
