"use client";

// VolumeControl is the chat-scope call audio adjuster: a button opening
// a small popover with the mic send volume (what peers hear) and the
// speaker volume of incoming voices. The sliders drive the audio graph's
// gain nodes (see useCallVolumes), so they apply live during a call and
// pre-set the next one otherwise.

import { useState } from "react";
import { Box, IconButton, Popover, Slider, Typography } from "@mui/material";
import MicIcon from "@mui/icons-material/Mic";
import VolumeUpIcon from "@mui/icons-material/VolumeUp";
import { useTranslation } from "react-i18next";

type VolumeControlProps = {
  // The mic send volume, 0..1.
  localVolume: number;
  // The speaker volume of incoming voices, 0..1.
  remoteVolume: number;
  onLocalVolumeChange: (volume: number) => void;
  onRemoteVolumeChange: (volume: number) => void;
};

export function VolumeControl({
  localVolume,
  remoteVolume,
  onLocalVolumeChange,
  onRemoteVolumeChange,
}: VolumeControlProps) {
  const { t } = useTranslation();
  // The anchor as state rather than a ref: Popover reads it during
  // render, which refs are not for.
  const [anchor, setAnchor] = useState<HTMLButtonElement | null>(null);
  const open = anchor !== null;

  const sliderRow = (
    icon: React.ReactNode,
    label: string,
    value: number,
    onChange: (volume: number) => void,
  ) => (
    <Box>
      <Box sx={{ display: "flex", alignItems: "center", gap: 0.75 }}>
        {icon}
        <Typography variant="caption" color="text.secondary">
          {label}
        </Typography>
        <Typography
          variant="caption"
          color="text.secondary"
          sx={{ ml: "auto" }}
        >
          {Math.round(value * 100)}%
        </Typography>
      </Box>
      <Slider
        size="small"
        min={0}
        max={1}
        step={0.01}
        value={value}
        onChange={(_, v) => onChange(v as number)}
        aria-label={label}
      />
    </Box>
  );

  return (
    <>
      <IconButton
        onClick={(e) => setAnchor((a) => (a === null ? e.currentTarget : null))}
        aria-label={t("chat.volume.title")}
        size="small"
      >
        <VolumeUpIcon fontSize="small" />
      </IconButton>
      <Popover
        open={open}
        onClose={() => setAnchor(null)}
        anchorEl={anchor}
        anchorOrigin={{ vertical: "bottom", horizontal: "right" }}
        transformOrigin={{ vertical: "top", horizontal: "right" }}
      >
        <Box
          sx={{
            p: 2,
            width: 220,
            display: "flex",
            flexDirection: "column",
            gap: 1.5,
          }}
        >
          <Typography variant="subtitle2">{t("chat.volume.title")}</Typography>
          {sliderRow(
            <MicIcon fontSize="small" color="action" />,
            t("chat.volume.microphone"),
            localVolume,
            onLocalVolumeChange,
          )}
          {sliderRow(
            <VolumeUpIcon fontSize="small" color="action" />,
            t("chat.volume.speaker"),
            remoteVolume,
            onRemoteVolumeChange,
          )}
        </Box>
      </Popover>
    </>
  );
}
