"use client";

import { useState } from "react";
import {
  IconButton,
  ListItemIcon,
  ListItemText,
  Menu,
  MenuItem,
  Tooltip,
} from "@mui/material";
import CheckIcon from "@mui/icons-material/Check";
import TranslateIcon from "@mui/icons-material/Translate";
import { useTranslation } from "react-i18next";
import i18n, {
  LANGUAGE_STORAGE_KEY,
  supportedLngs,
  type SupportedLng,
} from "@/i18n";

// Language names are shown as endonyms (each language in its own tongue), so
// they intentionally don't go through the translation files.
const LANGUAGE_NAMES: Record<SupportedLng, string> = {
  en: "English",
  zh: "中文",
};

// Top-bar control that switches the UI language via a small menu. The choice
// is persisted to localStorage (read back by I18nProvider on the next load)
// and applied immediately through i18n.changeLanguage, which re-renders every
// useTranslation subscriber.
export default function LanguageSwitcher() {
  const { t } = useTranslation();
  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null);
  const open = anchorEl !== null;

  const selectLanguage = (lng: SupportedLng) => {
    window.localStorage.setItem(LANGUAGE_STORAGE_KEY, lng);
    void i18n.changeLanguage(lng);
    setAnchorEl(null);
  };

  return (
    <>
      <Tooltip title={t("language.label")}>
        <IconButton
          aria-label={t("language.label")}
          aria-controls={open ? "language-menu" : undefined}
          aria-haspopup="true"
          aria-expanded={open ? "true" : undefined}
          onClick={(e) => setAnchorEl(e.currentTarget)}
        >
          <TranslateIcon />
        </IconButton>
      </Tooltip>
      <Menu
        id="language-menu"
        anchorEl={anchorEl}
        open={open}
        onClose={() => setAnchorEl(null)}
        anchorOrigin={{ vertical: "bottom", horizontal: "right" }}
        transformOrigin={{ vertical: "top", horizontal: "right" }}
        // Same layering rule as the profile menu: the top bar sits one step
        // above theme.zIndex.modal, so its menus must go one step further.
        sx={{ zIndex: (theme) => theme.zIndex.modal + 2 }}
      >
        {supportedLngs.map((lng) => (
          <MenuItem
            key={lng}
            selected={i18n.language === lng}
            onClick={() => selectLanguage(lng)}
          >
            {/* Reserve the icon slot for inactive entries too so the labels
                stay aligned. */}
            <ListItemIcon sx={{ minWidth: 32 }}>
              {i18n.language === lng ? <CheckIcon fontSize="small" /> : null}
            </ListItemIcon>
            <ListItemText>{LANGUAGE_NAMES[lng]}</ListItemText>
          </MenuItem>
        ))}
      </Menu>
    </>
  );
}
