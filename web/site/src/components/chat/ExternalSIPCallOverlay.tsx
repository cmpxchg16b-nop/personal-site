"use client";

import { useTranslation } from "react-i18next";
import type { ExternalSIPCall, UseExternalSIPResult } from "@/api/ss/sip";
import { VideoWindow } from "./VideoWindow";

function callName(call: ExternalSIPCall): string {
  return call.remoteUri ?? call.target ?? call.callId;
}

/** Floating video streams for accepted external SIP calls. */
export function ExternalSIPCallOverlay({ sip }: { sip: UseExternalSIPResult }) {
  const { t } = useTranslation();
  const videoCalls = Object.values(sip.calls)
    .filter((call) => call.state === "accepted" && call.kind === "video")
    .sort((a, b) => b.updatedAt - a.updatedAt);

  return videoCalls.flatMap((call, index) => {
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
  });
}
