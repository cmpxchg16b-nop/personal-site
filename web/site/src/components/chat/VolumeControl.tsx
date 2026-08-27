"use client";

// VolumeControl is the call audio adjuster: a button opening a small
// popover with the mic send volume (what peers hear), the speaker volume
// of incoming voices, and the echo-cancellation toggle (the capture-side
// voice processing). They drive the audio graph (see useCallVolumes and
// useEchoCancellation). The chat page portals it into the TopBar (see
// TopBarActions) and only while a call session is live — with no ongoing
// call there is nothing to adjust.

import { useState } from "react";
import {
  Box,
  IconButton,
  Popover,
  Slider,
  Switch,
  Typography,
} from "@mui/material";
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
  // The capture-side voice processing switch (echo cancellation & co.).
  echoCancellation: boolean;
  onEchoCancellationChange: (on: boolean) => void;
};

export function VolumeControl({
  localVolume,
  remoteVolume,
  onLocalVolumeChange,
  onRemoteVolumeChange,
  echoCancellation,
  onEchoCancellationChange,
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
        // The TopBar (the button's home) sits at modal + 1, so the
        // popover floats one step higher to not slide under the sticky
        // bar it is anchored to.
        sx={{ zIndex: (theme) => theme.zIndex.modal + 2 }}
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
          <Box sx={{ display: "flex", alignItems: "center", gap: 0.75 }}>
            <Typography variant="caption" color="text.secondary">
              {t("chat.volume.echoCancellation")}
            </Typography>
            <Switch
              size="small"
              checked={echoCancellation}
              onChange={(_, on) => onEchoCancellationChange(on)}
              slotProps={{
                input: { "aria-label": t("chat.volume.echoCancellation") },
              }}
              sx={{ ml: "auto" }}
            />
          </Box>
        </Box>
      </Popover>
    </>
  );
}
