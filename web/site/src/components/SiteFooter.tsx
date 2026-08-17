"use client";

import { Typography } from "@mui/material";
import { useTranslation } from "react-i18next";

// SiteFooter is the centered line at the bottom of the page: the copyright
// notice and a small colophon. Reading the current year during render is
// safe: I18nProvider gates the whole tree behind client-side mounting.
export default function SiteFooter() {
  const { t } = useTranslation();
  return (
    <Typography
      component="footer"
      variant="body2"
      color="text.secondary"
      align="center"
      sx={{ mt: 4, mb: 2 }}
    >
      {t("footer.copyright", {
        year: new Date().getFullYear(),
        name: t("hero.name"),
      })}
      {" · "}
      {t("footer.builtWith")}
    </Typography>
  );
}
