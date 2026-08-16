"use client";

import { createTheme } from "@mui/material/styles";

// Built-in MUI color-scheme support: declaring both schemes makes the theme
// emit CSS variables for light and dark. Enabling cssVariables with the
// "data" selector scopes them to <html data-mui-color-scheme="light|dark">
// (instead of a prefers-color-scheme media query), so the active scheme can
// be switched at runtime — system / light / dark — via
// useColorScheme().setMode() (see src/components/ColorModeToggle.tsx).
// Mode "system" resolves through the OS setting and tracks it live.
const theme = createTheme({
  cssVariables: {
    colorSchemeSelector: "data",
  },
  colorSchemes: {
    light: true,
    dark: true,
  },
  // Base corner radius (default 4px). Card and Paper both derive their
  // rounding from this token, so raising it rounds every surface at once.
  shape: {
    borderRadius: 12,
  },
  components: {
    MuiButton: {
      styleOverrides: {
        root: {
          borderRadius: "9999px",
        },
      },
    },
  },
});

export default theme;
