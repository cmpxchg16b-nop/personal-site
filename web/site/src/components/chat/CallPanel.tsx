"use client";

// CallPanel is the conversation's voice-call strip, rendered between the
// conversation header and the message list while a call with the peer is
// live: ringing state (with a cancel button for the caller; incoming
// calls are answered from the global popup, not here), and in-call state
// with the remote and local voice spectra, the call's duration, and a
// hang-up button.

import { useEffect, useRef, useState } from "react";
import { Box, Button, Typography, useTheme } from "@mui/material";
import CallEndIcon from "@mui/icons-material/CallEnd";
import RingVolumeIcon from "@mui/icons-material/RingVolume";
import { useTranslation } from "react-i18next";
import { AudioSpectrum } from "./AudioSpectrum";
import type { ActivePhoneCall } from "./types";

type CallPanelProps = {
  call: ActivePhoneCall;
  // The peer's display name (the spectrum labels and the ringing text).
  peerName: string;
  // FFT taps of the two voices (see useCallMedia); null until wired.
  localAnalyser: AnalyserNode | null;
  remoteAnalyser: AnalyserNode | null;
  // Hangs the call up — a cancel while ringing, an end once accepted.
  onEnd: () => void;
};

// formatDuration renders elapsed seconds as m:ss.
function formatDuration(seconds: number): string {
  const m = Math.floor(seconds / 60);
  const s = Math.floor(seconds % 60);
  return `${m}:${s.toString().padStart(2, "0")}`;
}

export function CallPanel({
  call,
  peerName,
  localAnalyser,
  remoteAnalyser,
  onEnd,
}: CallPanelProps) {
  const { t } = useTranslation();
  const theme = useTheme();
  // The call's own accept time: stamped when this panel first observes
  // the accepted status — the wire keeps no accept timestamp, and the
  // panel remounting on a conversation switch simply restarts the
  // display. elapsed ticks once a second while accepted.
  const acceptedAtRef = useRef<number | null>(null);
  const [elapsed, setElapsed] = useState(0);
  const accepted = call.status === "accepted";
  useEffect(() => {
    if (!accepted) return;
    acceptedAtRef.current ??= Date.now() / 1000;
    const timer = window.setInterval(
      () => setElapsed(Date.now() / 1000 - (acceptedAtRef.current ?? 0)),
      1000,
    );
    return () => window.clearInterval(timer);
  }, [accepted]);

  return (
    <Box
      sx={{
        px: 2,
        py: 1.25,
        borderBottom: 1,
        borderColor: "divider",
        flexShrink: 0,
        display: "flex",
        flexDirection: "column",
        gap: 1,
      }}
    >
      <Box sx={{ display: "flex", alignItems: "center", gap: 1 }}>
        {accepted ? (
          <>
            <Typography variant="body2" sx={{ fontWeight: 600 }}>
              {t("chat.call.inCall")}
            </Typography>
            <Typography variant="caption" color="text.secondary">
              {formatDuration(elapsed)}
            </Typography>
          </>
        ) : (
          <>
            <RingVolumeIcon
              fontSize="small"
              color="primary"
              sx={{
                "@keyframes ringPulse": {
                  "0%, 100%": { opacity: 1 },
                  "50%": { opacity: 0.35 },
                },
                animation: "ringPulse 1.2s ease-in-out infinite",
              }}
            />
            <Typography variant="body2" sx={{ fontWeight: 600 }}>
              {call.incoming
                ? t("chat.call.incoming")
                : t("chat.call.calling", { name: peerName })}
            </Typography>
            {call.incoming && (
              <Typography variant="caption" color="text.secondary">
                {t("chat.call.ringingHint")}
              </Typography>
            )}
          </>
        )}
        {/* The end button belongs to the call's owner actions: the
            caller can cancel a ringing or hang up; an incoming ringing
            call is answered from the global popup, not here. */}
        {!(call.incoming && !accepted) && (
          <Button
            size="small"
            color="error"
            variant="outlined"
            startIcon={<CallEndIcon fontSize="small" />}
            onClick={onEnd}
            sx={{ ml: "auto", flexShrink: 0 }}
          >
            {accepted ? t("chat.call.end") : t("chat.call.cancel")}
          </Button>
        )}
      </Box>
      {accepted && (
        <>
          {/* The two voices, remote over local: both pass the audio
              graph's analyser taps, so these charts show what is actually
              heard / sent. */}
          <Box>
            <Typography variant="caption" color="text.secondary">
              {peerName}
            </Typography>
            <AudioSpectrum
              analyser={remoteAnalyser}
              height={44}
              color={theme.palette.primary.main}
            />
          </Box>
          <Box>
            <Typography variant="caption" color="text.secondary">
              {t("chat.call.you")}
            </Typography>
            <AudioSpectrum
              analyser={localAnalyser}
              height={32}
              color={theme.palette.success.main}
            />
          </Box>
        </>
      )}
    </Box>
  );
}
