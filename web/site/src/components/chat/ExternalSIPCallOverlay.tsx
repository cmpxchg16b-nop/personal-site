"use client";

import { Box, Button, Paper, Stack, Typography } from "@mui/material";
import CallEndIcon from "@mui/icons-material/CallEnd";
import CallIcon from "@mui/icons-material/Call";
import VideocamIcon from "@mui/icons-material/Videocam";
import { useTranslation } from "react-i18next";
import {
  isExternalSIPCallActive,
  type ExternalSIPCall,
  type UseExternalSIPResult,
} from "@/api/ss/sip";
import { VideoWindow } from "./VideoWindow";

function callName(call: ExternalSIPCall): string {
  return call.remoteUri ?? call.target ?? call.callId;
}

/** External-SIP-only chrome; it deliberately does not reuse ConversationRefs. */
export function ExternalSIPCallOverlay({ sip }: { sip: UseExternalSIPResult }) {
  const { t } = useTranslation();
  const calls = Object.values(sip.calls).sort(
    (a, b) => b.updatedAt - a.updatedAt,
  );
  const incoming =
    calls.find(
      (call) => call.direction === "incoming" && call.state === "ringing",
    ) ?? null;
  const active = calls.filter(
    (call) => isExternalSIPCallActive(call) && call.callId !== incoming?.callId,
  );
  const videoCalls = calls.filter(
    (call) => call.state === "accepted" && call.kind === "video",
  );

  return (
    <>
      {incoming !== null && (
        <Paper
          elevation={8}
          role="alertdialog"
          aria-label={t("chat.externalSip.incomingTitle")}
          sx={{
            position: "fixed",
            top: 72,
            right: 24,
            zIndex: (theme) => theme.zIndex.modal,
            width: 300,
            maxWidth: "calc(100vw - 48px)",
            borderRadius: 2,
            p: 2,
          }}
        >
          <Stack spacing={1.5} sx={{ alignItems: "center" }}>
            {incoming.kind === "video" ? (
              <VideocamIcon color="primary" />
            ) : (
              <CallIcon color="primary" />
            )}
            <Typography variant="subtitle1" noWrap sx={{ maxWidth: "100%" }}>
              {callName(incoming)}
            </Typography>
            <Typography variant="body2" color="text.secondary">
              {t(
                incoming.kind === "video"
                  ? "chat.externalSip.incomingVideo"
                  : "chat.externalSip.incomingVoice",
              )}
            </Typography>
            <Stack direction="row" spacing={1}>
              <Button
                color="error"
                variant="outlined"
                startIcon={<CallEndIcon />}
                onClick={() => void sip.decline(incoming.callId)}
              >
                {t("chat.call.decline")}
              </Button>
              <Button
                color="success"
                variant="contained"
                startIcon={<CallIcon />}
                onClick={() => void sip.accept(incoming.callId)}
              >
                {t("chat.externalSip.accept")}
              </Button>
            </Stack>
          </Stack>
        </Paper>
      )}

      {active.length > 0 && (
        <Stack
          spacing={1}
          sx={{
            position: "fixed",
            left: 24,
            bottom: 24,
            zIndex: (theme) => theme.zIndex.modal,
            width: 320,
            maxWidth: "calc(100vw - 48px)",
          }}
        >
          {active.map((call) => (
            <Paper
              key={call.callId}
              elevation={8}
              sx={{ p: 1.5, borderRadius: 2 }}
            >
              <Box sx={{ display: "flex", alignItems: "center", gap: 1 }}>
                {call.kind === "video" ? (
                  <VideocamIcon fontSize="small" color="primary" />
                ) : (
                  <CallIcon fontSize="small" color="primary" />
                )}
                <Box sx={{ minWidth: 0, flexGrow: 1 }}>
                  <Typography variant="body2" noWrap sx={{ fontWeight: 600 }}>
                    {callName(call)}
                  </Typography>
                  <Typography variant="caption" color="text.secondary">
                    {t(`chat.externalSip.states.${call.state}`)}
                  </Typography>
                  {call.reason !== undefined && call.reason !== "" && (
                    <Typography
                      variant="caption"
                      color="error"
                      sx={{ display: "block" }}
                    >
                      {call.reason}
                    </Typography>
                  )}
                </Box>
                <Button
                  size="small"
                  color="error"
                  variant="outlined"
                  startIcon={<CallEndIcon fontSize="small" />}
                  onClick={() => void sip.end(call.callId)}
                >
                  {call.state === "dialing" || call.state === "ringing"
                    ? t("chat.call.cancel")
                    : t("chat.call.end")}
                </Button>
              </Box>
            </Paper>
          ))}
        </Stack>
      )}

      {videoCalls.flatMap((call, index) => {
        const media = sip.media[call.callId];
        const windows = [];
        if (
          media?.localStream !== null &&
          media?.localStream !== undefined &&
          media.localStream.getVideoTracks().length > 0
        ) {
          windows.push(
            <VideoWindow
              key={`${call.callId}:local`}
              title={t("chat.call.me")}
              stream={media.localStream}
              mirrored
              home={{ bottom: 60 + index * 24, right: 360 }}
              width={200}
            />,
          );
        }
        if (
          media?.remoteStream !== null &&
          media?.remoteStream !== undefined &&
          media.remoteStream.getVideoTracks().length > 0
        ) {
          windows.push(
            <VideoWindow
              key={`${call.callId}:remote`}
              title={callName(call)}
              stream={media.remoteStream}
              home={{ top: 120 + index * 24, right: 360 }}
              width={280}
            />,
          );
        }
        return windows;
      })}
    </>
  );
}
