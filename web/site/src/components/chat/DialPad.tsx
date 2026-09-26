"use client";

// DialPad is the in-call DTMF keypad: the twelve keys on a FloatingCard
// (the video windows' shell — draggable, home-positioned), floated over
// the chat. A key press queues the digit on the call's DTMF sender (RFC
// 4733 telephone-event) through the dialer the parent computed
// (useCallMedia's dtmfFor): the pad renders only while the call
// negotiated the capability, and its visibility is the user's toggle —
// the composer's dial-pad button, or this card's close button.

import { Box, Button, IconButton } from "@mui/material";
import CloseIcon from "@mui/icons-material/Close";
import { useTranslation } from "react-i18next";
import { FloatingCard } from "./FloatingCard";

// keys is the pad's layout, row by row.
const keys = [
  ["1", "2", "3"],
  ["4", "5", "6"],
  ["7", "8", "9"],
  ["*", "0", "#"],
];

type DialPadProps = {
  // Reports a key press — the digit to queue on the call's DTMF sender.
  onDigit: (digit: string) => void;
  // The pad's home position (fixed positioning) before any drag.
  home: { top?: number; right?: number; bottom?: number; left?: number };
  // Hides the pad (the composer's toggle shows it again).
  onClose: () => void;
};

export function DialPad({ onDigit, home, onClose }: DialPadProps) {
  const { t } = useTranslation();
  return (
    <FloatingCard
      title={t("chat.dialPad.title")}
      home={home}
      width={220}
      actions={
        <IconButton
          size="small"
          onClick={onClose}
          aria-label={t("chat.dialPad.hide")}
        >
          <CloseIcon fontSize="small" />
        </IconButton>
      }
    >
      <Box
        sx={{
          display: "grid",
          gridTemplateColumns: "repeat(3, 1fr)",
          gap: 0.5,
          p: 1,
        }}
      >
        {keys.flat().map((key) => (
          <Button
            key={key}
            variant="text"
            onClick={() => onDigit(key)}
            aria-label={t("chat.dialPad.key", { key })}
            sx={{ minWidth: 0, fontSize: "1.15rem", fontWeight: 500 }}
          >
            {key}
          </Button>
        ))}
      </Box>
    </FloatingCard>
  );
}
