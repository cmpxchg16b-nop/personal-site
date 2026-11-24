"use client";

import { useTranslation } from "react-i18next";
import type { ExternalSIPCall, UseExternalSIPResult } from "@/api/ss/sip";
import type { ChatUser } from "@/api/ss/types";
import type { SxProps, Theme } from "@mui/material";
import ConversationView from "./ConversationView";
import {
  externalSIPConversationKey,
  externalSIPConversationMessages,
  type ExternalSIPConversationRef,
} from "./externalSipConversation";

type ExternalSIPConversationViewProps = {
  conversation: ExternalSIPConversationRef;
  call: ExternalSIPCall | null;
  sip: UseExternalSIPResult;
  localAnalyser: AnalyserNode | null;
  remoteAnalyser: AnalyserNode | null;
  dialPad?: {
    open: boolean;
    onToggle: () => void;
  };
  onBack: () => void;
  sx?: SxProps<Theme>;
};

// This is only a data adapter. External SIP deliberately renders the exact
// same ConversationView as an ordinary DM, not a second chat UI.
export default function ExternalSIPConversationView({
  conversation,
  call,
  sip,
  localAnalyser,
  remoteAnalyser,
  dialPad,
  onBack,
  sx,
}: ExternalSIPConversationViewProps) {
  const { t } = useTranslation();
  const selfId = `external:self:${conversation.credentialId}`;
  const peerId = `external:peer:${externalSIPConversationKey(conversation)}`;
  const localUri =
    call?.localUri ??
    sip.messages.find(
      (message) =>
        message.credentialId === conversation.credentialId &&
        message.localUri !== undefined,
    )?.localUri;
  const peer: ChatUser = {
    id: peerId,
    name: conversation.target,
    online: true,
    visible: true,
  };
  const self: ChatUser = {
    id: selfId,
    name: localUri ?? t("chat.call.you"),
    online: true,
    visible: true,
  };
  const startCall = (kind: "voice" | "video") => {
    void sip.dial({
      credentialId: conversation.credentialId,
      target: conversation.target,
      kind,
    });
  };

  return (
    <ConversationView
      conversation={{ kind: "dm", channelId: "external", user: peer }}
      messages={externalSIPConversationMessages(
        sip,
        conversation,
        selfId,
        peerId,
      )}
      usersById={{ [selfId]: self, [peerId]: peer }}
      currentUserId={selfId}
      onSend={(text) => {
        void sip.sendMessage({
          credentialId: conversation.credentialId,
          target: conversation.target,
          text,
        });
      }}
      onRequestFile={() => {}}
      getFileByFileId={() => undefined}
      call={call}
      connectionState={null}
      showPresence={false}
      onStartCall={() => startCall("voice")}
      onStartVideoCall={() => startCall("video")}
      onEndCall={() => {
        if (call !== null) void sip.end(call.callId);
      }}
      localAnalyser={localAnalyser}
      remoteAnalyser={remoteAnalyser}
      dialPad={dialPad}
      onBack={onBack}
      sx={sx}
    />
  );
}
