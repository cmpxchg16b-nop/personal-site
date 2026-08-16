"use client";

import { Box, IconButton, Tooltip } from "@mui/material";
import { useColorScheme } from "@mui/material/styles";
import ContrastIcon from "@mui/icons-material/Contrast";
import LightModeIcon from "@mui/icons-material/LightMode";
import DarkModeIcon from "@mui/icons-material/DarkMode";
import { useTranslation } from "react-i18next";

type Mode = "system" | "light" | "dark";

// Clicking the button advances to the next entry in this cycle.
const NEXT_MODE: Record<Mode, Mode> = {
  system: "light",
  light: "dark",
  dark: "system",
};

const MODE_ICON: Record<Mode, React.ReactNode> = {
  system: <ContrastIcon />,
  light: <LightModeIcon />,
  dark: <DarkModeIcon />,
};

const MODE_LABEL_KEY = {
  system: "colorMode.system",
  light: "colorMode.light",
  dark: "colorMode.dark",
} as const;

// Top-bar button that cycles the lightness preference through
// system → light → dark. The choice is persisted by MUI's color-scheme
// manager (localStorage key "mui-mode") and applied to <html> as
// data-mui-color-scheme (see src/theme.ts and app/layout.tsx).
export default function ColorModeToggle() {
  const { t } = useTranslation();
  const { mode, setMode } = useColorScheme();
  const modeLabel = mode ? t(MODE_LABEL_KEY[mode]) : "";

  // mode is undefined during SSR and the hydration render, so the icon is
  // swapped in only after mount; the placeholder keeps the same box size to
  // avoid a mismatch and any layout shift.
  return (
    <Tooltip title={mode ? t("colorMode.label", { mode: modeLabel }) : ""}>
      <IconButton
        aria-label={
          mode
            ? t("colorMode.switchWithCurrent", { mode: modeLabel })
            : t("colorMode.switch")
        }
        onClick={() => mode && setMode(NEXT_MODE[mode])}
      >
        {mode ? MODE_ICON[mode] : <Box sx={{ width: 24, height: 24 }} />}
      </IconButton>
    </Tooltip>
  );
}
