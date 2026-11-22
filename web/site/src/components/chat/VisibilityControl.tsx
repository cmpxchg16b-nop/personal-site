"use client";

import { useState } from "react";
import {
  Box,
  FormControlLabel,
  IconButton,
  Popover,
  Switch,
  Tooltip,
  Typography,
} from "@mui/material";
import VisibilityIcon from "@mui/icons-material/Visibility";
import VisibilityOffIcon from "@mui/icons-material/VisibilityOff";
import { useTranslation } from "react-i18next";

type VisibilityControlProps = {
  visible: boolean;
  // Registration has not confirmed the requested state, or is not ready yet.
  disabled: boolean;
  onVisibleChange: (visible: boolean) => void;
};

// VisibilityControl is the chat page's public-presence switch. It lives in the
// TopBarActions slot alongside call audio; changing it re-registers the same
// subscriber instead of creating a new peer incarnation.
export function VisibilityControl({
  visible,
  disabled,
  onVisibleChange,
}: VisibilityControlProps) {
  const { t } = useTranslation();
  const [anchor, setAnchor] = useState<HTMLButtonElement | null>(null);
  const state = t(visible ? "chat.visibility.online" : "chat.visibility.invisible");

  return (
    <>
      <Tooltip title={t("chat.visibility.control", { state })}>
        <IconButton
          onClick={(e) => setAnchor((a) => (a === null ? e.currentTarget : null))}
          aria-label={t("chat.visibility.control", { state })}
          size="small"
        >
          {visible ? (
            <VisibilityIcon fontSize="small" />
          ) : (
            <VisibilityOffIcon fontSize="small" />
          )}
        </IconButton>
      </Tooltip>
      <Popover
        open={anchor !== null}
        onClose={() => setAnchor(null)}
        anchorEl={anchor}
        anchorOrigin={{ vertical: "bottom", horizontal: "right" }}
        transformOrigin={{ vertical: "top", horizontal: "right" }}
        sx={{ zIndex: (theme) => theme.zIndex.modal + 2 }}
      >
        <Box sx={{ p: 2, width: 280 }}>
          <Typography variant="subtitle2">
            {t("chat.visibility.title")}
          </Typography>
          <FormControlLabel
            sx={{ mt: 1, mx: 0, display: "flex", justifyContent: "space-between" }}
            label={state}
            labelPlacement="start"
            control={
              <Switch
                size="small"
                checked={visible}
                disabled={disabled}
                onChange={(_, next) => onVisibleChange(next)}
                slotProps={{
                  input: { "aria-label": t("chat.visibility.title") },
                }}
              />
            }
          />
          <Typography variant="caption" color="text.secondary">
            {t("chat.visibility.description")}
          </Typography>
        </Box>
      </Popover>
    </>
  );
}
